package event

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type datum struct {
	typ string
	val string
}

func (d datum) TypeName() string { return d.typ }
func (d datum) Canon() string    { return d.val }

func intD(v string) datum { return datum{typ: "Int", val: v} }

func newLoop(t *testing.T, cfg Config) *Loop {
	t.Helper()
	l := New(cfg)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// 基础传播：A.x -> A.o -> B.i -> B.o -> C.i，类型 + trace 全程保持。
func TestPropagatesAndPreservesTraceAndType(t *testing.T) {
	var got atomic.Value
	l := newLoop(t, Config{TraceLimit: 64})

	mustAdd := func(name string, ports []Port, h Handler) {
		t.Helper()
		if err := l.AddNeuron(name, ports, h); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd("A", []Port{{Name: "x", Type: "Int"}, {Name: "o", Type: "Int", Out: true}},
		func(_ context.Context, s Signal, tx *Tx) error {
			if s.Datum.TypeName() != "Int" {
				t.Errorf("A got type %s", s.Datum.TypeName())
			}
			return tx.Emit("o", intD(s.Datum.Canon()+"+1"))
		})
	mustAdd("B", []Port{{Name: "i", Type: "Int"}, {Name: "o", Type: "Int", Out: true}},
		func(_ context.Context, s Signal, tx *Tx) error {
			return tx.Emit("o", intD(s.Datum.Canon()+"+1"))
		})
	mustAdd("C", []Port{{Name: "i", Type: "Int"}},
		func(_ context.Context, s Signal, _ *Tx) error {
			got.Store(s.Datum.Canon())
			return nil
		})

	if err := l.Connect(Endpoint{"A", "o"}, Endpoint{"B", "i"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Connect(Endpoint{"B", "o"}, Endpoint{"C", "i"}); err != nil {
		t.Fatal(err)
	}
	l.Start()
	if err := l.Emit(context.Background(), Endpoint{"A", "x"}, intD("1"), "t-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.WaitIdle(timeoutCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got.Load() != "1+1+1" {
		t.Fatalf("C got %v, want 1+1+1", got.Load())
	}
	trail := l.Trace()
	if len(trail) != 3 {
		t.Fatalf("trace len = %d (%+v)", len(trail), trail)
	}
	for _, s := range trail {
		if s.TraceID != "t-1" {
			t.Fatalf("trace lost: %+v", s)
		}
	}
	if trail[2].Hops != 2 {
		t.Fatalf("hops = %d, want 2", trail[2].Hops)
	}
}

func timeoutCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// 背压 Block：容量 1 + 慢消费者，队列深度绝不越界，且事件最终全部送达。
func TestBlockBackpressureBoundsMailbox(t *testing.T) {
	const total = 200
	var handled atomic.Int64

	l := newLoop(t, Config{Capacity: 1, Policy: Block, EmitTimeout: 2 * time.Second})
	if err := l.AddNeuron("Src", []Port{{Name: "x", Type: "Int"}, {Name: "o", Type: "Int", Out: true}},
		func(_ context.Context, s Signal, tx *Tx) error { return tx.Emit("o", s.Datum) }); err != nil {
		t.Fatal(err)
	}
	if err := l.AddNeuron("Slow", []Port{{Name: "i", Type: "Int"}},
		func(_ context.Context, _ Signal, _ *Tx) error {
			handled.Add(1)
			time.Sleep(50 * time.Microsecond)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := l.Connect(Endpoint{"Src", "o"}, Endpoint{"Slow", "i"}); err != nil {
		t.Fatal(err)
	}
	l.Start()

	for i := 0; i < total; i++ {
		if err := l.Emit(context.Background(), Endpoint{"Src", "x"}, intD("1"), "bp"); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	if err := l.WaitIdle(timeoutCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got := handled.Load(); got != total {
		t.Fatalf("handled = %d, want %d", got, total)
	}
	// 容量 1：任意时刻 mailbox 深度 ≤ 1。
	if d := l.Stats()["Slow"].MaxDepth; d > 1 {
		t.Fatalf("Slow max depth = %d, exceeds capacity", d)
	}
	if l.Stats()["Slow"].Dropped != 0 {
		t.Fatalf("unexpected drops: %+v", l.Stats()["Slow"])
	}
}

// DropNewest：满了立即拒绝，不阻塞。
func TestDropNewest(t *testing.T) {
	gate := make(chan struct{})
	l := newLoop(t, Config{Capacity: 1, Policy: DropNewest})
	if err := l.AddNeuron("Sink", []Port{{Name: "i", Type: "Int"}},
		func(context.Context, Signal, *Tx) error { <-gate; return nil }); err != nil {
		t.Fatal(err)
	}
	l.Start()
	_ = l.Emit(context.Background(), Endpoint{"Sink", "i"}, intD("1"), "x") // 占住 worker
	_ = l.Emit(context.Background(), Endpoint{"Sink", "i"}, intD("2"), "x") // 占住 mailbox
	err := l.Emit(context.Background(), Endpoint{"Sink", "i"}, intD("3"), "x")
	if !errors.Is(err, ErrBackpressure) {
		t.Fatalf("err = %v, want backpressure", err)
	}
	if l.Stats()["Sink"].Dropped < 1 {
		t.Fatalf("dropped = %d, want >=1", l.Stats()["Sink"].Dropped)
	}
	close(gate)
	_ = l.WaitIdle(timeoutCtx(t))
}

// 类型不匹配必须在入队前被拒绝。
func TestTypeMismatchRejected(t *testing.T) {
	l := newLoop(t, Config{})
	if err := l.AddNeuron("N", []Port{{Name: "i", Type: "Int"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(context.Background(), Endpoint{"N", "i"}, datum{typ: "String", val: "x"}, "t"); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("err = %v, want type mismatch", err)
	}
	if l.Rejected() != 1 {
		t.Fatalf("rejected = %d", l.Rejected())
	}
	if err := l.Connect(Endpoint{"N", "i"}, Endpoint{"N", "i"}); err == nil {
		t.Fatal("input->input connect must fail")
	}
}

// 反馈环由 MaxHops 斩断，不会无限打爆 mailbox。
func TestHopLimitBreaksCycle(t *testing.T) {
	l := newLoop(t, Config{MaxHops: 8, Capacity: 16, Policy: DropNewest, TraceLimit: 128})
	for _, n := range []string{"A", "B"} {
		if err := l.AddNeuron(n, []Port{{Name: "i", Type: "Int"}, {Name: "o", Type: "Int", Out: true}},
			func(_ context.Context, s Signal, tx *Tx) error { return tx.Emit("o", s.Datum) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Connect(Endpoint{"A", "o"}, Endpoint{"B", "i"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Connect(Endpoint{"B", "o"}, Endpoint{"A", "i"}); err != nil {
		t.Fatal(err)
	}
	l.Start()
	_ = l.Emit(context.Background(), Endpoint{"A", "i"}, intD("1"), "cycle")
	if err := l.WaitIdle(timeoutCtx(t)); err != nil {
		t.Fatal(err)
	}
	var maxHop uint16
	for _, s := range l.Trace() {
		if s.Hops > maxHop {
			maxHop = s.Hops
		}
	}
	if maxHop > 8 {
		t.Fatalf("max hops = %d, exceeds limit", maxHop)
	}
}
