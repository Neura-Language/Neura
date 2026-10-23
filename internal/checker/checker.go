// Package checker 实现 Neura v0.3.7 的静态语义检查（Phase 3）。
//
// 覆盖的规则（v0.3.7 §12 清单 + 决策）：
//
//	1  顶层声明重名                     2  引用未知类型
//	4  字段重名                         5  instance ↔ logic / binding
//	9  未知函数 / 未知 capability 方法  10 调用参数个数与类型
//
// 17  返回类型（含隐式尾表达式）       19 self 上下文
// 20  unit_lit 位置                   28 synapse 端口存在性/方向
// 29  端口类型匹配                    34 place 目标
// 35  纯函数（where）                 39 局部 mut 赋值不算副作用
// 36  Range 不可显式声明              37 emit 上下文
//
//	3  hazardous 必须 strict 或带 guard（N3）
//
// N4 未知 where 标识符                N5 consistent_hash 字段
// N8  unit_lit 位置                   N11 break/continue 循环外
// N12 where 非纯函数
//
// 规则 14 已废弃（语义并入 37）：本 checker 跳过并在 Logs 里打印
// "rule 14 deprecated, see rule 37"。
package checker

import (
	"fmt"
	"sort"
	"strings"

	"neura/internal/ast"
	"neura/internal/diag"
	"neura/internal/lexer"
)

// Result 是检查结果：诊断 + 日志（deprecated 提示等）。
type Result struct {
	Errors []diag.Error
	Logs   []string
}

// DeprecatedRule14Log 是规则 14 的废弃提示（原样输出，便于 CI 匹配）。
const DeprecatedRule14Log = "rule 14 deprecated, see rule 37"

func Check(f *ast.File) Result {
	c := &Checker{
		sym:         newSymbols(),
		typeCache:   map[ast.Expr]string{},
		purityState: map[string]int{},
		whereKnown:  map[string]string{},
	}
	c.res.Logs = append(c.res.Logs, DeprecatedRule14Log)
	c.collect(f)
	c.checkDecls(f)
	c.sortErrs()
	return c.res
}

// ---------------- 符号表与上下文 ----------------

type varInfo struct {
	Type    string
	Fn      *ast.FnType
	Mutable bool
}

type symbols struct {
	kind      map[string]string
	structs   map[string]*ast.StructDecl
	logics    map[string]*ast.LogicDecl
	instances map[string]*ast.InstanceDecl
	neurons   map[string]*ast.NeuronDecl
	fns       map[string]*ast.FnDecl
	fields    map[string]map[string]string     // struct/logic → field → sig
	caps      map[string]map[string]*ast.FnSig // logic → method → sig
	ports     map[string]map[string]*ast.Port  // neuron → port → decl
}

func newSymbols() *symbols {
	return &symbols{
		kind:      map[string]string{},
		structs:   map[string]*ast.StructDecl{},
		logics:    map[string]*ast.LogicDecl{},
		instances: map[string]*ast.InstanceDecl{},
		neurons:   map[string]*ast.NeuronDecl{},
		fns:       map[string]*ast.FnDecl{},
		fields:    map[string]map[string]string{},
		caps:      map[string]map[string]*ast.FnSig{},
		ports:     map[string]map[string]*ast.Port{},
	}
}

const (
	pureUnknown = 0
	pureYes     = 1
	pureNo      = 2
	pureBusy    = 3
)

type Checker struct {
	res         Result
	sym         *symbols
	typeCache   map[ast.Expr]string
	purityState map[string]int

	scopes []map[string]varInfo

	curResult string
	loopDepth int
	emitOK    bool
	inLogic   bool

	inWhere    bool
	whereKnown map[string]string
}

// builtinWhereIdents：where 表达式里可直接引用的内建资源指标。
// （N12 的 gpu_mem 属此类；N4 的 foo_bar 不是 → E-UNKNOWN-WHERE-IDENT）
var builtinWhereIdents = map[string]bool{
	"gpu_mem": true, "cpu_mem": true, "gpu_util": true, "now": true,
}

// pureBuiltins：视为纯函数的内建（规则 35）。
var pureBuiltins = map[string]bool{
	"is_hazardous": true,
	"hash_of":      true,
}

func (c *Checker) errorAt(p diag.Pos, code, msg string) {
	c.res.Errors = append(c.res.Errors, diag.Error{Code: code, Pos: p, Msg: msg})
}

