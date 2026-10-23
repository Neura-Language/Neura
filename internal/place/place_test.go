package place

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"neura/internal/ast"
	"neura/internal/parser"
)

func whereOf(t *testing.T, expr string) *Compiled {
	t.Helper()
	src := fmt.Sprintf("place I at [\"n1\"] where %s\n", expr)
	f, errs := parser.Parse(src)
	if len(errs) != 0 {
		t.Fatalf("parse where %q: %v", expr, errs)
	}
	for _, d := range f.Decls {
		if pd, ok := d.(*ast.PlaceDecl); ok {
			c, err := Compile(pd.Where)
			if err != nil {
				t.Fatalf("compile where %q: %v", expr, err)
			}
			return c
		}
	}
	t.Fatalf("no place decl in %q", src)
	return nil
}

func nodesFixture() []NodeInfo {
	return []NodeInfo{
		{Name: "n1", Ready: true, Labels: map[string]string{"zone": "edge", "arch": "arm64"},
			Metrics: map[string]float64{"gpu_mem": 8, "cpu_mem": 32}},
		{Name: "n2", Ready: true, Labels: map[string]string{"zone": "edge", "arch": "x86"},
			Metrics: map[string]float64{"gpu_mem": 2, "cpu_mem": 16}},
		{Name: "n3", Ready: true, Labels: map[string]string{"zone": "cloud", "arch": "x86"},
			Metrics: map[string]float64{"gpu_mem": 16, "cpu_mem": 64}},
		{Name: "n4", Ready: false, Labels: map[string]string{"zone": "edge"},
			Metrics: map[string]float64{"gpu_mem": 64}},
	}
}

func TestWhereLabelAndMetricMatching(t *testing.T) {
	c := whereOf(t, `gpu_mem >= 4 && zone == "edge"`)
	a := NewLabelAdapter(nodesFixture()...)
	got, err := a.Match(context.Background(), Request{Where: c})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "n1" {
		t.Fatalf("got %v, want [n1]", got)
	}

	// 未 Ready 的节点即使指标最高也不能被选中。
	c = whereOf(t, `gpu_mem >= 32`)
	got, err = a.Match(context.Background(), Request{Where: c})
	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("err = %v, want no node", err)
	}

	// 实例字段优先于节点指标。
	c = whereOf(t, `user_id >= 10`)
	got, err = a.Match(context.Background(), Request{Where: c, Vars: map[string]any{"user_id": int64(42)}})
	if err != nil || len(got) != 3 {
		t.Fatalf("vars match = %v, %v", got, err)
	}

	// has_label / ! / ~=。
	c = whereOf(t, `has_label("arch") && !(arch ~= "arm")`)
	got, err = a.Match(context.Background(), Request{Where: c})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "n2" || got[1] != "n3" {
		t.Fatalf("got %v, want [n2 n3]", got)
	}

	if unk := c.Unknown(nodesFixture(), nil); len(unk) != 0 {
		t.Fatalf("unknown = %v", unk)
	}
}

func TestWhereTypeErrors(t *testing.T) {
	for _, expr := range []string{
		`gpu_mem + "x" > 1`,
		`gpu_mem / 0 > 1`,
		`gpu_mem > "x"`,
	} {
		c := whereOf(t, expr)
		_, err := c.Match(NodeInfo{Name: "n", Ready: true, Metrics: map[string]float64{"gpu_mem": 1}}, nil, 0)
		if err == nil {
			t.Fatalf("%q should fail", expr)
		}
	}
	// 未知标识符在求值时报错，Unknown() 能提前定位拼写。
	c := whereOf(t, `gpu_men > 1`)
	_, err := c.Match(NodeInfo{Name: "n", Ready: true, Metrics: map[string]float64{"gpu_mem": 1}}, nil, 0)
	if err == nil {
		t.Fatal("typo should not silently match")
	}
	if unk := c.Unknown(nodesFixture(), nil); len(unk) != 1 || unk[0] != "gpu_men" {
		t.Fatalf("unknown = %v", unk)
	}
}

