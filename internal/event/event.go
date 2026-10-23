// Package event 是 Neura 的异步 Signal 事件循环。
//
// 模型：每个 neuron 是一个 actor（拥有独立的有界 mailbox + 单 goroutine），
// synapse 是 actor 之间的静态路由边。emit 在 handler 内只写本地 outbox（永不阻塞），
// handler 返回后由 actor 统一把 outbox 下刷到下游 mailbox；下刷阶段才施加背压。
// 这样既保证「下游不会被无限压垮」，也避免 handler 在持锁/占用自身 mailbox 时阻塞死锁。
//
// 强类型：Port.Type 是 Signal<T> 的内层 T（由 checker 从 AST 推导），
// 路由建立时校验两端类型相等，投递时再次校验 Datum.TypeName() == Port.Type。
// 编译器是类型安全的第一道闸，运行时这里做防篡改/远端注入的第二道闸。
package event

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Datum 是事件载荷的最小契约。runtime.Value 直接实现它；
// 需要保留静态类型时用 runtime 的 signalValue 包装（TypeName 返回声明类型）。
type Datum interface {
	TypeName() string // 强类型标签，例如 "Int" / "Option<Int>" / "Signal<Int>"
	Canon() string    // 确定性规范化文本，用于 trace/哈希
}

// Port 是 neuron 的一个端口。Type 是 Signal<T> 的内层 T。
type Port struct {
	Name string
	Type string
	Out  bool
}

// Endpoint 是 Neuron.port。
type Endpoint struct {
	Neuron string
	Port   string
}

func (e Endpoint) String() string {
	if e.Neuron == "" {
		return e.Port
	}
	return e.Neuron + "." + e.Port
}

// Signal 是投递到某个输入端口的事件包。不含时间戳：trace 可复现。
type Signal struct {
	TraceID string
	Seq     uint64 // 全局逻辑序号（仅用于 trace 排序，不参与语义）
	Hops    uint16 // 已跨神经元跳数；用于斩断反馈环
	From    Endpoint
	Port    string // 目标输入端口
	Type    string // 静态类型标签
	Datum   Datum
}

// Handler 是 neuron 的串行执行体。tx.Emit 只记录、不阻塞。
// 返回 error 只影响本事件的统计与日志，不终止 loop。
type Handler func(ctx context.Context, s Signal, tx *Tx) error

// 错误集。
var (
	ErrClosed        = errors.New("event: loop closed")
	ErrBackpressure  = errors.New("event: mailbox full")
	ErrDupNeuron     = errors.New("event: duplicate neuron")
	ErrUnknownNeuron = errors.New("event: unknown neuron")
	ErrUnknownPort   = errors.New("event: unknown port")
	ErrBadPort       = errors.New("event: bad port spec")
	ErrTypeMismatch  = errors.New("event: signal type mismatch")
	ErrHopsExceeded  = errors.New("event: hop limit exceeded")
)

// Policy 是 mailbox 满时的背压策略。
type Policy int

const (
	// Block：等待 EmitTimeout（默认 50ms），超时后丢弃并计入 Dropped。
	// 默认策略；放在下刷阶段，handler 已经返回，不会造成自身 mailbox 死锁。
	Block Policy = iota
	// DropNewest：满了直接拒绝新事件（适合可丢的遥测/心跳信号）。
	DropNewest
	// DropOldest：挤掉队首最旧事件（适合保留最新状态的 sensor 流）。
	DropOldest
)

func (p Policy) String() string {
	switch p {
	case DropNewest:
		return "drop_newest"
	case DropOldest:
		return "drop_oldest"
	default:
		return "block"
	}
}

// Config 是事件循环的静态配置。
type Config struct {
	Capacity    int           // 每个 neuron 的 mailbox 容量，默认 64
	Policy      Policy        // 默认 Block
	EmitTimeout time.Duration // Block 的最长等待，默认 50ms
	MaxHops     uint16        // 反馈环上限，默认 64
	TraceLimit  int           // 保留最近 N 条 Signal 供测试/观测；0 表示关闭
	OnError     func(Signal, error)
}

func (c *Config) defaults() {
	if c.Capacity <= 0 {
		c.Capacity = 64
	}
	if c.EmitTimeout <= 0 {
		c.EmitTimeout = 50 * time.Millisecond
	}
	if c.MaxHops == 0 {
		c.MaxHops = 64
	}
}

