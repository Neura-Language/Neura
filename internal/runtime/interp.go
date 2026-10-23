// Package runtime 是 Neura v0.3.7 的最小运行时（Phase 4）。
//
// 已实现：内存版 LogicDB（按 logic / 名字 / 字段倒排索引）、match 求值顺序
// （strict → 相似度 → guard → 确定性排序）、reuse / invoke / emit / learn、
// MatchTrace（JSONL）、place 仅 local。
//
// 打 stub（§14 明确不做）：embedding 相似度、custom similarity、自动 delta、
// 跨节点 exactly_once；place 的 at 非 local 也只打 stub warning。
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"neura/internal/ast"
	"neura/internal/cluster"
	"neura/internal/event"
	"neura/internal/place"
	"neura/internal/state"
)

// Program 是加载后的可执行程序。
type Program struct {
	file *ast.File
	DB   *LogicDB

	fns       map[string]*ast.FnDecl
	logics    map[string]*ast.LogicDecl
	instances map[string]*ast.InstanceDecl

	placed map[string]string

	// Phase 5：静态节点表 + 远端调度器（nil 表示全本地）。
	Table   *cluster.Table
	Cluster *cluster.Dispatcher

	mu       sync.Mutex // 保护 outputs/learns/warnings/trace/seq（节点并发处理请求）
	outputs  []string
	learns   []string
	warnings []string
	trace    []TraceRecord
	seq      int

	sim          SimilarityProvider // nil → Jaccard
	shared       *state.Coordinator // 非 nil 表示跨节点 exactly-once 已启用
	placeAdapter place.Adapter      // 非 nil 时 place...where 走 Adapter 求解
	placements   map[string]Placement
	reqSeq       int64
	nodeTag      string
	selfAddr     string
	localSeq     int
}

// Result 是一次运行的产物。TraceID 是全局 trace 串联键。
type Result struct {
	TraceID  string
	Returned Value
	Outputs  []string
	Learns   []string
	Warnings []string
	Trace    []TraceRecord
}

// NewProgram 收集声明、装载实例、应用 place（含 stub 警告）。
func NewProgram(f *ast.File) *Program { return newProgram(f, nil) }

// NewProgramWithPlacement 在装载阶段就使用 Place Adapter 求解 where 约束。
func NewProgramWithPlacement(f *ast.File, a place.Adapter) *Program { return newProgram(f, a) }

func newProgram(f *ast.File, adapter place.Adapter) *Program {
	p := &Program{
		file:         f,
		DB:           NewLogicDB(),
		fns:          map[string]*ast.FnDecl{},
		logics:       map[string]*ast.LogicDecl{},
		instances:    map[string]*ast.InstanceDecl{},
		placed:       map[string]string{},
		placements:   map[string]Placement{},
		Table:        cluster.NewTable(),
		nodeTag:      fmt.Sprintf("n%d", time.Now().UnixNano()%1e9),
		placeAdapter: adapter,
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FnDecl:
			p.fns[x.Name] = x
		case *ast.LogicDecl:
			p.logics[x.Name] = x
		case *ast.InstanceDecl:
			p.instances[x.Name] = x
		}
	}

	// 实例字段是常量表达式，用空 env 求值；顺序 = 声明顺序（确定性）。
	ev := &interp{p: p, global: newEnv(nil)}
	for _, d := range f.Decls {
		in, ok := d.(*ast.InstanceDecl)
		if !ok {
			continue
		}
		fields := map[string]Value{}
		for _, b := range in.Bindings {
			fields[b.Name] = ev.eval(b.Value, ev.global)
		}
		p.DB.Add(&Instance{Name: in.Name, Logic: in.Logic, Fields: fields})
	}

	for _, d := range f.Decls {
		if pd, ok := d.(*ast.PlaceDecl); ok {
			p.applyPlace(pd)
		}
	}
	return p
}

// applyPlace：local 直接落本机；其余优先交给 Place Adapter（where 约束），
// 没有 Adapter 时退回静态节点表（At 列表顺序 = failover 顺序）。
func (p *Program) applyPlace(pd *ast.PlaceDecl) {
	if pd.AtSpec != nil && pd.AtSpec.Local {
		p.placed[pd.Name] = "local"
		p.placements[pd.Name] = Placement{Target: pd.Name, Adapter: "local"}
		return
	}
	if p.placeAdapter != nil && p.applyAdapter(pd) {
		return
	}
	nodes := "[]"
	if pd.AtSpec != nil {
		nodes = "[" + strings.Join(pd.AtSpec.Nodes, ", ") + "]"
	}
	p.placed[pd.Name] = "cluster"
	var chosen []string
	if pd.AtSpec != nil && len(pd.AtSpec.Nodes) > 0 {
		p.Table.Set(pd.Name, pd.AtSpec.Nodes)
		chosen = append([]string(nil), pd.AtSpec.Nodes...)
	}
	p.placements[pd.Name] = Placement{Target: pd.Name, Nodes: chosen, Adapter: "static"}
	p.warnf("place %s at %s: cluster（静态节点表已登记，failover 顺序 = 声明顺序）", pd.Name, nodes)
}