func (c *Checker) sortErrs() {
	sort.SliceStable(c.res.Errors, func(i, j int) bool {
		a, b := c.res.Errors[i].Pos, c.res.Errors[j].Pos
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
}

func (c *Checker) pushScope() { c.scopes = append(c.scopes, map[string]varInfo{}) }

func (c *Checker) popScope() {
	if n := len(c.scopes); n > 0 {
		c.scopes = c.scopes[:n-1]
	}
}

func (c *Checker) declareVar(name string, v varInfo, p diag.Pos) {
	if len(c.scopes) == 0 {
		return
	}
	top := c.scopes[len(c.scopes)-1]
	if _, dup := top[name]; dup {
		c.errorAt(p, diag.CodeDupDecl, "变量 "+name+" 在同一作用域内重复声明")
		return
	}
	top[name] = v
}

func (c *Checker) lookupVar(name string) (varInfo, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if v, ok := c.scopes[i][name]; ok {
			return v, true
		}
	}
	return varInfo{}, false
}

// ---------------- 类型签名 ----------------

func sigOf(t ast.Type) string {
	switch n := t.(type) {
	case nil:
		return "Unit"
	case *ast.NamedType:
		if len(n.Args) == 0 {
			return n.Name
		}
		parts := make([]string, len(n.Args))
		for i, a := range n.Args {
			parts[i] = sigOf(a)
		}
		return n.Name + "<" + strings.Join(parts, ", ") + ">"
	case *ast.TupleType:
		if len(n.Elems) == 0 {
			return "()"
		}
		parts := make([]string, len(n.Elems))
		for i, e := range n.Elems {
			parts[i] = sigOf(e)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case *ast.FnType:
		params := make([]string, len(n.Params))
		for i, p := range n.Params {
			params[i] = sigOf(p)
		}
		return "fn(" + strings.Join(params, ", ") + ") -> " + sigOf(n.Result)
	}
	return "?"
}

func asFnType(t ast.Type) *ast.FnType {
	if f, ok := t.(*ast.FnType); ok {
		return f
	}
	return nil
}

func isUnitSig(s string) bool { return s == "" || s == "Unit" || s == "()" }

func compatible(a, b string) bool {
	if a == "?" || b == "?" || a == b {
		return true
	}
	return isUnitSig(a) && isUnitSig(b)
}

func isNumeric(s string) bool {
	switch s {
	case "Int", "Int32", "Int64", "Float", "F32", "F64", "Duration", "Size":
		return true
	}
	return false
}

func exprPosOf(e ast.Expr) diag.Pos {
	switch n := e.(type) {
	case *ast.Ident:
		return n.At
	case *ast.QualifiedRef:
		return n.At
	case *ast.Literal:
		return n.At
	case *ast.UnitExpr:
		return n.At
	case *ast.RangeExpr:
		return n.At
	case *ast.UnaryExpr:
		return n.At
	case *ast.BinaryExpr:
		return n.At
	case *ast.CallExpr:
		return n.At
	case *ast.FieldExpr:
		return n.At
	case *ast.ParenExpr:
		return n.At
	}
	return diag.Pos{}
}

// ---------------- 收集符号（规则 1 / 4） ----------------

func (c *Checker) declareType(name, kind string, p diag.Pos) {
	if prev, ok := c.sym.kind[name]; ok {
		c.errorAt(p, diag.CodeDupDecl, fmt.Sprintf("%s 重复声明（已声明为 %s）", name, prev))
		return
	}
	c.sym.kind[name] = kind
}

func (c *Checker) collect(f *ast.File) {
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.StructDecl:
			c.declareType(x.Name, "struct", x.At)
			c.sym.structs[x.Name] = x
			fields := map[string]string{}
			for _, fl := range x.Fields {
				if _, dup := fields[fl.Name]; dup {
					c.errorAt(fl.At, diag.CodeDupField, fmt.Sprintf("struct %s 字段 %s 重复声明", x.Name, fl.Name))
					continue
				}
				fields[fl.Name] = sigOf(fl.Type)
			}
			c.sym.fields[x.Name] = fields

		case *ast.LogicDecl:
			c.declareType(x.Name, "logic", x.At)
			c.sym.logics[x.Name] = x
			fields := map[string]string{}
			for _, fl := range x.Structure {
				if _, dup := fields[fl.Name]; dup {
					c.errorAt(fl.At, diag.CodeDupField, fmt.Sprintf("logic %s 字段 %s 重复声明", x.Name, fl.Name))
					continue
				}
				fields[fl.Name] = sigOf(fl.Type)
			}
			c.sym.fields[x.Name] = fields
			caps := map[string]*ast.FnSig{}
			for _, sig := range x.Capability {
				if _, dup := caps[sig.Name]; dup {
					c.errorAt(sig.At, diag.CodeDupDecl, fmt.Sprintf("logic %s 的 capability %s 重复声明", x.Name, sig.Name))
					continue
				}
				caps[sig.Name] = sig
			}
			c.sym.caps[x.Name] = caps

		case *ast.InstanceDecl:
			c.declareType(x.Name, "instance", x.At)
			c.sym.instances[x.Name] = x

		case *ast.NeuronDecl:
			c.declareType(x.Name, "neuron", x.At)
			c.sym.neurons[x.Name] = x
			ports := map[string]*ast.Port{}
			for _, p := range x.Ports {
				if _, dup := ports[p.Name]; dup {
					c.errorAt(p.At, diag.CodeDupDecl, fmt.Sprintf("neuron %s 端口 %s 重复声明", x.Name, p.Name))
					continue
				}
				ports[p.Name] = p
			}
			c.sym.ports[x.Name] = ports

		case *ast.FnDecl:
			if _, ok := c.sym.kind[x.Name]; ok {
				c.errorAt(x.At, diag.CodeDupDecl, "函数 "+x.Name+" 与已有声明重名")
				continue
			}
			c.sym.kind[x.Name] = "fn"
			c.sym.fns[x.Name] = x
		}
	}
}

// ---------------- 声明检查 ----------------

func (c *Checker) checkDecls(f *ast.File) {
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.StructDecl:
			for _, fl := range x.Fields {
				c.checkType(fl.Type)
			}
		case *ast.FnDecl:
			c.checkFn(x)
		case *ast.LogicDecl:
			c.checkLogic(x)
		case *ast.InstanceDecl:
			c.checkInstance(x)
		case *ast.NeuronDecl:
			c.checkNeuron(x)
		case *ast.SynapseDecl:
			c.checkSynapse(x)
		case *ast.PlaceDecl:
			c.checkPlace(x)
		}
	}
}

// 规则 2：类型引用必须存在（内建 20 个 / 已声明 struct·logic·neuron）。
func (c *Checker) checkType(t ast.Type) {
	switch n := t.(type) {
	case nil:
		return
	case *ast.NamedType:
		if !lexer.BuiltinTypes[n.Name] {
			if _, ok := c.sym.kind[n.Name]; !ok {
				c.errorAt(n.At, diag.CodeUnknownType, "未知类型 "+n.Name)
			}
		}
		for _, a := range n.Args {
			c.checkType(a)
		}
	case *ast.TupleType:
		for _, e := range n.Elems {
			c.checkType(e)
		}
	case *ast.FnType:
		for _, p := range n.Params {
			c.checkType(p)
		}
		c.checkType(n.Result)
	}
}

