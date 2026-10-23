package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"neura/internal/event"
	"neura/internal/parser"
	"neura/internal/place"
	"neura/internal/state"
)

func loadWithPlacement(t *testing.T, src string, a place.Adapter) *Program {
	t.Helper()
	f, errs := parser.Parse(src)
	if len(errs) != 0 {
		t.Fatalf("parse: %v", errs)
	}
	return NewProgramWithPlacement(f, a)
}

// ---------------- Event Loop 接线 ----------------

const eventSrc = `
neuron A {
  input x: Signal<Int>
  output o: Signal<Int>
  on signal v: Int from x { emit o(v + 1) }
}

neuron B {
  input i: Signal<Int>
  on signal v: Int from i { learn(v) }
}

synapse A.o -> B.i
`

func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestEventLoopBridge(t *testing.T) {
	p := load(t, eventSrc)
	loop, err := p.BuildEventLoop(event.Config{Capacity: 8, Policy: event.Block, TraceLimit: 32})
	if err != nil {
		t.Fatal(err)
	}
	loop.Start()
	defer loop.Close()

	if err := p.Inject(context.Background(), loop, "A", "x", IntV(41), "trace-evt"); err != nil {
		t.Fatal(err)
	}
	if err := loop.WaitIdle(waitCtx(t)); err != nil {
		t.Fatal(err)
	}

	res := p.snapshot(Unit(), "trace-evt")
	found := false
	for _, l := range res.Learns {
		if l == "learn 42" {
			found = true
		}
	}
	if !found {
		t.Fatalf("learns = %v, want learn 42", res.Learns)
	}

	trail := loop.Trace()
	if len(trail) != 2 {
		t.Fatalf("trace = %+v", trail)
	}
	if trail[1].TraceID != "trace-evt" || trail[1].From.Neuron != "A" || trail[1].Port != "i" {
		t.Fatalf("bad propagated signal: %+v", trail[1])
	}

	// 类型闸门：错误类型在入队前拒绝。
	if err := p.Inject(context.Background(), loop, "A", "x", StringV("nope"), "t"); err == nil {
		t.Fatal("wrong type must be rejected")
	}
	if err := p.Inject(context.Background(), loop, "A", "nope", IntV(1), "t"); err == nil {
		t.Fatal("unknown port must be rejected")
	}
}

func TestBuildEventLoopRejectsTypeMismatchedSynapse(t *testing.T) {
	src := `
neuron A { output o: Signal<Int> }
neuron B { input i: Signal<String> }
synapse A.o -> B.i
`
	p := load(t, src)
	if _, err := p.BuildEventLoop(event.Config{}); !errors.Is(err, event.ErrTypeMismatch) {
		t.Fatalf("err = %v, want type mismatch", err)
	}
}

// ---------------- 相似度扩展 ----------------

type fixedProvider struct{ score float64 }

func (f fixedProvider) Name() string { return "fixed" }
func (f fixedProvider) Score(_, _ map[string]Value) float64 {
	return f.score
}

func TestSimilarityProviderTaggedInTrace(t *testing.T) {
	src := `
logic L { structure: { k: Int } capability: { run() -> Unit {} } }
instance A : L { structure: { k: 1 } }
instance B : L { structure: { k: 2 } }
fn f(x: L) -> Unit {
  match x against LogicDB {
    case L(e) => { invoke e.run() }
  }
}
`
	p := load(t, src)
	p.SetSimilarityProvider(fixedProvider{score: 0.25})
	sub := InstV(&Instance{Name: "<s>", Logic: "L", Fields: map[string]Value{"k": IntV(3)}})
	if _, err := p.Run("f", []Value{sub}); err != nil {
		t.Fatal(err)
	}
	if len(p.trace) != 1 {
		t.Fatalf("trace = %+v", p.trace)
	}
	for _, c := range p.trace[0].Candidates {
		if c.Provider != "fixed" || c.Score != 0.25 {
			t.Fatalf("candidate = %+v", c)
		}
	}
}

func TestSimHashProvider(t *testing.T) {
	sh := SimHashProvider{}
	a := map[string]Value{"region": StringV("eu"), "tier": IntV(1)}
	b := map[string]Value{"region": StringV("eu"), "tier": IntV(1)}
	c := map[string]Value{"region": StringV("us"), "tier": IntV(9)}
	if got := sh.Score(a, b); got != 1 {
		t.Fatalf("identical score = %v", got)
	}
	s1, s2 := sh.Score(a, c), sh.Score(a, c)
	if s1 != s2 || s1 < 0 || s1 > 1 {
		t.Fatalf("score = %v/%v", s1, s2)
	}
	if sh.Score(map[string]Value{}, map[string]Value{}) != 1 {
		t.Fatal("empty vs empty = 1")
	}
	if sh.Name() != "simhash" {
		t.Fatal(sh.Name())
	}
}

type fakeProbe struct {
	fail bool
	vec  []float32
}

func (f *fakeProbe) Embed(ctx context.Context, text string) ([]float32, error) {
	if f.fail {
		return nil, errors.New("probe down")
	}
	if f.vec == nil {
		return []float32{1, 0}, nil
	}
	return f.vec, nil
}

type fakeIndex struct {
	gotVec []float32
	gotK   int
	hits   []VectorHit
}

func (f *fakeIndex) Search(ctx context.Context, vec []float32, k int) ([]VectorHit, error) {
	f.gotVec, f.gotK = vec, k
	return f.hits, nil
}

func (f *fakeIndex) Upsert(context.Context, string, []float32) error { return nil }