func (p *Program) warnf(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warnings = append(p.warnings, fmt.Sprintf(format, a...))
}

func (p *Program) addOutput(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outputs = append(p.outputs, s)
}

func (p *Program) addLearn(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.learns = append(p.learns, s)
}

// nextReqID 生成幂等键（同一次远程调用重试时复用同一个 key）。
func (p *Program) nextReqID(instance, method string) string {
	n := atomic.AddInt64(&p.reqSeq, 1)
	return fmt.Sprintf("%s:%s.%s#%d", p.nodeTag, instance, method, n)
}

// SetNodeAddr 设置本节点在 trace 里的标识（serve 时用实际监听地址）。
func (p *Program) SetNodeAddr(addr string) {
	p.selfAddr = addr
}

// EnableCluster 启用跨节点调度（self 为空表示不认领任何本地放置）。
func (p *Program) EnableCluster(self string, timeout time.Duration, logf func(string, ...any)) {
	p.selfAddr = self
	p.Cluster = &cluster.Dispatcher{Self: self, Timeout: timeout, Table: p.Table, Log: logf}
}

// EnableClusterS2S 同 EnableCluster，但跨节点调用走 S2S 加密：
// 线下 Root Key 握手 → HKDF-SHA256 SessionKey → AES-256-GCM 业务帧。
func (p *Program) EnableClusterS2S(self string, timeout time.Duration, root *cluster.RootKey, logf func(string, ...any)) {
	p.EnableCluster(self, timeout, logf)
	if p.Cluster != nil {
		p.Cluster.RootKey = root
	}
}

// interp.addTrace 打上 trace_id / node / depth / local 序号，再进全局序列。
func (in *interp) addTrace(r TraceRecord) {
	r.TraceID = in.traceID
	r.Node = in.p.selfAddr
	if r.Node == "" {
		r.Node = in.p.nodeTag
	}
	r.Depth = in.depth
	p := in.p
	p.mu.Lock()
	p.localSeq++
	r.Local = p.localSeq
	p.seq++
	r.Seq = p.seq
	p.trace = append(p.trace, r)
	p.mu.Unlock()
}

// appendRemoteTrace 把远端回传的 trace 片段按序并入本地 trace（重排 seq）。
func (p *Program) appendRemoteTrace(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var recs []TraceRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		p.warnf("trace 聚合失败: %v", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range recs {
		p.seq++
		recs[i].Seq = p.seq
		p.trace = append(p.trace, recs[i])
	}
}

func (p *Program) newTraceID() string {
	n := atomic.AddInt64(&p.reqSeq, 1)
	return fmt.Sprintf("%s-t%d", p.nodeTag, n)
}

// Placements 返回 place 结果（名字 → local / local(stub)）。
func (p *Program) Placements() map[string]string {
	out := map[string]string{}
	for k, v := range p.placed {
		out[k] = v
	}
	return out
}

// FuncNames 返回可用入口名（升序）。
func (p *Program) FuncNames() []string {
	out := make([]string, 0, len(p.fns))
	for k := range p.fns {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run 调用入口函数。args 不足时按参数类型合成（logic 类型 → 空字段实例）。
func (p *Program) Run(entry string, args []Value) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeError); ok {
				err = re
				return
			}
			panic(r)
		}
	}()
	if entry == "" {
		entry = "main"
	}
	fd, ok := p.fns[entry]
	if !ok {
		return Result{}, fmt.Errorf("入口函数 %q 不存在；可用: %s", entry, strings.Join(p.FuncNames(), ", "))
	}
	in := &interp{p: p, global: newEnv(nil), traceID: p.newTraceID()}
	ret := in.callFn(fd, args)
	return p.snapshot(ret, in.traceID), nil
}