func (c *Checker) checkFn(fd *ast.FnDecl) {
	c.checkType(fd.Result)
	c.pushScope()
	defer c.popScope()
	for _, p := range fd.Params {
		c.checkType(p.Type)
		c.declareVar(p.Name, varInfo{Type: sigOf(p.Type), Fn: asFnType(p.Type), Mutable: false}, p.At)
	}

	savedResult, savedEmit, savedLogic, savedLoop := c.curResult, c.emitOK, c.inLogic, c.loopDepth
	c.curResult, c.emitOK, c.inLogic, c.loopDepth = sigOf(fd.Result), false, false, 0
	c.checkFnBody(fd.Body)
	c.curResult, c.emitOK, c.inLogic, c.loopDepth = savedResult, savedEmit, savedLogic, savedLoop
}

// checkFnBody：语句检查 + 规则 17 的尾表达式/返回检查。
func (c *Checker) checkFnBody(b *ast.Block) {
	c.pushScope()
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	// 规则 17：非 Unit 返回类型的函数，尾表达式/尾 if 必须与返回类型一致
	if !isUnitSig(c.curResult) {
		if tt, ok := c.blockTail(b); ok {
			if !compatible(c.curResult, tt) {
				c.errorAt(b.At, diag.CodeReturnType,
					fmt.Sprintf("函数返回 %s，但末尾表达式的类型是 %s", c.curResult, tt))
			}
		} else if len(b.Stmts) == 0 {
			c.errorAt(b.At, diag.CodeMissingReturn,
				"函数返回 "+c.curResult+"，但函数体为空")
		}
	}
	c.popScope()
}

// blockTail 取块尾的值类型（块尾表达式 / 两侧都有值的 if）。
func (c *Checker) blockTail(b *ast.Block) (string, bool) {
	if b == nil || len(b.Stmts) == 0 {
		return "Unit", isUnitSig(c.curResult)
	}
	return c.stmtTail(b.Stmts[len(b.Stmts)-1])
}

func (c *Checker) stmtTail(s ast.Stmt) (string, bool) {
	switch x := s.(type) {
	case *ast.ExprStmt:
		return c.exprType(x.X), true
	case *ast.ReturnStmt:
		if x.Value == nil {
			return "Unit", true
		}
		return c.exprType(x.Value), true
	case *ast.Block:
		return c.blockTail(x)
	case *ast.IfStmt:
		if x.Else == nil {
			return "", false
		}
		t1, ok1 := c.blockTail(x.Then)
		t2, ok2 := c.stmtTail(x.Else)
		if !ok1 || !ok2 {
			return "", false
		}
		if !compatible(t1, t2) {
			c.errorAt(x.At, diag.CodeTypeMismatch,
				fmt.Sprintf("if 两分支类型不一致：%s vs %s", t1, t2))
			return t1, true
		}
		return t1, true
	}
	return "", false
}

func (c *Checker) checkLogic(l *ast.LogicDecl) {
	for _, fl := range l.Structure {
		c.checkType(fl.Type)
	}

	// N3：hazardous 必须 strict 或带 guard
	hazardous := false
	for _, m := range l.Modifiers {
		if m == "hazardous" {
			hazardous = true
		}
	}
	if hazardous && !l.Strict && l.Guard == nil {
		c.errorAt(l.At, diag.CodeHazardNoGuard, "hazardous logic 必须 strict 或带 guard")
	}
	if l.Guard != nil {
		if gt := c.exprType(l.Guard); !compatible("Bool", gt) {
			c.errorAt(l.At, diag.CodeCondNotBool, "guard 表达式必须是 Bool，实际是 "+gt)
		}
	}

	for _, sig := range l.Capability {
		c.checkType(sig.Result)
		for _, p := range sig.Params {
			c.checkType(p.Type)
		}
		if sig.Body == nil {
			continue
		}
		savedResult, savedEmit, savedLogic, savedLoop := c.curResult, c.emitOK, c.inLogic, c.loopDepth
		c.curResult, c.emitOK, c.inLogic, c.loopDepth = sigOf(sig.Result), false, true, 0
		c.pushScope()
		for _, p := range sig.Params {
			c.declareVar(p.Name, varInfo{Type: sigOf(p.Type), Fn: asFnType(p.Type)}, p.At)
		}
		c.checkFnBody(sig.Body)
		c.popScope()
		c.curResult, c.emitOK, c.inLogic, c.loopDepth = savedResult, savedEmit, savedLogic, savedLoop
	}
}

func (c *Checker) checkInstance(in *ast.InstanceDecl) {
	// 规则 5：logic 必须存在
	lg, ok := c.sym.logics[in.Logic]
	if !ok {
		c.errorAt(in.At, diag.CodeInstanceLogic, "instance "+in.Name+" 指向的 logic "+in.Logic+" 不存在")
	}

	want := map[string]string{}
	if ok {
		for _, fl := range lg.Structure {
			want[fl.Name] = sigOf(fl.Type)
		}
	}

	seen := map[string]bool{}
	for _, b := range in.Bindings {
		if seen[b.Name] {
			c.errorAt(b.At, diag.CodeDupField, "binding "+b.Name+" 重复")
			continue
		}
		seen[b.Name] = true
		vt := c.exprType(b.Value)
		wt, defined := want[b.Name]
		if !ok {
			continue // logic 缺失已报，避免级联
		}
		if !defined {
			c.errorAt(b.At, diag.CodeBindingExtra,
				fmt.Sprintf("%s 的 structure 没有字段 %s", in.Logic, b.Name))
			continue
		}
		if !compatible(wt, vt) {
			c.errorAt(b.At, diag.CodeBindingType,
				fmt.Sprintf("binding %s 类型是 %s，但 %s 的字段类型是 %s", b.Name, vt, in.Logic, wt))
		}
	}
	if ok {
		for name, wt := range want {
			if !seen[name] {
				c.errorAt(in.At, diag.CodeBindingMissing,
					fmt.Sprintf("instance %s 缺少 structure 字段 %s: %s", in.Name, name, wt))
			}
		}
	}
}