// Loop 是事件循环。AddNeuron/Connect 必须在 Start 之前完成（启动后路由表只读）。
type Loop struct {
	cfg Config

	mu     sync.RWMutex
	actors map[string]*actor
	ports  map[string]map[string]Port
	routes map[Endpoint][]Endpoint

	seq      atomic.Uint64
	pending  atomic.Int64
	rejected atomic.Uint64

	traceMu sync.Mutex
	trail   []Signal

	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	started atomic.Bool
	wg      sync.WaitGroup
}

type actor struct {
	name string
	in   chan Signal
	h    Handler

	enqueued atomic.Uint64
	handled  atomic.Uint64
	failed   atomic.Uint64
	dropped  atomic.Uint64
	blocked  atomic.Uint64
	unrouted atomic.Uint64
	maxDepth atomic.Int64
}

// New 创建事件循环（未启动）。
func New(cfg Config) *Loop {
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Loop{
		cfg:    cfg,
		actors: map[string]*actor{},
		ports:  map[string]map[string]Port{},
		routes: map[Endpoint][]Endpoint{},
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// AddNeuron 注册 neuron 及其端口。h 为 nil 时该 neuron 是 sink（收到即丢弃）。
func (l *Loop) AddNeuron(name string, ports []Port, h Handler) error {
	if name == "" {
		return fmt.Errorf("%w: empty neuron name", ErrBadPort)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.actors[name]; ok {
		return fmt.Errorf("%w: %s", ErrDupNeuron, name)
	}
	pm := make(map[string]Port, len(ports))
	for _, p := range ports {
		if p.Name == "" || p.Type == "" {
			return fmt.Errorf("%w: %s.%s", ErrBadPort, name, p.Name)
		}
		if _, dup := pm[p.Name]; dup {
			return fmt.Errorf("%w: duplicate port %s.%s", ErrBadPort, name, p.Name)
		}
		pm[p.Name] = p
	}
	if h == nil {
		h = func(context.Context, Signal, *Tx) error { return nil }
	}
	l.ports[name] = pm
	l.actors[name] = &actor{
		name: name,
		in:   make(chan Signal, l.cfg.Capacity),
		h:    h,
	}
	return nil
}

// Connect 建立 synapse 路由：from 必须是输出端口，to 必须是输入端口，类型必须相等。
func (l *Loop) Connect(from, to Endpoint) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	fp, ok := l.portLocked(from)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPort, from)
	}
	tp, ok := l.portLocked(to)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPort, to)
	}
	if !fp.Out {
		return fmt.Errorf("%w: %s 不是输出端口", ErrBadPort, from)
	}
	if tp.Out {
		return fmt.Errorf("%w: %s 不是输入端口", ErrBadPort, to)
	}
	if fp.Type != tp.Type {
		return fmt.Errorf("%w: %s 是 %s，%s 是 %s", ErrTypeMismatch, from, fp.Type, to, tp.Type)
	}
	for _, e := range l.routes[from] {
		if e == to {
			return nil // 幂等
		}
	}
	l.routes[from] = append(l.routes[from], to)
	return nil
}

func (l *Loop) portLocked(e Endpoint) (Port, bool) {
	pm := l.ports[e.Neuron]
	if pm == nil {
		return Port{}, false
	}
	p, ok := pm[e.Port]
	return p, ok
}

// Start 为每个 neuron 启动一个 actor goroutine。重复调用是幂等的。
func (l *Loop) Start() {
	if !l.started.CompareAndSwap(false, true) {
		return
	}
	l.mu.RLock()
	actors := make([]*actor, 0, len(l.actors))
	for _, a := range l.actors {
		actors = append(actors, a)
	}
	l.mu.RUnlock()
	for _, a := range actors {
		l.wg.Add(1)
		go l.run(a)
	}
}

func (l *Loop) run(a *actor) {
	defer l.wg.Done()
	for {
		select {
		case <-l.done:
			return
		case s := <-a.in:
			l.dispatch(a, s)
		}
	}
}

// dispatch：先执行 handler（期间 tx.Emit 只入 outbox），返回后再统一下刷。
func (l *Loop) dispatch(a *actor, s Signal) {
	defer l.pending.Add(-1)

	if s.Hops > l.cfg.MaxHops {
		a.dropped.Add(1)
		l.report(s, ErrHopsExceeded)
		return
	}
	tx := &Tx{loop: l, from: a.name, hops: s.Hops, traceID: s.TraceID}
	a.handled.Add(1)
	if err := a.h(l.ctx, s, tx); err != nil {
		a.failed.Add(1)
		l.report(s, err)
	}
	for _, e := range tx.emits {
		l.route(a.name, s, e)
	}
}