// RunHandler 直接执行 neuron 的 handler（Phase 4 没有事件循环，emit 靠它可达）。
//
// target 形式：
//   - "Neuron"           → 有 on signal 时执行第一个 signal handler，否则执行 legacy handle
//   - "Neuron.amount"    → 精确执行 on signal amount
func (p *Program) RunHandler(target string, args []Value) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeError); ok {
				err = re
				return
			}
			panic(r)
		}
	}()

	neuronName, signalName := target, ""
	if i := strings.IndexByte(target, '.'); i >= 0 {
		neuronName, signalName = target[:i], target[i+1:]
	}

	var nd *ast.NeuronDecl
	for _, d := range p.file.Decls {
		if n, ok := d.(*ast.NeuronDecl); ok && n.Name == neuronName {
			nd = n
		}
	}
	if nd == nil {
		return Result{}, fmt.Errorf("neuron %q 不存在", neuronName)
	}

	var (
		body    *ast.Block
		sigName string
		sigType ast.Type
	)
	switch {
	case signalName != "":
		for _, sh := range nd.Signals {
			if sh.Name == signalName {
				body, sigName, sigType = sh.Body, sh.Name, sh.Type
				break
			}
		}
		if body == nil {
			return Result{}, fmt.Errorf("neuron %q 没有 on signal %q", neuronName, signalName)
		}
	case len(nd.Signals) > 0:
		sh := nd.Signals[0]
		body, sigName, sigType = sh.Body, sh.Name, sh.Type
	case nd.Handler != nil:
		body = nd.Handler
	default:
		return Result{}, fmt.Errorf("neuron %q 没有 handler", neuronName)
	}

	in := &interp{p: p, global: newEnv(nil), traceID: p.newTraceID()}
	e := newEnv(nil)
	for i, port := range nd.Ports {
		if i < len(args) {
			e.define(port.Name, args[i])
			continue
		}
		e.define(port.Name, in.synthesize(port.Type))
	}
	if sigName != "" {
		// signal 值：优先取端口之后的实参，否则按声明类型给零值
		if len(args) > len(nd.Ports) {
			e.define(sigName, args[len(nd.Ports)])
		} else {
			e.define(sigName, in.zeroOf(sigType))
		}
	}
	in.execFnBody(body, e)
	return p.snapshot(Unit(), in.traceID), nil
}

func (p *Program) snapshot(ret Value, traceID string) Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Result{
		TraceID:  traceID,
		Returned: ret,
		Outputs:  append([]string(nil), p.outputs...),
		Learns:   append([]string(nil), p.learns...),
		Warnings: append([]string(nil), p.warnings...),
		Trace:    append([]TraceRecord(nil), p.trace...),
	}
}

// zeroOf 给 signal 形参一个类型正确的零值。
func (in *interp) zeroOf(t ast.Type) Value {
	nt, ok := t.(*ast.NamedType)
	if !ok {
		return Unit()
	}
	switch nt.Name {
	case "Int", "Int32", "Int64":
		return IntV(0)
	case "Float", "F32", "F64":
		return FloatV(0)
	case "Bool":
		return BoolV(false)
	case "String":
		return StringV("")
	case "Size":
		return sizeValue("0B")
	case "Duration":
		return durationValue("0ms")
	}
	if _, isLogic := in.p.logics[nt.Name]; isLogic {
		return InstV(&Instance{Name: "<subject>", Logic: nt.Name, Fields: map[string]Value{}})
	}
	return Unit()
}

// WriteTrace 把 MatchTrace 以 JSONL 写出（字段固定、无时间戳 → 可复现）。
func (p *Program) WriteTrace(w io.Writer) error {
	enc := json.NewEncoder(w)
	for _, r := range p.trace {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ---- §14 明确不做的功能：统一 stub ----

func (p *Program) Embed(text string) []float64 {
	if ep, ok := p.sim.(*EmbeddingProvider); ok && ep.Probe != nil {
		vec, err := ep.embed(context.Background(), text)
		if err != nil {
			p.warnf("embedding 探针失败（%v）", err)
			return nil
		}
		out := make([]float64, len(vec))
		for i, f := range vec {
			out[i] = float64(f)
		}
		return out
	}
	p.warnf("stub: embedding 相似度未实现（%q）", text)
	return nil
}

func (p *Program) CustomSimilarity(a, b map[string]Value) float64 {
	if p.sim != nil {
		return p.sim.Score(a, b)
	}
	p.warnf("stub: custom similarity 未实现，回退到纯函数 Similarity")
	return Similarity(a, b)
}

func (p *Program) AutoDelta() bool {
	p.warnf("stub: 自动 delta 未实现")
	return false
}

func (p *Program) CrossNodeExactlyOnce() bool {
	if p.shared != nil {
		return true
	}
	p.warnf("stub: 跨节点共享幂等未实现（节点本地幂等表已启用，见 cluster.Idempotency）")
	return false
}

// ---------------- 解释器 ----------------

type env struct {
	vars   map[string]Value
	parent *env
}

func newEnv(parent *env) *env {
	return &env{vars: map[string]Value{}, parent: parent}
}

func (e *env) get(name string) (Value, bool) {
	for cur := e; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return Value{}, false
}

func (e *env) define(name string, v Value) { e.vars[name] = v }

func (e *env) set(name string, v Value) bool {
	for cur := e; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			cur.vars[name] = v
			return true
		}
	}
	return false
}