func (c *Checker) checkNeuron(n *ast.NeuronDecl) {
	for _, p := range n.Ports {
		c.checkType(p.Type)
	}

	// 每个 handler 单独一个作用域：端口可见，signal 名绑定为声明类型，emit 合法（规则 37）。
	checkHandler := func(sh *ast.SignalHandler, body *ast.Block) {
		if sh != nil {
			c.checkType(sh.Type)
		}
		savedEmit, savedLogic, savedLoop, savedResult := c.emitOK, c.inLogic, c.loopDepth, c.curResult
		c.emitOK, c.inLogic, c.loopDepth, c.curResult = true, false, 0, "Unit"
		c.pushScope()
		for _, p := range n.Ports {
			c.declareVar(p.Name, varInfo{Type: sigOf(p.Type), Mutable: false}, p.At)
		}
		if sh != nil && sh.Name != "" {
			c.declareVar(sh.Name, varInfo{Type: sigOf(sh.Type), Mutable: false}, sh.At)
		}
		c.checkBlock(body)
		c.popScope()
		c.emitOK, c.inLogic, c.loopDepth, c.curResult = savedEmit, savedLogic, savedLoop, savedResult
	}

	for _, sh := range n.Signals {
		checkHandler(sh, sh.Body)
	}
	if n.Handler != nil {
		checkHandler(nil, n.Handler)
	}
}

// 规则 28 / 29：synapse 两端端口必须存在、方向正确、类型一致。
func (c *Checker) checkSynapse(s *ast.SynapseDecl) {
	if len(s.Src.Parts) != 2 || len(s.Dst.Parts) != 2 {
		c.errorAt(s.At, diag.CodePortNotFound, "synapse 两端必须是 Neuron.port 形式")
		return
	}
	srcN, dstN := s.Src.Parts[0], s.Dst.Parts[0]
	srcP := s.Src.Parts[1]
	dstP := s.Dst.Parts[1]

	srcType, ok1 := c.portType(srcN, srcP, "output", s.Src.At)
	dstType, ok2 := c.portType(dstN, dstP, "input", s.Dst.At)
	if ok1 && ok2 && !compatible(srcType, dstType) {
		c.errorAt(s.At, diag.CodePortType,
			fmt.Sprintf("端口类型不匹配：%s.%s 是 %s，%s.%s 是 %s",
				srcN, srcP, srcType, dstN, dstP, dstType))
	}
}

func (c *Checker) portType(neuron, port, wantDir string, p diag.Pos) (string, bool) {
	n, ok := c.sym.neurons[neuron]
	if !ok {
		c.errorAt(p, diag.CodeUnknownNeuron, "未定义的 neuron "+neuron)
		return "?", false
	}
	for _, pr := range n.Ports {
		if pr.Name != port {
			continue
		}
		if pr.Dir != wantDir {
			c.errorAt(p, diag.CodePortDirection,
				fmt.Sprintf("%s.%s 是 %s 端口，这里需要 %s", neuron, port, pr.Dir, wantDir))
			return sigOf(pr.Type), false
		}
		return sigOf(pr.Type), true
	}
	c.errorAt(p, diag.CodePortNotFound, fmt.Sprintf("%s 没有端口 %s", neuron, port))
	return "?", false
}

// fieldsOfTarget：struct 自身字段；instance 用其 logic 的 structure。
func (c *Checker) fieldsOfTarget(name string) (map[string]string, bool) {
	if f, ok := c.sym.fields[name]; ok {
		return f, true
	}
	if inst, ok := c.sym.instances[name]; ok {
		if f, ok := c.sym.fields[inst.Logic]; ok {
			return f, true
		}
	}
	return nil, false
}

func (c *Checker) checkPlace(p *ast.PlaceDecl) {
	// 规则 34：place 目标必须已声明
	if _, ok := c.sym.kind[p.Name]; !ok {
		c.errorAt(p.At, diag.CodePlaceTarget, "place 目标 "+p.Name+" 未声明")
	}

	// where：N4 未知标识符 + N12 纯函数
	if p.Where != nil {
		savedIn, savedKnown := c.inWhere, c.whereKnown
		c.inWhere = true
		c.whereKnown = map[string]string{}
		for name := range builtinWhereIdents {
			c.whereKnown[name] = "?"
		}
		if f, ok := c.fieldsOfTarget(p.Name); ok {
			for name := range f {
				c.whereKnown[name] = "?"
			}
		}
		c.checkWhere(p.Where)
		c.inWhere, c.whereKnown = savedIn, savedKnown
	}

	// N5：consistent_hash 的字段必须在该目标的 structure 中
	if p.Balance != nil && p.Balance.Kind == "consistent_hash" {
		fields, hasFields := c.fieldsOfTarget(p.Name)
		for _, a := range p.Balance.Args {
			id, ok := a.(*ast.Ident)
			if !ok {
				continue
			}
			if !hasFields {
				continue
			}
			if _, ok := fields[id.Name]; !ok {
				c.errorAt(id.At, diag.CodeHashFieldNotFound,
					fmt.Sprintf("consistent_hash 字段 %s 不在 %s 的 structure 中", id.Name, p.Name))
			}
		}
	}

	// 注：policy 的参数按各自策略单独校验（consistent_hash 验字段名），
	// 不做通用 exprType —— 否则 balance 的字段名会被当成普通标识符误报。
}

// checkWhere：类型/标识符检查（inWhere=true）+ 规则 35 纯函数检查。
func (c *Checker) checkWhere(e ast.Expr) {
	c.exprType(e)
	c.walkCalls(e, func(callee ast.Expr) {
		switch fn := callee.(type) {
		case *ast.Ident:
			if pureBuiltins[fn.Name] {
				return
			}
			if !c.fnPure(fn.Name) {
				c.errorAt(fn.At, diag.CodeWhereNotPure,
					"where 表达式的函数必须是纯函数（含 learn 日志副作用）")
			}
		case *ast.QualifiedRef:
			// 方法调用视为外部调用，非白名单 → 非纯
			c.errorAt(fn.At, diag.CodeWhereNotPure,
				"where 表达式的函数必须是纯函数（方法调用视为外部调用）")
		}
	})
}

