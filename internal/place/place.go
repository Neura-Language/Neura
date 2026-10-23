// Package place 把 `place ... where ...` 编译成节点标记匹配，并提供可替换的调度 Adapter。
//
// 设计目标：
//   - 默认实现只用标签/指标匹配 + 确定性排序，零第三方依赖；
//   - Adapter 只管「给我当前节点快照」和「按约束排序」，不绑定 K8s；
//   - K8s 通过 NodeSource 适配（List/Watch），未来用 client-go 实现 source 即可，
//     不需要改 place 包，也不会把 client-go 拖进主二进制。
package place

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"neura/internal/ast"
)

// NodeInfo 是可调度节点快照。Labels 做等值/包含匹配，Metrics 做数值约束。
type NodeInfo struct {
	Name     string             `json:"name"`
	Ready    bool               `json:"ready"`
	Labels   map[string]string  `json:"labels,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	Capacity int                `json:"capacity,omitempty"`
	Load     float64            `json:"load,omitempty"`
}

// Request 是一次 place 求解输入。
type Request struct {
	Target  string            // place 目标名（instance / logic / neuron）
	Where   Selector          // 已编译的 where；nil 表示不过滤
	Labels  map[string]string // K8s 风格精确标签匹配（额外约束）
	Vars    map[string]any    // 实例字段值（where 可引用），优先级高于节点指标
	At      []string          // 显式节点集合；非空时顺序即偏好顺序
	Balance string            // "" / "round_robin" / "consistent_hash"
	HashKey string            // consistent_hash 的键（实例字段规范值）
	Seq     uint64            // 单调序号，round_robin 用它做确定性轮转
	Now     float64           // where 的 now；0 表示用 Adapter 时钟
	Self    string            // 本节点地址（供上层判断 local）
}

// Adapter 是调度抽象。Match 返回按 failover 优先级排列的节点名。
type Adapter interface {
	Name() string
	Nodes(ctx context.Context) ([]NodeInfo, error)
	Match(ctx context.Context, req Request) ([]string, error)
}

// Selector 是 where 的运行时形态。Compiled 实现它。
type Selector interface {
	Match(n NodeInfo, vars map[string]any, now float64) (bool, error)
}

// 调度错误。
var (
	ErrNoNode       = errors.New("place: no node matches constraints")
	ErrBadWhere     = errors.New("place: unsupported where expression")
	ErrBadBalance   = errors.New("place: unknown balance policy")
	ErrNotSupported = errors.New("place: not supported")
)

// ---------------- where 编译器 ----------------

// Compiled 是 where 表达式的可复用求值器。
type Compiled struct {
	src  string
	deps []string
	eval func(n NodeInfo, vars map[string]any, now float64) (bool, error)
}

// Compile 把 where AST 编译成纯函数。只接受编译器已放行的纯表达式：
// 比较、&&/||/!、加减乘除、字面量、标识符、has_label/has_metric。
func Compile(e ast.Expr) (*Compiled, error) {
	if e == nil {
		return nil, nil
	}
	c := &compiler{deps: map[string]bool{}}
	fn, err := c.expr(e)
	if err != nil {
		return nil, err
	}
	deps := make([]string, 0, len(c.deps))
	for d := range c.deps {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return &Compiled{src: render(e), deps: deps, eval: fn}, nil
}

// Match 实现 Selector。
func (c *Compiled) Match(n NodeInfo, vars map[string]any, now float64) (bool, error) {
	if c == nil {
		return true, nil
	}
	return c.eval(n, vars, now)
}

// Deps 返回 where 引用到的标识符（诊断用）。
func (c *Compiled) Deps() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.deps...)
}

func (c *Compiled) String() string {
	if c == nil {
		return ""
	}
	return c.src
}

// Unknown 返回在给定节点集合/实例变量中都找不到的依赖（拼写错误诊断）。
func (c *Compiled) Unknown(nodes []NodeInfo, vars map[string]any) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, d := range c.deps {
		if d == "now" || d == "true" || d == "false" {
			continue
		}
		if _, ok := vars[d]; ok {
			continue
		}
		found := false
		for _, n := range nodes {
			if _, ok := n.Metrics[d]; ok {
				found = true
				break
			}
			if _, ok := n.Labels[d]; ok {
				found = true
				break
			}
		}
		if !found {
			out = append(out, d)
		}
	}
	return out
}

// val 是 where 求值的三类值：number/string/bool。
type val struct {
	kind byte // 'n' number, 's' string, 'b' bool
	n    float64
	s    string
	b    bool
}

func num(f float64) val  { return val{kind: 'n', n: f} }
func str(s string) val   { return val{kind: 's', s: s} }
func boolean(b bool) val { return val{kind: 'b', b: b} }

func (v val) canon() string {
	switch v.kind {
	case 'n':
		return strconv.FormatFloat(v.n, 'g', -1, 64)
	case 'b':
		if v.b {
			return "true"
		}
		return "false"
	default:
		return v.s
	}
}

type compiler struct{ deps map[string]bool }

func (c *compiler) expr(e ast.Expr) (func(NodeInfo, map[string]any, float64) (bool, error), error) {
	vf, err := c.value(e)
	if err != nil {
		return nil, err
	}
	return func(n NodeInfo, vars map[string]any, now float64) (bool, error) {
		v, err := vf(n, vars, now)
		if err != nil {
			return false, err
		}
		if v.kind != 'b' {
			return false, fmt.Errorf("%w: where 结果不是 Bool", ErrBadWhere)
		}
		return v.b, nil
	}, nil
}

func (c *compiler) value(e ast.Expr) (func(NodeInfo, map[string]any, float64) (val, error), error) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return c.value(x.X)
	case *ast.Literal:
		return c.literal(x)
	case *ast.Ident:
		name := x.Name
		c.deps[name] = true
		return func(n NodeInfo, vars map[string]any, now float64) (val, error) {
			if vars != nil {
				if v, ok := vars[name]; ok {
					return anyVal(v), nil
				}
			}
			if name == "now" {
				return num(now), nil
			}
			if v, ok := n.Metrics[name]; ok {
				return num(v), nil
			}
			if v, ok := n.Labels[name]; ok {
				return str(v), nil
			}
			return val{}, fmt.Errorf("%w: 未知标识符 %s", ErrBadWhere, name)
		}, nil
	case *ast.QualifiedRef:
		if len(x.Parts) != 2 || x.Parts[0] != "node" {
			return nil, fmt.Errorf("%w: 只支持 node.<label>", ErrBadWhere)
		}
		name := x.Parts[1]
		c.deps[name] = true
		return func(n NodeInfo, _ map[string]any, _ float64) (val, error) {
			if v, ok := n.Metrics[name]; ok {
				return num(v), nil
			}
			if v, ok := n.Labels[name]; ok {
				return str(v), nil
			}
			return val{}, fmt.Errorf("%w: 未知节点属性 %s", ErrBadWhere, name)
		}, nil
	case *ast.UnaryExpr:
		inner, err := c.value(x.X)
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case "!":
			return func(n NodeInfo, vars map[string]any, now float64) (val, error) {
				v, err := inner(n, vars, now)
				if err != nil {
					return val{}, err
				}
				return boolean(!truthy(v)), nil
			}, nil
		case "-":
			return func(n NodeInfo, vars map[string]any, now float64) (val, error) {
				v, err := inner(n, vars, now)
				if err != nil || v.kind != 'n' {
					return val{}, fmt.Errorf("%w: 一元 - 需要数字", ErrBadWhere)
				}
				return num(-v.n), nil
			}, nil
		default:
			return nil, fmt.Errorf("%w: 一元运算符 %s", ErrBadWhere, x.Op)
		}
	case *ast.BinaryExpr:
		switch x.Op {
		case "&&", "||":
			l, err := c.expr(x.L)
			if err != nil {
				return nil, err
			}
			r, err := c.expr(x.R)
			if err != nil {
				return nil, err
			}
			return func(n NodeInfo, vars map[string]any, now float64) (val, error) {
				lv, err := l(n, vars, now)
				if err != nil {
					return val{}, err
				}
				if x.Op == "&&" && !lv {
					return boolean(false), nil
				}
				if x.Op == "||" && lv {
					return boolean(true), nil
				}
				rv, err := r(n, vars, now)
				if err != nil {
					return val{}, err
				}
				return boolean(rv), nil
			}, nil
		default:
			lv, err := c.value(x.L)
			if err != nil {
				return nil, err
			}
			rv, err := c.value(x.R)
			if err != nil {
				return nil, err
			}
			op := x.Op
			return func(n NodeInfo, vars map[string]any, now float64) (val, error) {
				l, err := lv(n, vars, now)
				if err != nil {
					return val{}, err
				}
				r, err := rv(n, vars, now)
				if err != nil {
					return val{}, err
				}
				return cmp(op, l, r)
			}, nil
		}
	case *ast.CallExpr:
		id, ok := x.Fn.(*ast.Ident)
		if !ok || (id.Name != "has_label" && id.Name != "has_metric") {
			return nil, fmt.Errorf("%w: 只支持 has_label/has_metric", ErrBadWhere)
		}
		if len(x.Args) != 1 {
			return nil, fmt.Errorf("%w: %s 需要 1 个参数", ErrBadWhere, id.Name)
		}
		lit, ok := x.Args[0].(*ast.Literal)
		if !ok || lit.Kind != "string" {
			return nil, fmt.Errorf("%w: %s 参数必须是字符串字面量", ErrBadWhere, id.Name)
		}
		name := unquote(lit.Text)
		c.deps[name] = true
		wantMetric := id.Name == "has_metric"
		return func(n NodeInfo, _ map[string]any, _ float64) (val, error) {
			if wantMetric {
				_, ok := n.Metrics[name]
				return boolean(ok), nil
			}
			_, ok := n.Labels[name]
			return boolean(ok), nil
		}, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrBadWhere, e)
}

func (c *compiler) literal(l *ast.Literal) (func(NodeInfo, map[string]any, float64) (val, error), error) {
	switch l.Kind {
	case "int", "float":
		f, err := strconv.ParseFloat(l.Text, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: 数字 %q", ErrBadWhere, l.Text)
		}
		return func(NodeInfo, map[string]any, float64) (val, error) { return num(f), nil }, nil
	case "string":
		s := unquote(l.Text)
		return func(NodeInfo, map[string]any, float64) (val, error) { return str(s), nil }, nil
	case "bool":
		b := l.Text == "true"
		return func(NodeInfo, map[string]any, float64) (val, error) { return boolean(b), nil }, nil
	}
	return nil, fmt.Errorf("%w: 字面量 %s", ErrBadWhere, l.Kind)
}

func cmp(op string, l, r val) (val, error) {
	if op == "==" || op == "!=" {
		eq := l.canon() == r.canon()
		if l.kind == 'n' && r.kind == 'n' {
			eq = l.n == r.n
		}
		if op == "==" {
			return boolean(eq), nil
		}
		return boolean(!eq), nil
	}
	if op == "~=" {
		if l.kind != 's' || r.kind != 's' {
			return val{}, fmt.Errorf("%w: ~= 需要字符串", ErrBadWhere)
		}
		return boolean(strings.Contains(l.s, r.s)), nil
	}
	if l.kind == 's' && r.kind == 's' {
		switch op {
		case "<":
			return boolean(l.s < r.s), nil
		case "<=":
			return boolean(l.s <= r.s), nil
		case ">":
			return boolean(l.s > r.s), nil
		case ">=":
			return boolean(l.s >= r.s), nil
		}
	}
	if l.kind != 'n' || r.kind != 'n' {
		return val{}, fmt.Errorf("%w: %s 两侧类型不可比较", ErrBadWhere, op)
	}
	switch op {
	case "<":
		return boolean(l.n < r.n), nil
	case "<=":
		return boolean(l.n <= r.n), nil
	case ">":
		return boolean(l.n > r.n), nil
	case ">=":
		return boolean(l.n >= r.n), nil
	case "+":
		return num(l.n + r.n), nil
	case "-":
		return num(l.n - r.n), nil
	case "*":
		return num(l.n * r.n), nil
	case "/":
		if r.n == 0 {
			return val{}, fmt.Errorf("%w: where 除以 0", ErrBadWhere)
		}
		return num(l.n / r.n), nil
	}
	return val{}, fmt.Errorf("%w: 运算符 %s", ErrBadWhere, op)
}

func truthy(v val) bool {
	switch v.kind {
	case 'b':
		return v.b
	case 'n':
		return v.n != 0
	default:
		return v.s != ""
	}
}

func anyVal(v any) val {
	switch x := v.(type) {
	case nil:
		return str("")
	case string:
		return str(x)
	case bool:
		return boolean(x)
	case int:
		return num(float64(x))
	case int64:
		return num(float64(x))
	case int32:
		return num(float64(x))
	case float64:
		return num(x)
	case float32:
		return num(float64(x))
	default:
		return str(fmt.Sprint(x))
	}
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

func render(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.Literal:
		return x.Text
	case *ast.QualifiedRef:
		return strings.Join(x.Parts, ".")
	case *ast.ParenExpr:
		return "(" + render(x.X) + ")"
	case *ast.UnaryExpr:
		return x.Op + render(x.X)
	case *ast.BinaryExpr:
		return render(x.L) + " " + x.Op + " " + render(x.R)
	case *ast.CallExpr:
		return render(x.Fn) + "(...)"
	default:
		return "<where>"
	}
}

// ---------------- LabelAdapter ----------------

// LabelAdapter 是最简节点打标匹配器：标签 + 指标 + 确定性排序。
type LabelAdapter struct {
	mu    sync.RWMutex
	nodes map[string]NodeInfo
	clock func() time.Time
}

func NewLabelAdapter(nodes ...NodeInfo) *LabelAdapter {
	a := &LabelAdapter{nodes: map[string]NodeInfo{}, clock: time.Now}
	a.SetNodes(nodes)
	return a
}

// SetClock 注入时钟（where 里的 now）。
func (a *LabelAdapter) SetClock(now func() time.Time) {
	a.mu.Lock()
	a.clock = now
	a.mu.Unlock()
}

// SetNodes 整体热替换节点快照。
func (a *LabelAdapter) SetNodes(nodes []NodeInfo) {
	m := make(map[string]NodeInfo, len(nodes))
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		n.Labels = cloneMap(n.Labels)
		n.Metrics = cloneMapF(n.Metrics)
		m[n.Name] = n
	}
	a.mu.Lock()
	a.nodes = m
	a.mu.Unlock()
}

// Upsert 增量更新（K8s watch 事件用）。
func (a *LabelAdapter) Upsert(n NodeInfo) {
	if n.Name == "" {
		return
	}
	n.Labels = cloneMap(n.Labels)
	n.Metrics = cloneMapF(n.Metrics)
	a.mu.Lock()
	if a.nodes == nil {
		a.nodes = map[string]NodeInfo{}
	}
	a.nodes[n.Name] = n
	a.mu.Unlock()
}

// Delete 删除节点。
func (a *LabelAdapter) Delete(name string) {
	a.mu.Lock()
	delete(a.nodes, name)
	a.mu.Unlock()
}

func (a *LabelAdapter) Name() string { return "labels" }

// Nodes 返回按名字升序的节点快照。
func (a *LabelAdapter) Nodes(context.Context) ([]NodeInfo, error) {
	a.mu.RLock()
	out := make([]NodeInfo, 0, len(a.nodes))
	for _, n := range a.nodes {
		out = append(out, cloneNode(n))
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Match 实现 Adapter：Ready → Labels → At → where → balance。
func (a *LabelAdapter) Match(ctx context.Context, req Request) ([]string, error) {
	_ = ctx
	now := req.Now
	if now == 0 {
		a.mu.RLock()
		clk := a.clock
		a.mu.RUnlock()
		if clk != nil {
			now = float64(clk().Unix())
		}
	}
	nodes, err := a.Nodes(context.Background())
	if err != nil {
		return nil, err
	}

	at := map[string]bool{}
	for _, n := range req.At {
		at[n] = true
	}
	candidates := make([]NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		if !n.Ready {
			continue
		}
		if len(at) > 0 && !at[n.Name] {
			continue
		}
		if !labelsMatch(n.Labels, req.Labels) {
			continue
		}
		if req.Where != nil {
			ok, err := req.Where.Match(n, req.Vars, now)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		candidates = append(candidates, n)
	}

	// At 显式顺序优先：按 At 顺序排列。
	if len(req.At) > 0 {
		idx := map[string]int{}
		for i, name := range req.At {
			if _, ok := idx[name]; !ok {
				idx[name] = i
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool { return idx[candidates[i].Name] < idx[candidates[j].Name] })
	}

	switch req.Balance {
	case "", "none":
		if len(req.At) == 0 {
			sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
		}
	case "round_robin":
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
		if len(candidates) > 0 {
			shift := int(req.Seq % uint64(len(candidates)))
			candidates = append(candidates[shift:], candidates[:shift]...)
		}
	case "consistent_hash":
		if req.HashKey == "" {
			return nil, fmt.Errorf("%w: consistent_hash 需要 HashKey", ErrBadBalance)
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			hi := rendezvous(req.HashKey, candidates[i].Name)
			hj := rendezvous(req.HashKey, candidates[j].Name)
			if hi != hj {
				return hi > hj
			}
			return candidates[i].Name < candidates[j].Name
		})
	default:
		return nil, fmt.Errorf("%w: %s", ErrBadBalance, req.Balance)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: target=%s where=%s", ErrNoNode, req.Target, req.Where)
	}
	out := make([]string, len(candidates))
	for i, n := range candidates {
		out[i] = n.Name
	}
	return out, nil
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// rendezvous（最高随机权重）是一致哈希的无环实现：
// 节点增减只影响该键在冲突节点间的重排，不需要维护虚拟环。
func rendezvous(key, node string) uint64 {
	return fnv1a(key + "\x00" + node)
}

func fnv1a(s string) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneMapF(m map[string]float64) map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneNode(n NodeInfo) NodeInfo {
	n.Labels = cloneMap(n.Labels)
	n.Metrics = cloneMapF(n.Metrics)
	return n
}