// runtimeError 是运行期硬错误（如实参个数不符）。
type runtimeError struct{ msg string }

func (e runtimeError) Error() string { return e.msg }

type returnSignal struct{ v Value }
type breakSignal struct{}
type continueSignal struct{}

type interp struct {
	p        *Program
	global   *env
	noRemote bool   // 节点内部执行时禁止再次远端转发，避免回路
	traceID  string // 全局 trace 串联键
	depth    int    // 跨节点调用深度

	// 事件循环绑定：非 nil 时 emit 写入 outbox，由 actor 在 handler 返回后下刷。
	out      *event.Tx
	outPorts []*ast.Port
}

func (in *interp) callFn(fd *ast.FnDecl, args []Value) Value {
	// 实参个数必须精确匹配：多传/少传都不能静默忽略
	if len(args) > len(fd.Params) {
		panic(runtimeError{fmt.Sprintf("%s 需要 %d 个参数，实际传入 %d 个",
			fd.Name, len(fd.Params), len(args))})
	}
	e := newEnv(in.global)
	for i, p := range fd.Params {
		if i < len(args) {
			e.define(p.Name, args[i])
			continue
		}
		e.define(p.Name, in.synthesize(p.Type))
	}
	return in.execFnBody(fd.Body, e)
}

// synthesize：缺省实参（logic/instance 类型 → 空字段实例）。
func (in *interp) synthesize(t ast.Type) Value {
	if nt, ok := t.(*ast.NamedType); ok {
		if _, isLogic := in.p.logics[nt.Name]; isLogic {
			return InstV(&Instance{Name: "<subject>", Logic: nt.Name, Fields: map[string]Value{}})
		}
	}
	return Unit()
}

func (in *interp) execFnBody(b *ast.Block, e *env) (result Value) {
	defer func() {
		if r := recover(); r != nil {
			if rs, ok := r.(returnSignal); ok {
				result = rs.v
				return
			}
			panic(r)
		}
	}()
	// Neura 用尾表达式作返回值
	return in.execBlock(b, e)
}

func (in *interp) execBlock(b *ast.Block, e *env) Value {
	last := Unit()
	if b == nil {
		return last
	}
	for _, s := range b.Stmts {
		last = in.execStmt(s, e)
	}
	return last
}

func (in *interp) execStmt(s ast.Stmt, e *env) Value {
	switch x := s.(type) {
	case *ast.Block:
		return in.execBlock(x, newEnv(e))

	case *ast.LetStmt:
		v := in.eval(x.Value, e)
		e.define(x.Name, v)
		return v

	case *ast.AssignStmt:
		v := in.eval(x.Value, e)
		in.assign(x.Target, v, e)
		return v

	case *ast.ExprStmt:
		return in.eval(x.X, e)

	case *ast.DoStmt:
		return in.execBlock(x.Body, newEnv(e))

	case *ast.ForStmt:
		lo, hi, inclusive := in.rangeBounds(x.Iter, e)
		for i := lo; ; i++ {
			if inclusive {
				if i > hi {
					break
				}
			} else if i >= hi {
				break
			}
			broke := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						switch r.(type) {
						case breakSignal:
							broke = true
						case continueSignal:
						default:
							panic(r)
						}
					}
				}()
				ee := newEnv(e)
				ee.define(x.Var, IntV(i))
				in.execBlock(x.Body, ee)
			}()
			if broke {
				break
			}
		}
		return Unit()

	case *ast.IfStmt:
		cond := in.eval(x.Cond, e)
		if cond.Kind == KBool && cond.B {
			return in.execBlock(x.Then, newEnv(e))
		}
		if x.Else != nil {
			return in.execStmt(x.Else, e)
		}
		return Unit()

	case *ast.MatchStmt:
		return in.evalMatch(x, e)

	case *ast.ReturnStmt:
		v := Unit()
		if x.Value != nil {
			v = in.eval(x.Value, e)
		}
		panic(returnSignal{v: v})

	case *ast.BreakStmt:
		panic(breakSignal{})

	case *ast.ContinueStmt:
		panic(continueSignal{})

	case *ast.EmitStmt:
		if v, ok := in.emitExplicit(x, e); ok {
			return v
		}
		v := in.eval(x.X, e)
		in.p.addOutput("emit " + v.String())
		if in.out != nil {
			in.emitDefault(x.X, v)
		}
		return v

	case *ast.InvokeStmt:
		return in.eval(x.X, e)

	case *ast.ReuseStmt:
		v := in.eval(x.X, e)
		in.p.addOutput("reuse " + v.String())
		return v

	case *ast.LearnStmt:
		v := in.eval(x.X, e)
		in.p.addLearn("learn " + v.String())
		return Unit() // learn 是日志副作用语句，不产生值
	}
	return Unit()
}