func (c *Checker) walkCalls(e ast.Expr, fn func(callee ast.Expr)) {
	switch x := e.(type) {
	case nil:
		return
	case *ast.CallExpr:
		fn(x.Fn)
		for _, a := range x.Args {
			c.walkCalls(a, fn)
		}
	case *ast.BinaryExpr:
		c.walkCalls(x.L, fn)
		c.walkCalls(x.R, fn)
	case *ast.UnaryExpr:
		c.walkCalls(x.X, fn)
	case *ast.ParenExpr:
		c.walkCalls(x.X, fn)
	case *ast.RangeExpr:
		c.walkCalls(x.Lo, fn)
		c.walkCalls(x.Hi, fn)
	case *ast.FieldExpr:
		c.walkCalls(x.X, fn)
	case *ast.UnitExpr:
		c.walkCalls(x.Value, fn)
	}
}

// ---------------- 语句检查 ----------------

func (c *Checker) checkBlock(b *ast.Block) {
	if b == nil {
		return
	}
	c.pushScope()
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	c.popScope()
}

func (c *Checker) checkStmt(s ast.Stmt) {
	switch x := s.(type) {
	case *ast.Block:
		c.checkBlock(x)

	case *ast.LetStmt:
		if x.Type != nil {
			c.checkType(x.Type)
		}
		vt := c.exprType(x.Value)
		declared := ""
		if x.Type != nil {
			declared = sigOf(x.Type)
		}
		// 规则 36：Range 不能显式声明（parser 已拦，这里兜底）
		if declared == "Range" {
			c.errorAt(x.At, diag.CodeRangeOutsideFor, "Range 是 for 专用类型，不能显式声明")
		}
		if declared != "" && !compatible(declared, vt) {
			c.errorAt(x.At, diag.CodeTypeMismatch,
				fmt.Sprintf("let %s 声明为 %s，但初值类型是 %s", x.Name, declared, vt))
		}
		typ := declared
		if typ == "" {
			typ = vt
		}
		c.declareVar(x.Name, varInfo{Type: typ, Fn: asFnType(x.Type), Mutable: x.Mutable}, x.At)

	case *ast.AssignStmt:
		tt, mutable := c.assignTarget(x.Target)
		vt := c.exprType(x.Value)
		if !mutable {
			c.errorAt(exprPosOf(x.Target), diag.CodeAssignImmutable,
				"给不可变变量赋值；需要可变语义请用 let mut")
		}
		if !compatible(tt, vt) {
			c.errorAt(exprPosOf(x.Target), diag.CodeTypeMismatch,
				fmt.Sprintf("赋值类型不匹配：左侧 %s，右侧 %s", tt, vt))
		}

	case *ast.ExprStmt:
		c.exprType(x.X)

	case *ast.DoStmt:
		// 规则 37：do block 内禁止 emit
		saved := c.emitOK
		c.emitOK = false
		c.checkBlock(x.Body)
		c.emitOK = saved

	case *ast.ForStmt:
		it := c.exprType(x.Iter)
		if it != "Range" && it != "?" {
			c.errorAt(x.At, diag.CodeForNotRange,
				"for 的迭代值必须是 Range（a..b / a..=b），实际是 "+it)
		}
		c.pushScope()
		c.declareVar(x.Var, varInfo{Type: "Int"}, x.At)
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		c.popScope()

	case *ast.IfStmt:
		if ct := c.exprType(x.Cond); !compatible("Bool", ct) {
			c.errorAt(x.At, diag.CodeCondNotBool, "if 条件必须是 Bool，实际是 "+ct)
		}
		c.checkBlock(x.Then)
		if x.Else != nil {
			c.checkStmt(x.Else)
		}

	case *ast.MatchStmt:
		c.checkMatch(x)

	case *ast.ReturnStmt:
		if isUnitSig(c.curResult) {
			if x.Value != nil {
				c.errorAt(x.At, diag.CodeReturnType, "函数返回 Unit，不能 return 值")
				c.exprType(x.Value)
			}
			return
		}
		if x.Value == nil {
			c.errorAt(x.At, diag.CodeReturnType, "函数返回 "+c.curResult+"，需要 return 值")
			return
		}
		vt := c.exprType(x.Value)
		if !compatible(c.curResult, vt) {
			c.errorAt(x.At, diag.CodeReturnType,
				fmt.Sprintf("return 类型是 %s，但函数返回 %s", vt, c.curResult))
		}

	case *ast.BreakStmt:
		if c.loopDepth == 0 {
			c.errorAt(x.At, diag.CodeBreakOutsideLoop, "break 只能出现在循环体内")
		}

	case *ast.ContinueStmt:
		if c.loopDepth == 0 {
			c.errorAt(x.At, diag.CodeContinueOutsideLoop, "continue 只能出现在循环体内")
		}

	case *ast.EmitStmt:
		// 规则 37：emit 仅 neuron handler 合法
		if !c.emitOK {
			c.errorAt(x.At, diag.CodeEmitContext,
				"emit 仅在 neuron handler 内合法（do block / fn block 禁止，规则 37）")
		}
		c.exprType(x.X)

	case *ast.InvokeStmt:
		c.exprType(x.X)

	case *ast.ReuseStmt:
		c.exprType(x.X)

	case *ast.LearnStmt:
		c.exprType(x.X)
	}
}

// assignTarget：返回目标类型与是否可赋值。
func (c *Checker) assignTarget(e ast.Expr) (string, bool) {
	switch t := e.(type) {
	case *ast.Ident:
		if v, ok := c.lookupVar(t.Name); ok {
			return v.Type, v.Mutable
		}
		if t.Name == "self" && c.inLogic {
			return "?", true
		}
		c.errorAt(t.At, diag.CodeUnknownIdent, "未知标识符 "+t.Name)
		return "?", true
	case *ast.QualifiedRef:
		base := t.Parts[0]
		if base == "self" {
			if !c.inLogic {
				c.errorAt(t.At, diag.CodeSelfOutside, "self 只能出现在 logic capability 内")
			}
			return "?", true
		}
		if v, ok := c.lookupVar(base); ok {
			tp := v.Type
			for _, f := range t.Parts[1:] {
				tp = c.fieldType(tp, f, t.At)
			}
			return tp, true
		}
		if c.inWhere {
			return "?", true
		}
		c.errorAt(t.At, diag.CodeUnknownIdent, "未知标识符 "+base)
		return "?", true
	case *ast.FieldExpr:
		tp := c.exprType(t.X)
		return c.fieldType(tp, t.Name, t.At), true
	}
	return "?", true
}

