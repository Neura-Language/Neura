package runtime

import (
	"context"
	"fmt"
	"strings"

	"neura/internal/ast"
	"neura/internal/event"
)

// signalValue 保留编译期声明的 Signal<T> 内层类型。
// runtime.Value 的 TypeName 只有数值宽度合并后的族名；跨节点/事件帧需要精确类型时用这个包装。
type signalValue struct {
	Value Value
	Type  string
}

func (s signalValue) TypeName() string { return s.Type }
func (s signalValue) Canon() string    { return s.Value.Canon() }

func asValue(d event.Datum) Value {
	switch x := d.(type) {
	case Value:
		return x
	case signalValue:
		return x.Value
	case *signalValue:
		if x != nil {
			return x.Value
		}
	}
	return Unit()
}

// BuildEventLoop 从 AST 构建事件循环：
//   - 每个 neuron → 一个 actor（端口类型来自 Signal<T> 的内层 T）
//   - 每条 synapse → 一条有向路由（两端类型必须一致）
//
// 返回值未 Start；调用方决定策略（Capacity/Policy/EmitTimeout/MaxHops）。
func (p *Program) BuildEventLoop(cfg event.Config) (*event.Loop, error) {
	loop := event.New(cfg)
	for _, d := range p.file.Decls {
		nd, ok := d.(*ast.NeuronDecl)
		if !ok {
			continue
		}
		ports := make([]event.Port, 0, len(nd.Ports))
		for _, pt := range nd.Ports {
			ports = append(ports, event.Port{
				Name: pt.Name,
				Type: signalInnerType(pt.Type),
				Out:  pt.Dir == "output",
			})
		}
		if err := loop.AddNeuron(nd.Name, ports, p.eventHandler(nd)); err != nil {
			return nil, err
		}
	}
	for _, d := range p.file.Decls {
		sd, ok := d.(*ast.SynapseDecl)
		if !ok {
			continue
		}
		if len(sd.Src.Parts) != 2 || len(sd.Dst.Parts) != 2 {
			return nil, fmt.Errorf("synapse 两端必须是 Neuron.port")
		}
		from := event.Endpoint{Neuron: sd.Src.Parts[0], Port: sd.Src.Parts[1]}
		to := event.Endpoint{Neuron: sd.Dst.Parts[0], Port: sd.Dst.Parts[1]}
		if err := loop.Connect(from, to); err != nil {
			return nil, fmt.Errorf("synapse %s -> %s: %w", from, to, err)
		}
	}
	return loop, nil
}

// eventHandler 把一个 neuron 的 on signal / legacy handle 包装成 actor handler。
func (p *Program) eventHandler(nd *ast.NeuronDecl) event.Handler {
	return func(ctx context.Context, s event.Signal, tx *event.Tx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				if re, ok := r.(runtimeError); ok {
					err = re
					return
				}
				err = fmt.Errorf("neuron %s handler panic: %v", nd.Name, r)
			}
		}()
		body, sh := selectSignal(nd, s)
		if body == nil {
			return nil // 纯 sink neuron：没有 handler 不是错误
		}
		in := &interp{
			p:        p,
			global:   newEnv(nil),
			traceID:  s.TraceID,
			depth:    int(s.Hops),
			out:      tx,
			outPorts: nd.Ports,
		}
		e := newEnv(nil)
		for _, port := range nd.Ports {
			if port.Dir != "input" {
				continue
			}
			v := Unit()
			if port.Name == s.Port {
				v = asValue(s.Datum)
			}
			e.define(port.Name, v)
		}
		if sh != nil && sh.Name != "" {
			e.define(sh.Name, asValue(s.Datum))
		}
		in.execFnBody(body, e)
		return nil
	}
}

// selectSignal 按 输入端口 == From/Name > 类型匹配 > legacy handle 选处理器。
func selectSignal(nd *ast.NeuronDecl, s event.Signal) (*ast.Block, *ast.SignalHandler) {
	for _, sh := range nd.Signals {
		if sh.From == s.Port || sh.Name == s.Port {
			return sh.Body, sh
		}
	}
	for _, sh := range nd.Signals {
		if signalInnerType(sh.Type) == s.Type {
			return sh.Body, sh
		}
	}
	if nd.Handler != nil {
		return nd.Handler, nil
	}
	return nil, nil
}

// Inject 是运行时外部入口：把一个强类型 Value 投递给 neuron 的输入端口。
// 端口不存在/类型不符会在入队前被拒绝。
func (p *Program) Inject(ctx context.Context, loop *event.Loop, neuron, port string, v Value, traceID string) error {
	nd := p.neuron(neuron)
	if nd == nil {
		return fmt.Errorf("neuron %q 不存在", neuron)
	}
	for _, pt := range nd.Ports {
		if pt.Name != port {
			continue
		}
		if pt.Dir != "input" {
			return fmt.Errorf("%s.%s 不是输入端口", neuron, port)
		}
		tag := signalInnerType(pt.Type)
		if !valueConforms(v, tag) {
			return fmt.Errorf("%s.%s 需要 %s，实际 %s", neuron, port, tag, v.TypeName())
		}
		return loop.Emit(ctx, event.Endpoint{Neuron: neuron, Port: port}, signalValue{Value: v, Type: tag}, traceID)
	}
	return fmt.Errorf("%s 没有端口 %s", neuron, port)
}