// route 把 outbox 中的一次 emit 扇出到所有下游 mailbox。
func (l *Loop) route(from string, parent Signal, e Emit) {
	src := Endpoint{Neuron: from, Port: e.Port}
	l.mu.RLock()
	dsts := l.routes[src]
	long := len(dsts)
	l.mu.RUnlock()
	if long == 0 {
		if a := l.actor(from); a != nil {
			a.unrouted.Add(1)
		}
		return
	}
	hops := parent.Hops + 1
	if hops > l.cfg.MaxHops {
		l.report(parent, ErrHopsExceeded)
		return
	}
	for _, dst := range dsts {
		s := Signal{
			TraceID: parent.TraceID,
			Seq:     l.seq.Add(1),
			Hops:    hops,
			From:    src,
			Port:    dst.Port,
			Type:    e.Type,
			Datum:   e.Datum,
		}
		if err := l.push(dst, s); err != nil {
			l.report(s, err)
			continue
		}
		l.record(s)
	}
}

// push 是唯一施加背压的地方。调用点位于 handler 返回之后。
func (l *Loop) push(dst Endpoint, s Signal) error {
	a := l.actor(dst.Neuron)
	if a == nil {
		l.rejected.Add(1)
		return fmt.Errorf("%w: %s", ErrUnknownNeuron, dst.Neuron)
	}
	select {
	case <-l.done:
		return ErrClosed
	default:
	}

	push := func() bool {
		select {
		case a.in <- s:
			l.pending.Add(1)
			a.enqueued.Add(1)
			if d := int64(len(a.in)); d > a.maxDepth.Load() {
				a.maxDepth.Store(d)
			}
			return true
		case <-l.done:
			return false
		}
	}

	switch l.cfg.Policy {
	case DropNewest:
		select {
		case a.in <- s:
			l.pending.Add(1)
			a.enqueued.Add(1)
			if d := int64(len(a.in)); d > a.maxDepth.Load() {
				a.maxDepth.Store(d)
			}
			return nil
		default:
			a.dropped.Add(1)
			return ErrBackpressure
		}
	case DropOldest:
		select {
		case a.in <- s:
			l.pending.Add(1)
			a.enqueued.Add(1)
			return nil
		default:
			select {
			case old := <-a.in:
				l.pending.Add(-1) // 被挤掉的旧事件不再被处理
				a.dropped.Add(1)
				_ = old
			default:
			}
			if push() {
				return nil
			}
			return ErrClosed
		}
	default: // Block
		timer := time.NewTimer(l.cfg.EmitTimeout)
		defer timer.Stop()
		select {
		case a.in <- s:
			l.pending.Add(1)
			a.enqueued.Add(1)
			if d := int64(len(a.in)); d > a.maxDepth.Load() {
				a.maxDepth.Store(d)
			}
			return nil
		case <-timer.C:
			a.dropped.Add(1)
			a.blocked.Add(1)
			return ErrBackpressure
		case <-l.done:
			return ErrClosed
		}
	}
}

func (l *Loop) actor(name string) *actor {
	l.mu.RLock()
	a := l.actors[name]
	l.mu.RUnlock()
	return a
}

// Emit 是外部入口：类型由目标输入端口静态决定。
// 所有 ingress 必须显式给 traceID；空值表示独立事件（trace 仍是唯一 ID）。
func (l *Loop) Emit(ctx context.Context, dst Endpoint, d Datum, traceID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return ErrClosed
	default:
	}
	if d == nil {
		return fmt.Errorf("%w: nil datum", ErrTypeMismatch)
	}
	l.mu.RLock()
	p, ok := l.portLocked(dst)
	l.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPort, dst)
	}
	if p.Out {
		return fmt.Errorf("%w: %s 不是输入端口", ErrBadPort, dst)
	}
	if d.TypeName() != p.Type {
		l.rejected.Add(1)
		return fmt.Errorf("%w: %s 需要 %s，收到 %s", ErrTypeMismatch, dst, p.Type, d.TypeName())
	}
	s := Signal{
		TraceID: traceID,
		Seq:     l.seq.Add(1),
		Port:    dst.Port,
		Type:    p.Type,
		Datum:   d,
	}
	if err := l.push(dst, s); err != nil {
		return err
	}
	l.record(s)
	return nil
}