func TestRoundRobinIsDeterministicBySeq(t *testing.T) {
	a := NewLabelAdapter(nodesFixture()...)
	c := whereOf(t, `gpu_mem >= 2`)
	first, err := a.Match(context.Background(), Request{Where: c, Balance: "round_robin", Seq: 0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Match(context.Background(), Request{Where: c, Balance: "round_robin", Seq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("got %v / %v", first, second)
	}
	if first[0] == second[0] {
		t.Fatalf("seq must rotate starting node: %v vs %v", first, second)
	}
	// 同一 seq 必须可复现。
	again, _ := a.Match(context.Background(), Request{Where: c, Balance: "round_robin", Seq: 1})
	for i := range again {
		if again[i] != second[i] {
			t.Fatalf("round robin not deterministic: %v vs %v", again, second)
		}
	}
}

func TestConsistentHashMinimalDisruption(t *testing.T) {
	a := NewLabelAdapter(nodesFixture()...)
	c := whereOf(t, `gpu_mem >= 2`)
	owners := map[string]string{}
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("user-%d", i)
		got, err := a.Match(context.Background(), Request{Where: c, Balance: "consistent_hash", HashKey: key})
		if err != nil {
			t.Fatal(err)
		}
		owners[key] = got[0]
		again, _ := a.Match(context.Background(), Request{Where: c, Balance: "consistent_hash", HashKey: key})
		if again[0] != got[0] {
			t.Fatalf("hash not deterministic for %s", key)
		}
	}
	// 增加一个节点：只应迁移原本属于旧节点的部分键，且旧键的相对分布不变。
	a.Upsert(NodeInfo{Name: "n5", Ready: true, Labels: map[string]string{"zone": "edge"},
		Metrics: map[string]float64{"gpu_mem": 32}})
	moved := 0
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("user-%d", i)
		got, err := a.Match(context.Background(), Request{Where: c, Balance: "consistent_hash", HashKey: key})
		if err != nil {
			t.Fatal(err)
		}
		if got[0] != owners[key] {
			moved++
		}
	}
	// 理想迁移率 1/(N+1)，允许较宽区间；关键是不能全量重排。
	if moved == 0 || moved >= 150 {
		t.Fatalf("moved = %d/200, expected partial remap", moved)
	}
}

func TestAtOrderPreserved(t *testing.T) {
	a := NewLabelAdapter(nodesFixture()...)
	got, err := a.Match(context.Background(), Request{At: []string{"n3", "n1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "n3" || got[1] != "n1" {
		t.Fatalf("got %v, want [n3 n1]", got)
	}
}

// ---------------- RemoteAdapter / K8s 扩展口 ----------------

type fakeSource struct {
	nodes []NodeInfo
	ch    chan Event
}

func (f *fakeSource) List(context.Context) ([]NodeInfo, error) {
	return append([]NodeInfo(nil), f.nodes...), nil
}

func (f *fakeSource) Watch(context.Context) (<-chan Event, error) {
	if f.ch == nil {
		f.ch = make(chan Event, 8)
	}
	return f.ch, nil
}

func TestRemoteAdapterWithWatch(t *testing.T) {
	src := &fakeSource{nodes: nodesFixture(), ch: make(chan Event, 8)}
	r := NewRemoteAdapter(src)
	c := whereOf(t, `zone == "edge" && gpu_mem >= 4`)
	got, err := r.Match(context.Background(), Request{Where: c})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "n1" {
		t.Fatalf("got %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	src.ch <- Event{Kind: EventAdd, Node: NodeInfo{Name: "n9", Ready: true,
		Labels: map[string]string{"zone": "edge"}, Metrics: map[string]float64{"gpu_mem": 12}}}
	deadline := time.Now().Add(time.Second)
	for {
		got, err = r.Match(context.Background(), Request{Where: c})
		if err == nil && len(got) == 2 && got[0] == "n1" && got[1] == "n9" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch event not applied: %v %v", got, err)
		}
		time.Sleep(time.Millisecond)
	}
	src.ch <- Event{Kind: EventDelete, Node: NodeInfo{Name: "n9"}}
	deadline = time.Now().Add(time.Second)
	for {
		got, err = r.Match(context.Background(), Request{Where: c})
		if err == nil && len(got) == 1 && got[0] == "n1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delete event not applied: %v %v", got, err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestCompiledDepsAndRender(t *testing.T) {
	c := whereOf(t, `gpu_mem >= 4 && user_id == 42 && has_label("arch")`)
	deps := c.Deps()
	want := map[string]bool{"gpu_mem": true, "user_id": true, "arch": true}
	if len(deps) != len(want) {
		t.Fatalf("deps = %v", deps)
	}
	for _, d := range deps {
		if !want[d] {
			t.Fatalf("unexpected dep %q", d)
		}
	}
	if c.String() == "" {
		t.Fatal("render must not be empty")
	}
}