func (p *Program) neuron(name string) *ast.NeuronDecl {
	for _, d := range p.file.Decls {
		if nd, ok := d.(*ast.NeuronDecl); ok && nd.Name == name {
			return nd
		}
	}
	return nil
}

// ---------------- interp 的 emit 绑定 ----------------

// emitExplicit 支持 `emit out(value)`：parser 把 out(value) 解析成 CallExpr，
// 这里在求值前截获，避免被当成未知函数调用。
func (in *interp) emitExplicit(x *ast.EmitStmt, e *env) (Value, bool) {
	if in.out == nil {
		return Unit(), false
	}
	call, ok := x.X.(*ast.CallExpr)
	if !ok {
		return Unit(), false
	}
	id, ok := call.Fn.(*ast.Ident)
	if !ok {
		return Unit(), false
	}
	if _, isOut := in.outPortType(id.Name); !isOut {
		return Unit(), false
	}
	if len(call.Args) != 1 {
		in.p.warnf("emit %s(...) 需要且仅需要 1 个参数", id.Name)
		return Unit(), true
	}
	v := in.eval(call.Args[0], e)
	in.p.addOutput("emit " + v.String())
	in.emitTo(id.Name, v)
	return v, true
}

// emitDefault 处理 `emit <expr>`：
//   - neuron 只有一个输出端口 → 发往该端口；
//   - 表达式本身是输出端口标识符 → 发往该端口；
//   - 其余情况 warn（不静默丢事件）。
func (in *interp) emitDefault(x ast.Expr, v Value) {
	ports := in.outputPorts()
	if len(ports) == 1 {
		in.emitTo(ports[0].Name, v)
		return
	}
	if id, ok := x.(*ast.Ident); ok {
		if _, isOut := in.outPortType(id.Name); isOut {
			in.emitTo(id.Name, v)
			return
		}
	}
	if len(ports) == 0 {
		in.p.warnf("emit 出现在没有输出端口的 neuron 中：事件被忽略")
		return
	}
	in.p.warnf("emit 目标不明确（neuron 有 %d 个输出端口），请用 emit <port>(<value>)", len(ports))
}

func (in *interp) emitTo(port string, v Value) {
	if in.out == nil {
		return
	}
	tag, ok := in.outPortType(port)
	if !ok {
		in.p.warnf("emit 目标 %s 不是输出端口", port)
		return
	}
	if !valueConforms(v, tag) {
		in.p.warnf("emit %s 类型不符：需要 %s，实际 %s", port, tag, v.TypeName())
		return
	}
	if err := in.out.Emit(port, signalValue{Value: v, Type: tag}); err != nil {
		in.p.warnf("emit %s: %v", port, err)
	}
}

func (in *interp) outPortType(name string) (string, bool) {
	for _, p := range in.outPorts {
		if p.Name == name && p.Dir == "output" {
			return signalInnerType(p.Type), true
		}
	}
	return "", false
}

func (in *interp) outputPorts() []*ast.Port {
	out := make([]*ast.Port, 0, len(in.outPorts))
	for _, p := range in.outPorts {
		if p.Dir == "output" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------- 类型工具 ----------------

// signalInnerType 取 Signal<T> 的 T；非 Signal 类型原样返回（兼容旧样例）。
func signalInnerType(t ast.Type) string {
	if nt, ok := t.(*ast.NamedType); ok && nt.Name == "Signal" && len(nt.Args) == 1 {
		return typeString(nt.Args[0])
	}
	return typeString(t)
}

func typeString(t ast.Type) string {
	switch x := t.(type) {
	case *ast.NamedType:
		if len(x.Args) == 0 {
			return x.Name
		}
		args := make([]string, len(x.Args))
		for i, a := range x.Args {
			args[i] = typeString(a)
		}
		return x.Name + "<" + strings.Join(args, ", ") + ">"
	case *ast.TupleType:
		parts := make([]string, len(x.Elems))
		for i, el := range x.Elems {
			parts[i] = typeString(el)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case *ast.FnType:
		parts := make([]string, len(x.Params))
		for i, p := range x.Params {
			parts[i] = typeString(p)
		}
		return "fn(" + strings.Join(parts, ", ") + ") -> " + typeString(x.Result)
	}
	return "?"
}

// valueConforms 是编译期类型检查的运行时复核（防远端注入/emit 目标未检查的缺口）。
func valueConforms(v Value, tag string) bool {
	switch tag {
	case "Int", "Int32", "Int64":
		return v.Kind == KInt
	case "Float", "F32", "F64":
		return v.Kind == KFloat || v.Kind == KInt
	case "Bool":
		return v.Kind == KBool
	case "String":
		return v.Kind == KString
	case "Size":
		return v.Kind == KSize
	case "Duration":
		return v.Kind == KDuration
	case "Unit":
		return v.Kind == KUnit
	}
	if strings.HasPrefix(tag, "Option<") {
		return v.Kind == KUnit || valueConforms(v, strings.TrimSuffix(strings.TrimPrefix(tag, "Option<"), ">"))
	}
	return v.Kind == KInstance && v.Inst != nil && v.Inst.Logic == tag
}