func (in *interp) assign(target ast.Expr, v Value, e *env) {
	switch t := target.(type) {
	case *ast.Ident:
		if !e.set(t.Name, v) {
			e.define(t.Name, v)
		}
	case *ast.QualifiedRef:
		if len(t.Parts) == 2 {
			if base, ok := e.get(t.Parts[0]); ok && base.Kind == KInstance && base.Inst != nil {
				base.Inst.Fields[t.Parts[1]] = v
				return
			}
		}
	case *ast.FieldExpr:
		if base := in.eval(t.X, e); base.Kind == KInstance && base.Inst != nil {
			base.Inst.Fields[t.Name] = v
		}
	}
}

func (in *interp) rangeBounds(x ast.Expr, e *env) (int64, int64, bool) {
	re, ok := x.(*ast.RangeExpr)
	if !ok {
		in.p.warnf("for 的迭代值不是 Range，循环体不执行")
		return 0, -1, false
	}
	lo := int64(in.eval(re.Lo, e).AsFloat())
	hi := int64(in.eval(re.Hi, e).AsFloat())
	return lo, hi, re.Inclusive
}

// ---------------- match（strict → 相似 → guard → 排序） ----------------

func (in *interp) evalMatch(m *ast.MatchStmt, e *env) Value {
	subj := in.eval(m.Subject, e)
	if subj.Kind != KInstance || subj.Inst == nil {
		in.p.warnf("match 的 subject 不是 instance 值（%s），跳过", subj.String())
		in.addTrace(TraceRecord{
			Subject: subj.String(),
			Logic:   "-",
			Arm:     "-",
			Reason:  "subject-not-instance",
		})
		return Unit()
	}
	s := subj.Inst

	for _, arm := range m.Arms {
		pat := strings.Join(arm.Pattern.Parts, ".")
		rec := TraceRecord{Subject: s.Name, Logic: s.Logic, Arm: pat}

		var all []Candidate
		for _, c := range in.p.DB.InstancesOf(s.Logic) {
			if c.Logic != pat && c.Name != pat {
				continue
			}
			pass, _ := in.guardOK(c)
			all = append(all, Candidate{
				Instance: c.Name,
				Logic:    c.Logic,
				Strict:   StrictEqual(s.Fields, c.Fields),
				Score:    in.p.score(s.Fields, c.Fields),
				Guard:    pass,
				Provider: in.p.providerTag(),
			})
		}
		sortCandidates(all) // trace 里也保持 strict 优先、分数降序、名字升序
		rec.Candidates = all
		if len(all) == 0 {
			rec.Reason = "no-candidate"
			in.addTrace(rec)
			continue
		}

		// guard 过滤在排序之前（"strict 先 → 相似 → guard → 排序"）
		passing := make([]Candidate, 0, len(all))
		for _, c := range all {
			if c.Guard {
				passing = append(passing, c)
			}
		}
		if len(passing) == 0 {
			rec.Reason = "guard-rejected"
			in.addTrace(rec)
			continue
		}
		sortCandidates(passing)

		chosen, ok := in.p.DB.Lookup(passing[0].Instance)
		if !ok {
			rec.Reason = "no-candidate"
			in.addTrace(rec)
			continue
		}
		rec.Chosen = chosen.Name
		if passing[0].Strict {
			rec.Reason = "strict"
		} else {
			rec.Reason = "similarity"
		}
		rec.Bind = arm.Bind
		in.addTrace(rec)

		ee := newEnv(e)
		if arm.Bind != "" {
			ee.define(arm.Bind, InstV(chosen))
		}
		return in.execStmt(arm.Body, ee)
	}

	// 没有命中任何 arm：跳过（§17 N6：无 instance 时不匹配、跳过）
	return Unit()
}

// guardOK 求值 logic 的 guard；无 guard 恒为 true。
func (in *interp) guardOK(inst *Instance) (bool, bool) {
	lg := in.p.logics[inst.Logic]
	if lg == nil || lg.Guard == nil {
		return true, false
	}
	e := newEnv(in.global)
	for k, v := range inst.Fields {
		e.define(k, v)
	}
	v := in.eval(lg.Guard, e)
	if v.Kind != KBool {
		return false, true
	}
	return v.B, true
}