func TestEmbeddingProviderFallbackAndVectorProbe(t *testing.T) {
	ep := &EmbeddingProvider{Probe: &fakeProbe{}, Fallback: JaccardProvider{}, Timeout: time.Second}
	a := map[string]Value{"x": IntV(1)}
	b := map[string]Value{"y": IntV(2)}
	if got := ep.Score(a, b); got != 1 {
		t.Fatalf("cosine score = %v, want 1", got)
	}
	// 向量服务失败必须回退 Jaccard（a/b 无交集 → 0），不能阻断 match。
	ep.Probe = &fakeProbe{fail: true}
	if got := ep.Score(a, b); got != 0 {
		t.Fatalf("fallback score = %v, want 0", got)
	}

	idx := &fakeIndex{hits: []VectorHit{{ID: "A", Score: 0.9}}}
	ep = &EmbeddingProvider{Probe: &fakeProbe{vec: []float32{0, 1}}, Index: idx, Timeout: time.Second}
	hits, err := ep.Search(context.Background(), a, 3)
	if err != nil || len(hits) != 1 || hits[0].ID != "A" {
		t.Fatalf("search = %v %v", hits, err)
	}
	if idx.gotK != 3 || len(idx.gotVec) != 2 || idx.gotVec[1] != 1 {
		t.Fatalf("index probe got k=%d vec=%v", idx.gotK, idx.gotVec)
	}

	// Program.Embed 走探针；未配置时保持老 stub（返回 nil）。
	p := load(t, "fn f() -> Unit {}\n")
	if p.Embed("x") != nil {
		t.Fatal("no provider should be nil")
	}
	p.SetSimilarityProvider(&EmbeddingProvider{Probe: &fakeProbe{vec: []float32{1, 2}}, Timeout: time.Second})
	v := p.Embed("x")
	if len(v) != 2 || v[0] != 1 || v[1] != 2 {
		t.Fatalf("embed = %v", v)
	}
}

func TestNearestAndCanonicalText(t *testing.T) {
	db := NewLogicDB()
	db.Add(&Instance{Name: "B", Logic: "L", Fields: map[string]Value{"x": IntV(1)}})
	db.Add(&Instance{Name: "A", Logic: "L", Fields: map[string]Value{"x": IntV(1)}})
	db.Add(&Instance{Name: "C", Logic: "L", Fields: map[string]Value{"x": IntV(9)}})
	got := db.Nearest("L", map[string]Value{"x": IntV(1)}, nil, 2)
	if len(got) != 2 || got[0].Instance != "A" || got[1].Instance != "B" {
		t.Fatalf("nearest = %+v", got)
	}
	if got[0].Score != 1 {
		t.Fatalf("score = %v", got[0].Score)
	}
	// 键顺序不影响规范文本。
	t1 := CanonicalText(map[string]Value{"b": IntV(2), "a": StringV("x")})
	t2 := CanonicalText(map[string]Value{"a": StringV("x"), "b": IntV(2)})
	if t1 != t2 || !strings.Contains(t1, "a=") || strings.Index(t1, "a=") > strings.Index(t1, "b=") {
		t.Fatalf("canonical text = %q / %q", t1, t2)
	}
}

// ---------------- Place Adapter 接线 ----------------

func TestPlacementAdapterUsesWhereAndLabels(t *testing.T) {
	src := `
logic L { structure: { zone: String } capability: { run() -> Unit {} } }
instance I : L { structure: { zone: "edge" } }
place I at ["n1", "n2", "n3"] where gpu_mem >= 4 && zone == "edge"
`
	a := place.NewLabelAdapter(
		place.NodeInfo{Name: "n1", Ready: true, Metrics: map[string]float64{"gpu_mem": 8}},
		place.NodeInfo{Name: "n2", Ready: true, Metrics: map[string]float64{"gpu_mem": 2}},
		place.NodeInfo{Name: "n3", Ready: true, Metrics: map[string]float64{"gpu_mem": 16},
			Labels: map[string]string{"zone": "edge"}},
	)
	p := loadWithPlacement(t, src, a)
	nodes := p.Table.Nodes("I")
	if len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n3" {
		t.Fatalf("nodes = %v", nodes)
	}
	pl, ok := p.Placement("I")
	if !ok || pl.Adapter != "labels" || len(pl.Nodes) != 2 {
		t.Fatalf("placement = %+v", pl)
	}
	if p.Placements()["I"] != "cluster" {
		t.Fatalf("placements = %v", p.Placements())
	}
}

func TestPlacementAdapterFallsBackToStatic(t *testing.T) {
	src := `
logic L { structure: { zone: String } capability: { run() -> Unit {} } }
instance I : L { structure: { zone: "edge" } }
place I at ["n1", "n2"] where gpu_mem >= 100
`
	a := place.NewLabelAdapter(place.NodeInfo{Name: "n1", Ready: true, Metrics: map[string]float64{"gpu_mem": 1}})
	p := loadWithPlacement(t, src, a)
	if nodes := p.Table.Nodes("I"); len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n2" {
		t.Fatalf("static fallback nodes = %v", nodes)
	}
	pl, _ := p.Placement("I")
	if pl.Adapter != "static" {
		t.Fatalf("placement = %+v", pl)
	}
	found := false
	for _, w := range p.snapshot(Unit(), "").Warnings {
		if strings.Contains(w, "回退静态节点表") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing fallback warning")
	}
}

func TestEnableSharedStateFlipsExactlyOnce(t *testing.T) {
	p := load(t, "fn f() -> Unit {}\n")
	if p.CrossNodeExactlyOnce() {
		t.Fatal("default must stay stub")
	}
	store := state.NewStore(16, time.Minute)
	p.EnableSharedState(state.NewCoordinator("n1", store, nil, state.LocalRouter("n1")))
	if !p.CrossNodeExactlyOnce() {
		t.Fatal("shared state should enable exactly-once")
	}
}