func (c *Checker) checkMatch(m *ast.MatchStmt) {
	subj := c.exprType(m.Subject)
	if id, ok := m.Against.(*ast.Ident); !ok || id.Name != "LogicDB" {
		c.errorAt(exprPosOf(m.Against), diag.CodeMatchTarget, "match 的 against 必须是 LogicDB")
	}

	for _, arm := range m.Arms {
		pat := strings.Join(arm.Pattern.Parts, ".")
		bindType := pat
		if inst, isInst := c.sym.instances[pat]; isInst {
			if subj != "?" && inst.Logic != subj {
				c.errorAt(arm.Pattern.At, diag.CodeMatchPattern,
					fmt.Sprintf("%s 是 %s 的 instance，匹配不上 %s", pat, inst.Logic, subj))
			}
			bindType = inst.Logic
		} else if subj != "?" && pat != subj {
			c.errorAt(arm.Pattern.At, diag.CodeMatchPattern,
				fmt.Sprintf("%s 不是 %s 的 instance", pat, subj))
		}

		c.pushScope()
		if arm.Bind != "" {
			c.declareVar(arm.Bind, varInfo{Type: bindType}, arm.At)
		}
		c.checkStmt(arm.Body)
		c.popScope()
	}
}

// ---------------- 表达式类型 ----------------

func (c *Checker) exprType(e ast.Expr) string {
	if e == nil {
		return "Unit"
	}
	if t, ok := c.typeCache[e]; ok {
		return t
	}
	t := c.computeExprType(e)
	c.typeCache[e] = t
	return t
}

func (c *Checker) computeExprType(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Literal:
		switch x.Kind {
		case "int":
			return "Int"
		case "float":
			return "Float"
		case "string":
			return "String"
		case "duration":
			return "Duration"
		case "size":
			return "Size"
		case "bool":
			return "Bool"
		}
		return "?"

	case *ast.UnitExpr:
		// 规则 20 / N8：expr 位置不是 physics_value；span 指向 [ - ] (int|float)
		c.errorAt(x.At, diag.CodeUnitOutsidePhysics, "unit_lit 仅在 physics_value 位置合法")
		return "?"

	case *ast.UnaryExpr:
		if u, ok := x.X.(*ast.UnitExpr); ok && x.Op == "-" {
			// 有 - 时 span 指向 -（规则 20）
			c.errorAt(x.At, diag.CodeUnitOutsidePhysics, "unit_lit 仅在 physics_value 位置合法")
			if lit, ok := u.Value.(*ast.Literal); ok {
				return c.computeExprType(lit)
			}
			return "?"
		}
		t := c.exprType(x.X)
		if x.Op == "-" {
			if !isNumeric(t) && t != "?" {
				c.errorAt(x.At, diag.CodeTypeMismatch, "一元 - 需要数值类型，实际是 "+t)
			}
			return t
		}
		if t != "Bool" && t != "?" {
			c.errorAt(x.At, diag.CodeTypeMismatch, "! 需要 Bool，实际是 "+t)
		}
		return "Bool"

	case *ast.BinaryExpr:
		return c.binaryType(x)

	case *ast.ParenExpr:
		return c.exprType(x.X)

	case *ast.RangeExpr:
		lo := c.exprType(x.Lo)
		hi := c.exprType(x.Hi)
		if !compatible(lo, hi) {
			c.errorAt(x.OpAt, diag.CodeTypeMismatch,
				fmt.Sprintf("区间两端类型不一致：%s vs %s", lo, hi))
		}
		if !isNumeric(lo) && lo != "?" {
			c.errorAt(x.OpAt, diag.CodeTypeMismatch, "区间边界需要数值类型，实际是 "+lo)
		}
		return "Range"

	case *ast.Ident:
		return c.identType(x)

	case *ast.QualifiedRef:
		return c.qualifiedType(x)

	case *ast.CallExpr:
		return c.callType(x)

	case *ast.FieldExpr:
		base := c.exprType(x.X)
		return c.fieldType(base, x.Name, x.At)
	}
	return "?"
}

func (c *Checker) binaryType(x *ast.BinaryExpr) string {
	l := c.exprType(x.L)
	r := c.exprType(x.R)

	switch x.Op {
	case "+", "-", "*", "/", "%":
		if (!isNumeric(l) && l != "?") || (!isNumeric(r) && r != "?") {
			c.errorAt(x.At, diag.CodeTypeMismatch,
				fmt.Sprintf("运算符 %s 需要数值操作数，实际是 %s 与 %s", x.Op, l, r))
			return "?"
		}
		if l == "Float" || r == "Float" || l == "F32" || l == "F64" {
			return "Float"
		}
		return "Int"
	case "<", "<=", ">", ">=":
		if (!isNumeric(l) && l != "?") || (!isNumeric(r) && r != "?") {
			c.errorAt(x.At, diag.CodeTypeMismatch,
				fmt.Sprintf("比较运算符 %s 需要数值操作数，实际是 %s 与 %s", x.Op, l, r))
		}
		return "Bool"
	case "==", "!=":
		if !compatible(l, r) {
			c.errorAt(x.At, diag.CodeTypeMismatch,
				fmt.Sprintf("%s 两侧类型不同：%s 与 %s", x.Op, l, r))
		}
		return "Bool"
	case "&&", "||":
		if (l != "Bool" && l != "?") || (r != "Bool" && r != "?") {
			c.errorAt(x.At, diag.CodeTypeMismatch,
				fmt.Sprintf("%s 需要 Bool 操作数，实际是 %s 与 %s", x.Op, l, r))
		}
		return "Bool"
	case "~=":
		return "Bool"
	}
	return "?"
}