// ---------------- 表达式求值 ----------------

func (in *interp) eval(x ast.Expr, e *env) Value {
	switch n := x.(type) {
	case nil:
		return Unit()

	case *ast.Literal:
		return in.literal(n)

	case *ast.UnitExpr:
		base := in.eval(n.Value, e)
		// unit_lit 在 expr 位置属静态错误（规则 20）；运行时仅保留数值部分
		return base

	case *ast.Ident:
		if v, ok := e.get(n.Name); ok {
			return v
		}
		if n.Name == "gpu_mem" || n.Name == "cpu_mem" || n.Name == "gpu_util" {
			return IntV(0)
		}
		in.p.warnf("运行时未知标识符 %s", n.Name)
		return Unit()

	case *ast.QualifiedRef:
		base, ok := e.get(n.Parts[0])
		if !ok {
			in.p.warnf("运行时未知标识符 %s", n.Parts[0])
			return Unit()
		}
		for _, f := range n.Parts[1:] {
			if base.Kind != KInstance || base.Inst == nil {
				return Unit()
			}
			v, ok := base.Inst.Field(f)
			if !ok {
				return Unit()
			}
			base = v
		}
		return base

	case *ast.FieldExpr:
		base := in.eval(n.X, e)
		if base.Kind == KInstance && base.Inst != nil {
			if v, ok := base.Inst.Field(n.Name); ok {
				return v
			}
		}
		return Unit()

	case *ast.ParenExpr:
		return in.eval(n.X, e)

	case *ast.UnaryExpr:
		v := in.eval(n.X, e)
		if n.Op == "-" {
			switch v.Kind {
			case KInt:
				return IntV(-v.I)
			case KFloat:
				return FloatV(-v.F)
			case KSize:
				return Value{Kind: KSize, F: -v.F, S: "-" + v.S}
			case KDuration:
				return Value{Kind: KDuration, F: -v.F, S: "-" + v.S}
			}
			return v
		}
		if v.Kind == KBool {
			return BoolV(!v.B)
		}
		return BoolV(false)

	case *ast.BinaryExpr:
		return in.binary(n, e)

	case *ast.CallExpr:
		return in.call(n, e)

	case *ast.RangeExpr:
		in.p.warnf("Range 值不能作为普通表达式使用")
		return Unit()
	}
	return Unit()
}

func (in *interp) literal(l *ast.Literal) Value {
	switch l.Kind {
	case "int":
		if v, err := strconv.ParseInt(l.Text, 10, 64); err == nil {
			return IntV(v)
		}
		return IntV(0)
	case "float":
		if v, err := strconv.ParseFloat(l.Text, 64); err == nil {
			return FloatV(v)
		}
		return FloatV(0)
	case "string":
		return StringV(unquoteString(l.Text))
	case "size":
		return sizeValue(l.Text)
	case "duration":
		return durationValue(l.Text)
	case "bool":
		return BoolV(l.Text == "true")
	}
	return Unit()
}

func (in *interp) binary(n *ast.BinaryExpr, e *env) Value {
	switch n.Op {
	case "&&":
		l := in.eval(n.L, e)
		if l.Kind == KBool && !l.B {
			return BoolV(false)
		}
		r := in.eval(n.R, e)
		return BoolV(l.Kind == KBool && l.B && r.Kind == KBool && r.B)
	case "||":
		l := in.eval(n.L, e)
		if l.Kind == KBool && l.B {
			return BoolV(true)
		}
		r := in.eval(n.R, e)
		return BoolV(l.Kind == KBool && l.B || r.Kind == KBool && r.B)
	}

	l := in.eval(n.L, e)
	r := in.eval(n.R, e)

	switch n.Op {
	case "+", "-", "*", "/", "%":
		if l.Kind == KString && n.Op == "+" {
			return StringV(l.S + r.String())
		}
		if l.Kind == KInt && r.Kind == KInt {
			switch n.Op {
			case "+":
				return IntV(l.I + r.I)
			case "-":
				return IntV(l.I - r.I)
			case "*":
				return IntV(l.I * r.I)
			case "/":
				if r.I == 0 {
					in.p.warnf("运行时除以 0")
					return IntV(0)
				}
				return IntV(l.I / r.I)
			case "%":
				if r.I == 0 {
					in.p.warnf("运行时模 0")
					return IntV(0)
				}
				return IntV(l.I % r.I)
			}
		}
		lf, rf := l.AsFloat(), r.AsFloat()
		switch n.Op {
		case "+":
			return FloatV(lf + rf)
		case "-":
			return FloatV(lf - rf)
		case "*":
			return FloatV(lf * rf)
		case "/":
			return FloatV(lf / rf)
		case "%":
			return FloatV(float64(int64(lf) % int64(rf)))
		}
	case "<", "<=", ">", ">=":
		if l.Kind == KString && r.Kind == KString {
			switch n.Op {
			case "<":
				return BoolV(l.S < r.S)
			case "<=":
				return BoolV(l.S <= r.S)
			case ">":
				return BoolV(l.S > r.S)
			case ">=":
				return BoolV(l.S >= r.S)
			}
		}
		lf, rf := l.AsFloat(), r.AsFloat()
		switch n.Op {
		case "<":
			return BoolV(lf < rf)
		case "<=":
			return BoolV(lf <= rf)
		case ">":
			return BoolV(lf > rf)
		case ">=":
			return BoolV(lf >= rf)
		}
	case "==":
		return BoolV(l.Canon() == r.Canon())
	case "!=":
		return BoolV(l.Canon() != r.Canon())
	case "~=":
		return BoolV(strings.Contains(l.Canon(), r.Canon()))
	}
	return Unit()
}