func (l *Loop) record(s Signal) {
	if l.cfg.TraceLimit <= 0 {
		return
	}
	l.traceMu.Lock()
	l.trail = append(l.trail, s)
	if over := len(l.trail) - l.cfg.TraceLimit; over > 0 {
		l.trail = append([]Signal(nil), l.trail[over:]...)
	}
	l.traceMu.Unlock()
}

// Trace 返回按 Seq 升序的事件轨迹副本。
func (l *Loop) Trace() []Signal {
	l.traceMu.Lock()
	defer l.traceMu.Unlock()
	out := append([]Signal(nil), l.trail...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Seq < out[j-1].Seq; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (l *Loop) report(s Signal, err error) {
	if l.cfg.OnError != nil {
		l.cfg.OnError(s, err)
	}
}

// WaitIdle 等待所有在途事件（含 outbox 下刷）处理完。用于测试与优雅停机。
func (l *Loop) WaitIdle(ctx context.Context) error {
	t := time.NewTicker(time.Millisecond)
	defer t.Stop()
	for {
		if l.pending.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Pending 是在途事件数。
func (l *Loop) Pending() int64 { return l.pending.Load() }

// Stat 是单个 neuron 的观测快照。
type Stat struct {
	Enqueued uint64
	Handled  uint64
	Failed   uint64
	Dropped  uint64
	Blocked  uint64
	Unrouted uint64
	Depth    int
	MaxDepth int64
	Capacity int
}

// Stats 返回所有 neuron 的统计（按名字排序，便于日志稳定）。
func (l *Loop) Stats() map[string]Stat {
	l.mu.RLock()
	names := make([]string, 0, len(l.actors))
	for n := range l.actors {
		names = append(names, n)
	}
	l.mu.RUnlock()
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	out := make(map[string]Stat, len(names))
	for _, n := range names {
		a := l.actor(n)
		out[n] = Stat{
			Enqueued: a.enqueued.Load(),
			Handled:  a.handled.Load(),
			Failed:   a.failed.Load(),
			Dropped:  a.dropped.Load(),
			Blocked:  a.blocked.Load(),
			Unrouted: a.unrouted.Load(),
			Depth:    len(a.in),
			MaxDepth: a.maxDepth.Load(),
			Capacity: l.cfg.Capacity,
		}
	}
	return out
}

// Rejected 是类型/端口校验失败计数。
func (l *Loop) Rejected() uint64 { return l.rejected.Load() }

// Close 立即停止：取消 handler ctx，唤醒所有阻塞的 push，等待 actor 退出。
// 队列中未处理的事件会被丢弃（Stopped 计数由 Dropped 体现）。
func (l *Loop) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.cancel()
	})
	l.wg.Wait()
	return nil
}

// Tx 是 handler 的写侧。它只追加 outbox，不做任何阻塞操作。
type Tx struct {
	loop    *Loop
	from    string
	hops    uint16
	traceID string
	emits   []Emit
}

// Emit 记录一次输出。端口必须已声明为输出端口，且 Datum 类型必须匹配。
func (t *Tx) Emit(port string, d Datum) error {
	l := t.loop
	l.mu.RLock()
	p, ok := l.portLocked(Endpoint{Neuron: t.from, Port: port})
	l.mu.RUnlock()
	if !ok {
		l.rejected.Add(1)
		return fmt.Errorf("%w: %s.%s", ErrUnknownPort, t.from, port)
	}
	if !p.Out {
		l.rejected.Add(1)
		return fmt.Errorf("%w: %s.%s 不是输出端口", ErrBadPort, t.from, port)
	}
	if d == nil {
		l.rejected.Add(1)
		return fmt.Errorf("%w: nil datum", ErrTypeMismatch)
	}
	if d.TypeName() != p.Type {
		l.rejected.Add(1)
		return fmt.Errorf("%w: %s.%s 需要 %s，收到 %s", ErrTypeMismatch, t.from, port, p.Type, d.TypeName())
	}
	t.emits = append(t.emits, Emit{Port: port, Datum: d, Type: p.Type})
	return nil
}

// Emit 是 Tx.Emit 的链式简写（错误仍显式返回，不吞）。
func (t *Tx) EmitValue(port string, d Datum) { _ = t.Emit(port, d) }

// Emits 返回本次 handler 的 outbox（只读，供测试）。
func (t *Tx) Emits() []Emit { return append([]Emit(nil), t.emits...) }

// Emit 是 outbox 中的一条记录。
type Emit struct {
	Port  string
	Type  string
	Datum Datum
}