func (c *Checker) identType(x *ast.Ident) string {
	if v, ok := c.lookupVar(x.Name); ok {
		return v.Type
	}
	if x.Name == "self" {
		if c.inLogic {
			return "?"
		}
		c.errorAt(x.At, diag.CodeSelfOutside, "self 只能出现在 logic capability 内")
		return "?"
	}
	// 规则 9：顶层函数名在 expr 位置是函数值（可作高阶函数实参）
	if fd, ok := c.sym.fns[x.Name]; ok {
		return sigOfFnDecl(fd)
	}
	if c.inWhere {
		if t, ok := c.whereKnown[x.Name]; ok {
			return t
		}
		c.errorAt(x.At, diag.CodeUnknownWhereIdent, "未知标识符 "+x.Name)
		return "?"
	}
	c.errorAt(x.At, diag.CodeUnknownIdent, "未知标识符 "+x.Name)
	return "?"
}

func (c *Checker) qualifiedType(x *ast.QualifiedRef) string {
	base := x.Parts[0]
	if v, ok := c.lookupVar(base); ok {
		tp := v.Type
		for _, f := range x.Parts[1:] {
			tp = c.fieldType(tp, f, x.At)
		}
		return tp
	}
	if base == "self" {
		if !c.inLogic {
			c.errorAt(x.At, diag.CodeSelfOutside, "self 只能出现在 logic capability 内")
		}
		return "?"
	}
	if c.inWhere {
		if t, ok := c.whereKnown[base]; ok {
			return t
		}
		c.errorAt(x.At, diag.CodeUnknownWhereIdent, "未知标识符 "+base)
		return "?"
	}
	// 类型名/neuron 名出现在 expr 位置：交给后续规则（synapse 单独检查）
	if _, ok := c.sym.kind[base]; ok {
		return "?"
	}
	c.errorAt(x.At, diag.CodeUnknownIdent, "未知标识符 "+base)
	return "?"
}

func (c *Checker) fieldType(base, name string, p diag.Pos) string {
	if base == "?" || base == "" {
		return "?"
	}
	if fields, ok := c.sym.fields[base]; ok {
		if t, ok := fields[name]; ok {
			return t
		}
		if caps, ok := c.sym.caps[base]; ok {
			if sig, ok := caps[name]; ok {
				return sigOfCap(sig)
			}
		}
		c.errorAt(p, diag.CodeUnknownField, fmt.Sprintf("%s 没有字段 %s", base, name))
		return "?"
	}
	if caps, ok := c.sym.caps[base]; ok {
		if sig, ok := caps[name]; ok {
			return sigOfCap(sig)
		}
		c.errorAt(p, diag.CodeUnknownMethod, fmt.Sprintf("%s 没有 capability 方法 %s", base, name))
		return "?"
	}
	return "?"
}

func sigOfFnDecl(fd *ast.FnDecl) string {
	params := make([]string, len(fd.Params))
	for i, p := range fd.Params {
		params[i] = sigOf(p.Type)
	}
	return "fn(" + strings.Join(params, ", ") + ") -> " + sigOf(fd.Result)
}

func sigOfCap(sig *ast.FnSig) string {
	params := make([]string, len(sig.Params))
	for i, p := range sig.Params {
		params[i] = sigOf(p.Type)
	}
	return "fn(" + strings.Join(params, ", ") + ") -> " + sigOf(sig.Result)
}

func (c *Checker) callType(x *ast.CallExpr) string {
	switch fn := x.Fn.(type) {
	case *ast.Ident:
		switch fn.Name {
		case "learn":
			if len(x.Args) != 1 {
				c.errorAt(fn.At, diag.CodeArgCount,
					fmt.Sprintf("learn 需要 1 个参数，实际 %d 个", len(x.Args)))
			}
			for _, a := range x.Args {
				c.exprType(a)
			}
			return "Unit"
		case "is_hazardous":
			if len(x.Args) != 1 {
				c.errorAt(fn.At, diag.CodeArgCount,
					fmt.Sprintf("is_hazardous 需要 1 个参数，实际 %d 个", len(x.Args)))
			}
			for _, a := range x.Args {
				c.exprType(a)
			}
			return "Bool"
		case "hash_of":
			if len(x.Args) != 1 {
				c.errorAt(fn.At, diag.CodeArgCount,
					fmt.Sprintf("hash_of 需要 1 个参数，实际 %d 个", len(x.Args)))
			}
			for _, a := range x.Args {
				c.exprType(a)
			}
			return "Int"
		}
		// 局部函数值
		if v, ok := c.lookupVar(fn.Name); ok {
			if v.Fn != nil {
				return c.checkCallArgs(fn.Name, v.Fn.Params, sigOf(v.Fn.Result), x)
			}
			if v.Type == "?" {
				for _, a := range x.Args {
					c.exprType(a)
				}
				return "?"
			}
			c.errorAt(fn.At, diag.CodeNotCallable, v.Type+" 不可调用")
			return "?"
		}
		// 顶层函数
		if fd, ok := c.sym.fns[fn.Name]; ok {
			params := make([]ast.Type, len(fd.Params))
			for i, p := range fd.Params {
				params[i] = p.Type
			}
			return c.checkCallArgs(fd.Name, params, sigOf(fd.Result), x)
		}
		c.errorAt(fn.At, diag.CodeUnknownFn, "未知函数 "+fn.Name)
		for _, a := range x.Args {
			c.exprType(a)
		}
		return "?"

	case *ast.QualifiedRef:
		base := fn.Parts[0]
		if v, ok := c.lookupVar(base); ok && len(fn.Parts) == 2 {
			method := fn.Parts[1]
			if caps, ok := c.sym.caps[v.Type]; ok {
				if sig, ok := caps[method]; ok {
					params := make([]ast.Type, len(sig.Params))
					for i, p := range sig.Params {
						params[i] = p.Type
					}
					return c.checkCallArgs(base+"."+method, params, sigOf(sig.Result), x)
				}
			}
			c.errorAt(fn.At, diag.CodeUnknownMethod,
				fmt.Sprintf("%s 没有 capability 方法 %s", v.Type, method))
			for _, a := range x.Args {
				c.exprType(a)
			}
			return "?"
		}
		c.exprType(x.Fn)
		for _, a := range x.Args {
			c.exprType(a)
		}
		return "?"
	}

	c.exprType(x.Fn)
	for _, a := range x.Args {
		c.exprType(a)
	}
	return "?"
}

