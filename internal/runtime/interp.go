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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"neura/internal/ast"
)

// Program 是加载后的可执行程序。
type Program struct {
	file *ast.File
	DB   *LogicDB

	fns       map[string]*ast.FnDecl
	logics    map[string]*ast.LogicDecl
	instances map[string]*ast.InstanceDecl

	placed map[string]string

	outputs  []string
	learns   []string
	warnings []string
	trace    []TraceRecord
	seq      int
}

// Result 是一次运行的产物。
type Result struct {
	Returned Value
	Outputs  []string
	Learns   []string
	Warnings []string
	Trace    []TraceRecord
}

// NewProgram 收集声明、装载实例、应用 place（含 stub 警告）。
func NewProgram(f *ast.File) *Program {
	p := &Program{
		file:      f,
		DB:        NewLogicDB(),
		fns:       map[string]*ast.FnDecl{},
		logics:    map[string]*ast.LogicDecl{},
		instances: map[string]*ast.InstanceDecl{},
		placed:    map[string]string{},
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

// applyPlace：只实现 local；其余打 stub warning（不阻断）。
func (p *Program) applyPlace(pd *ast.PlaceDecl) {
	if pd.AtSpec != nil && pd.AtSpec.Local {
		p.placed[pd.Name] = "local"
		return
	}
	nodes := "[]"
	if pd.AtSpec != nil {
		nodes = "[" + strings.Join(pd.AtSpec.Nodes, ", ") + "]"
	}
	p.placed[pd.Name] = "local(stub)"
	p.warnf("place %s at %s: stub（只实现 local，跨节点部署未实现，按 local 处理）", pd.Name, nodes)
}

func (p *Program) warnf(format string, a ...any) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, a...))
}

func (p *Program) addTrace(r TraceRecord) {
	p.seq++
	r.Seq = p.seq
	p.trace = append(p.trace, r)
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
	in := &interp{p: p, global: newEnv(nil)}
	ret := in.callFn(fd, args)
	return Result{
		Returned: ret,
		Outputs:  append([]string(nil), p.outputs...),
		Learns:   append([]string(nil), p.learns...),
		Warnings: append([]string(nil), p.warnings...),
		Trace:    append([]TraceRecord(nil), p.trace...),
	}, nil
}

// RunHandler 直接执行 neuron 的 handle 块（Phase 4 没有事件循环，emit 靠它可达）。
func (p *Program) RunHandler(neuronName string, args []Value) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			if re, ok := r.(runtimeError); ok {
				err = re
				return
			}
			panic(r)
		}
	}()
	var nd *ast.NeuronDecl
	for _, d := range p.file.Decls {
		if n, ok := d.(*ast.NeuronDecl); ok && n.Name == neuronName {
			nd = n
		}
	}
	if nd == nil {
		return Result{}, fmt.Errorf("neuron %q 不存在", neuronName)
	}
	if nd.Handler == nil {
		return Result{}, fmt.Errorf("neuron %q 没有 handle 块", neuronName)
	}
	in := &interp{p: p, global: newEnv(nil)}
	e := newEnv(nil)
	for i, port := range nd.Ports {
		if i < len(args) {
			e.define(port.Name, args[i])
			continue
		}
		e.define(port.Name, in.synthesize(port.Type))
	}
	in.execFnBody(nd.Handler, e)
	return Result{
		Returned: Unit(),
		Outputs:  append([]string(nil), p.outputs...),
		Learns:   append([]string(nil), p.learns...),
		Warnings: append([]string(nil), p.warnings...),
		Trace:    append([]TraceRecord(nil), p.trace...),
	}, nil
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
	p.warnf("stub: embedding 相似度未实现（%q）", text)
	return nil
}

func (p *Program) CustomSimilarity(a, b map[string]Value) float64 {
	p.warnf("stub: custom similarity 未实现，回退到纯函数 Similarity")
	return Similarity(a, b)
}

func (p *Program) AutoDelta() bool {
	p.warnf("stub: 自动 delta 未实现")
	return false
}

func (p *Program) CrossNodeExactlyOnce() bool {
	p.warnf("stub: 跨节点 exactly_once 未实现")
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
	p      *Program
	global *env
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
		v := in.eval(x.X, e)
		in.p.outputs = append(in.p.outputs, "emit "+v.String())
		return v

	case *ast.InvokeStmt:
		return in.eval(x.X, e)

	case *ast.ReuseStmt:
		v := in.eval(x.X, e)
		in.p.outputs = append(in.p.outputs, "reuse "+v.String())
		return v

	case *ast.LearnStmt:
		v := in.eval(x.X, e)
		in.p.learns = append(in.p.learns, "learn "+v.String())
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
		in.p.addTrace(TraceRecord{
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
				Score:    Similarity(s.Fields, c.Fields),
				Guard:    pass,
			})
		}
		sortCandidates(all) // trace 里也保持 strict 优先、分数降序、名字升序
		rec.Candidates = all
		if len(all) == 0 {
			rec.Reason = "no-candidate"
			in.p.addTrace(rec)
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
			in.p.addTrace(rec)
			continue
		}
		sortCandidates(passing)

		chosen, ok := in.p.DB.Lookup(passing[0].Instance)
		if !ok {
			rec.Reason = "no-candidate"
			in.p.addTrace(rec)
			continue
		}
		rec.Chosen = chosen.Name
		if passing[0].Strict {
			rec.Reason = "strict"
		} else {
			rec.Reason = "similarity"
		}
		rec.Bind = arm.Bind
		in.p.addTrace(rec)

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
			in.p.learns = append(in.p.learns, "learn "+v.String())
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

// callMethod 执行 capability 方法（capability 不在 neuron handler 内，emit 已由 checker 拦）。
func (in *interp) callMethod(inst *Instance, name string, args []Value) Value {
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
		in.p.outputs = append(in.p.outputs, "invoke "+inst.Name+"."+name)
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