func (in *interp) call(n *ast.CallExpr, e *env) Value {
	args := make([]Value, len(n.Args))
	for i, a := range n.Args {
		args[i] = in.eval(a, e)
	}

	switch fn := n.Fn.(type) {
	case *ast.Ident:
		switch fn.Name {
		case "learn":
			v := Unit()
			if len(args) > 0 {
				v = args[0]
			}
			in.p.addLearn("learn " + v.String())
			return Unit()
		case "is_hazardous":
			if len(args) == 0 {
				return BoolV(false)
			}
			return BoolV(hashOf(args[0].Canon())%2 == 1)
		case "hash_of":
			if len(args) == 0 {
				return IntV(0)
			}
			return IntV(hashOf(args[0].Canon()))
		}
		if fd, ok := in.p.fns[fn.Name]; ok {
			return in.callFn(fd, args)
		}
		in.p.warnf("运行时未知函数 %s", fn.Name)
		return Unit()

	case *ast.QualifiedRef:
		if len(fn.Parts) == 2 {
			if base, ok := e.get(fn.Parts[0]); ok && base.Kind == KInstance && base.Inst != nil {
				return in.callMethod(base.Inst, fn.Parts[1], args)
			}
		}
		return Unit()

	case *ast.FieldExpr:
		if base := in.eval(fn.X, e); base.Kind == KInstance && base.Inst != nil {
			return in.callMethod(base.Inst, fn.Name, args)
		}
	}
	return Unit()
}

// callMethod：Phase 5 起先看静态节点表，命中远端就 RPC（带 failover），否则本地执行。
func (in *interp) callMethod(inst *Instance, name string, args []Value) Value {
	if in.p.Cluster != nil && !in.noRemote && in.p.Table.Has(inst.Name) {
		wire, err := in.toWireArgs(args)
		if err == nil {
			id := in.p.nextReqID(inst.Name, name)
			resp, node, derr := in.p.Cluster.Dispatch(inst.Name, name, wire, id, in.traceID, in.depth+1)
			switch {
			case derr == nil && !resp.Local:
				in.p.mu.Lock()
				in.p.outputs = append(in.p.outputs, resp.Outputs...)
				in.p.learns = append(in.p.learns, resp.Learns...)
				in.p.outputs = append(in.p.outputs, fmt.Sprintf("rpc %s.%s @%s", inst.Name, name, node))
				in.p.mu.Unlock()
				in.p.appendRemoteTrace(resp.Trace)
				if resp.Deduped {
					in.p.addOutput("rpc deduped " + id)
				}
				return Unit()
			case derr != nil && !errors.Is(derr, cluster.ErrNoPlacement):
				panic(runtimeError{fmt.Sprintf("remote %s.%s 失败: %v", inst.Name, name, derr)})
			}
		}
	}
	return in.callMethodLocal(inst, name, args)
}