// checkCallArgs：规则 10（参数个数 + 参数类型）。
func (c *Checker) checkCallArgs(name string, params []ast.Type, result string, x *ast.CallExpr) string {
	if len(x.Args) != len(params) {
		c.errorAt(x.At, diag.CodeArgCount,
			fmt.Sprintf("%s 需要 %d 个参数，实际 %d 个", name, len(params), len(x.Args)))
	}
	for i, a := range x.Args {
		at := c.exprType(a)
		if i >= len(params) {
			continue
		}
		want := sigOf(params[i])
		if !compatible(want, at) {
			c.errorAt(exprPosOf(a), diag.CodeArgType,
				fmt.Sprintf("%s 第 %d 个参数需要 %s，实际是 %s", name, i+1, want, at))
		}
	}
	return result
}

// ---------------- 纯函数（规则 35 / 39） ----------------

func (c *Checker) fnPure(name string) bool {
	switch c.purityState[name] {
	case pureYes:
		return true
	case pureNo:
		return false
	case pureBusy:
		// 递归调用：保守视为不纯，避免无限递归
		return false
	}
	fd, ok := c.sym.fns[name]
	if !ok {
		return false // 未知函数视为外部调用
	}
	c.purityState[name] = pureBusy
	locals := map[string]bool{}
	for _, p := range fd.Params {
		locals[p.Name] = true
	}
	pure := c.stmtsPure(fd.Body.Stmts, locals)
	if pure {
		c.purityState[name] = pureYes
	} else {
		c.purityState[name] = pureNo
	}
	return pure
}

func (c *Checker) stmtsPure(stmts []ast.Stmt, locals map[string]bool) bool {
	for _, s := range stmts {
		if !c.stmtPure(s, locals) {
			return false
		}
	}
	return true
}

func (c *Checker) stmtPure(s ast.Stmt, locals map[string]bool) bool {
	switch x := s.(type) {
	case *ast.EmitStmt:
		return false // emit 是副作用（规则 35）
	case *ast.LearnStmt:
		return false // learn 是日志副作用（规则 35 / A-P0-5）
	case *ast.InvokeStmt, *ast.ReuseStmt:
		// invoke/reuse 会触发外部逻辑执行
		return false
	case *ast.LetStmt:
		if !c.exprPure(x.Value, locals) {
			return false
		}
		locals[x.Name] = true
		return true
	case *ast.AssignStmt:
		// 规则 39：fn 内局部 mut 变量赋值不算副作用
		switch t := x.Target.(type) {
		case *ast.Ident:
			if locals[t.Name] {
				return c.exprPure(x.Value, locals)
			}
			return false // 全局状态
		case *ast.QualifiedRef:
			if t.Parts[0] == "self" {
				return false // 修改 self
			}
			if locals[t.Parts[0]] {
				return c.exprPure(x.Value, locals)
			}
			return false
		default:
			return false
		}
	case *ast.ExprStmt:
		return c.exprPure(x.X, locals)
	case *ast.DoStmt:
		return c.stmtsPure(x.Body.Stmts, locals)
	case *ast.ForStmt:
		return c.exprPure(x.Iter, locals) && c.stmtsPure(x.Body.Stmts, locals)
	case *ast.IfStmt:
		if !c.exprPure(x.Cond, locals) {
			return false
		}
		if !c.stmtsPure(x.Then.Stmts, locals) {
			return false
		}
		if x.Else != nil && !c.stmtPure(x.Else, locals) {
			return false
		}
		return true
	case *ast.MatchStmt:
		if !c.exprPure(x.Subject, locals) {
			return false
		}
		for _, a := range x.Arms {
			if !c.stmtPure(a.Body, locals) {
				return false
			}
		}
		return true
	case *ast.ReturnStmt:
		return x.Value == nil || c.exprPure(x.Value, locals)
	case *ast.Block:
		return c.stmtsPure(x.Stmts, locals)
	}
	return true
}

func (c *Checker) exprPure(e ast.Expr, locals map[string]bool) bool {
	switch x := e.(type) {
	case nil:
		return true
	case *ast.CallExpr:
		switch fn := x.Fn.(type) {
		case *ast.Ident:
			if fn.Name == "learn" {
				return false
			}
			if !pureBuiltins[fn.Name] && !locals[fn.Name] {
				if _, ok := c.sym.fns[fn.Name]; ok {
					if !c.fnPure(fn.Name) {
						return false
					}
				} else {
					return false // 外部调用（白名单外）
				}
			}
		case *ast.QualifiedRef:
			// 方法调用：视为外部调用（白名单外）
			return false
		}
		for _, a := range x.Args {
			if !c.exprPure(a, locals) {
				return false
			}
		}
		return true
	case *ast.BinaryExpr:
		return c.exprPure(x.L, locals) && c.exprPure(x.R, locals)
	case *ast.UnaryExpr:
		return c.exprPure(x.X, locals)
	case *ast.ParenExpr:
		return c.exprPure(x.X, locals)
	case *ast.RangeExpr:
		return c.exprPure(x.Lo, locals) && c.exprPure(x.Hi, locals)
	case *ast.FieldExpr:
		return c.exprPure(x.X, locals)
	case *ast.UnitExpr:
		return c.exprPure(x.Value, locals)
	}
	return true
}