// CallCapability 实现 cluster.Executor：节点收到 RPC 后在本机执行 capability。
// 只捕获本次调用的 outputs/learns，并禁止再次远端转发。
func (p *Program) CallCapability(instance, method string, args []cluster.Value, traceID string, depth int) (res cluster.ExecResult, err error) {
	inst, ok := p.DB.Lookup(instance)
	if !ok {
		return res, fmt.Errorf("instance %q 不存在", instance)
	}
	vals := make([]Value, len(args))
	for i, a := range args {
		v, verr := p.fromWire(a)
		if verr != nil {
			return res, verr
		}
		vals[i] = v
	}

	p.mu.Lock()
	outMark, learnMark, traceMark := len(p.outputs), len(p.learns), len(p.trace)
	p.mu.Unlock()

	if traceID == "" {
		traceID = p.newTraceID() // 调用方没带 trace 时自己起一条
	}
	in := &interp{p: p, global: newEnv(nil), noRemote: true, traceID: traceID, depth: depth}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if re, ok := r.(runtimeError); ok {
					err = re
					return
				}
				panic(r)
			}
		}()
		in.callMethodLocal(inst, method, vals)
	}()

	p.mu.Lock()
	res.Outputs = append([]string(nil), p.outputs[outMark:]...)
	res.Learns = append([]string(nil), p.learns[learnMark:]...)
	recs := append([]TraceRecord(nil), p.trace[traceMark:]...)
	p.mu.Unlock()

	if len(recs) > 0 {
		raw, merr := json.Marshal(recs)
		if merr != nil {
			return res, merr
		}
		res.Trace = raw
	}
	return res, err
}

func (in *interp) toWireArgs(args []Value) ([]cluster.Value, error) {
	out := make([]cluster.Value, len(args))
	for i, a := range args {
		w, err := in.toWire(a)
		if err != nil {
			return nil, err
		}
		out[i] = w
	}
	return out, nil
}

func (in *interp) toWire(v Value) (cluster.Value, error) {
	switch v.Kind {
	case KUnit:
		return cluster.Value{Kind: "unit"}, nil
	case KInt:
		return cluster.Value{Kind: "int", I: v.I}, nil
	case KFloat:
		return cluster.Value{Kind: "float", F: v.F}, nil
	case KBool:
		return cluster.Value{Kind: "bool", B: v.B}, nil
	case KString:
		return cluster.Value{Kind: "string", S: v.S}, nil
	case KSize:
		return cluster.Value{Kind: "size", F: v.F, S: v.S}, nil
	case KDuration:
		return cluster.Value{Kind: "duration", F: v.F, S: v.S}, nil
	case KInstance:
		if v.Inst == nil {
			return cluster.Value{Kind: "unit"}, nil
		}
		return cluster.Value{Kind: "instance", S: v.Inst.Name}, nil
	}
	return cluster.Value{}, fmt.Errorf("无法序列化的值: %v", v.Kind)
}

func (p *Program) fromWire(v cluster.Value) (Value, error) {
	switch v.Kind {
	case "unit":
		return Unit(), nil
	case "int":
		return IntV(v.I), nil
	case "float":
		return FloatV(v.F), nil
	case "bool":
		return BoolV(v.B), nil
	case "string":
		return StringV(v.S), nil
	case "size":
		return Value{Kind: KSize, F: v.F, S: v.S}, nil
	case "duration":
		return Value{Kind: KDuration, F: v.F, S: v.S}, nil
	case "instance":
		inst, ok := p.DB.Lookup(v.S)
		if !ok {
			return Value{}, fmt.Errorf("instance %q 在本地不存在", v.S)
		}
		return InstV(inst), nil
	}
	return Value{}, fmt.Errorf("未知 wire kind %q", v.Kind)
}

// callMethodLocal 是 Phase 4 的本地执行路径。
func (in *interp) callMethodLocal(inst *Instance, name string, args []Value) Value {
	lg := in.p.logics[inst.Logic]
	if lg == nil {
		in.p.warnf("instance %s 的 logic %s 不存在", inst.Name, inst.Logic)
		return Unit()
	}
	for _, sig := range lg.Capability {
		if sig.Name != name {
			continue
		}
		if len(args) > len(sig.Params) {
			panic(runtimeError{fmt.Sprintf("%s.%s 需要 %d 个参数，实际传入 %d 个",
				inst.Name, name, len(sig.Params), len(args))})
		}
		in.p.addOutput("invoke " + inst.Name + "." + name)
		if sig.Body == nil {
			return Unit()
		}
		e := newEnv(in.global)
		e.define("self", InstV(inst))
		for i, p := range sig.Params {
			if i < len(args) {
				e.define(p.Name, args[i])
				continue
			}
			e.define(p.Name, in.synthesize(p.Type))
		}
		return in.execFnBody(sig.Body, e)
	}
	in.p.warnf("logic %s 没有 capability 方法 %s", inst.Logic, name)
	return Unit()
}

// hashOf：纯函数、确定性（FNV-1a）。
func hashOf(s string) int64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return int64(h % 1000000007)
}
