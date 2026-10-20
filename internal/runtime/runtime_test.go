package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neura/internal/parser"
)

func load(t *testing.T, src string) *Program {
	t.Helper()
	f, errs := parser.Parse(src)
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	return NewProgram(f)
}

func subj(logic string, fields map[string]Value) Value {
	return InstV(&Instance{Name: "<subject>", Logic: logic, Fields: fields})
}

func mustRun(t *testing.T, p *Program, entry string, args []Value) Result {
	t.Helper()
	res, err := p.Run(entry, args)
	if err != nil {
		t.Fatalf("run %s: %v", entry, err)
	}
	return res
}

func contains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------- LogicDB ----------------

func TestLogicDBIndexes(t *testing.T) {
	db := NewLogicDB()
	db.Add(&Instance{Name: "B", Logic: "L", Fields: map[string]Value{"k": IntV(2)}})
	db.Add(&Instance{Name: "A", Logic: "L", Fields: map[string]Value{"k": IntV(1)}})

	if db.Len() != 2 {
		t.Fatalf("len = %d", db.Len())
	}
	of := db.InstancesOf("L")
	if len(of) != 2 || of[0].Name != "A" || of[1].Name != "B" {
		t.Fatalf("InstancesOf must be name-sorted, got %v,%v", of[0].Name, of[1].Name)
	}
	idx := db.FieldIndex("k", IntV(1))
	if len(idx) != 1 || idx[0].Name != "A" {
		t.Fatalf("FieldIndex = %v", idx)
	}
	if _, ok := db.Lookup("B"); !ok {
		t.Fatal("Lookup(B) failed")
	}
	if names := db.LogicNames(); len(names) != 1 || names[0] != "L" {
		t.Fatalf("LogicNames = %v", names)
	}
}

// ---------------- 纯函数打分 ----------------

func TestSimilarityIsPureAndBounded(t *testing.T) {
	a := map[string]Value{"x": IntV(1), "y": StringV("eu")}
	b := map[string]Value{"x": IntV(1), "y": StringV("us")}

	if !StrictEqual(a, map[string]Value{"x": IntV(1), "y": StringV("eu")}) {
		t.Fatal("StrictEqual should be true for identical fields")
	}
	if StrictEqual(a, b) {
		t.Fatal("StrictEqual should be false")
	}
	s1 := Similarity(a, b)
	s2 := Similarity(a, b)
	if s1 != s2 {
		t.Fatalf("similarity not deterministic: %v vs %v", s1, s2)
	}
	if s1 != 0.5 {
		t.Fatalf("similarity = %v, want 0.5", s1)
	}
	if Similarity(map[string]Value{}, map[string]Value{}) != 1 {
		t.Fatal("empty vs empty should be 1")
	}
}

// ---------------- match 顺序 ----------------

const twoCand = `
logic L {
  structure: { a: Int; b: Int }
  capability: { run() -> Unit {} }
}
instance X : L { structure: { a: 2; b: 2 } }
instance Y : L { structure: { a: 1; b: 2 } }

fn pick(x: L) -> Unit {
  match x against LogicDB {
    case L(e) => { }
  }
}
`

// strict 优先：Y 字段全等（strict），X 只有一半相同（similarity）。
func TestStrictBeatsSimilarity(t *testing.T) {
	p := load(t, twoCand)
	res := mustRun(t, p, "pick", []Value{subj("L", map[string]Value{"a": IntV(1), "b": IntV(2)})})

	if len(res.Trace) != 1 {
		t.Fatalf("trace = %d records, want 1", len(res.Trace))
	}
	rec := res.Trace[0]
	if rec.Chosen != "Y" || rec.Reason != "strict" {
		t.Fatalf("chosen=%q reason=%q, want Y/strict", rec.Chosen, rec.Reason)
	}
	if len(rec.Candidates) != 2 || !rec.Candidates[0].Strict {
		t.Fatalf("candidates must be strict-first, got %+v", rec.Candidates)
	}
}

// 同分时按实例名升序（确定性 tie-break），多次运行结果完全一致。
func TestDeterministicTieBreak(t *testing.T) {
	src := `
logic L {
  structure: { a: Int }
  capability: { run() -> Unit {} }
}
instance C : L { structure: { a: 1 } }
instance A : L { structure: { a: 1 } }
instance B : L { structure: { a: 1 } }

fn pick(x: L) -> Unit {
  match x against LogicDB {
    case L(e) => { }
  }
}
`
	var first string
	for i := 0; i < 5; i++ {
		p := load(t, src)
		res := mustRun(t, p, "pick", []Value{subj("L", map[string]Value{"a": IntV(1)})})
		if res.Trace[0].Chosen != "A" {
			t.Fatalf("chosen = %q, want A（同分按名字升序）", res.Trace[0].Chosen)
		}
		var buf bytes.Buffer
		if err := p.WriteTrace(&buf); err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = buf.String()
		} else if first != buf.String() {
			t.Fatalf("trace not deterministic:\n%s\nvs\n%s", first, buf.String())
		}
	}
}

// 相似度命中（没有 strict 候选时按分数降序选）。
func TestSimilarityOrdering(t *testing.T) {
	src := `
logic L {
  structure: { a: Int; b: Int; c: Int }
  capability: { run() -> Unit {} }
}
instance Low : L { structure: { a: 9; b: 9; c: 9 } }
instance High : L { structure: { a: 1; b: 1; c: 9 } }

fn pick(x: L) -> Unit {
  match x against LogicDB {
    case L(e) => { }
  }
}
`
	p := load(t, src)
	res := mustRun(t, p, "pick", []Value{subj("L", map[string]Value{"a": IntV(1), "b": IntV(1), "c": IntV(9)})})
	rec := res.Trace[0]
	if rec.Chosen != "High" || rec.Reason != "strict" {
		t.Fatalf("chosen=%q reason=%q, want High/strict（字段全等）", rec.Chosen, rec.Reason)
	}

	// 改 subject 让 c 不同：High 2/3，Low 1/3
	p2 := load(t, src)
	res2 := mustRun(t, p2, "pick", []Value{subj("L", map[string]Value{"a": IntV(1), "b": IntV(1), "c": IntV(0)})})
	rec2 := res2.Trace[0]
	if rec2.Chosen != "High" || rec2.Reason != "similarity" {
		t.Fatalf("chosen=%q reason=%q, want High/similarity", rec2.Chosen, rec2.Reason)
	}
}

// §17 N6 的运行时语义：LogicDB 无 SomeLogic instance 时不匹配、跳过；有则绑定 e。
func TestN6MatchSkipsWhenNoInstance(t *testing.T) {
	src := `
logic SomeLogic {
  structure: { user_id: Int }
  capability: { run() -> Unit {} }
}
instance Bar : SomeLogic { structure: { user_id: 42 } }

fn n6(x: SomeLogic) -> Unit {
  match x against LogicDB {
    case SomeLogic(e) => { invoke e.run() }
  }
}
`
	// 情况 1：DB 里有 instance → 绑定并 invoke
	p := load(t, src)
	res := mustRun(t, p, "n6", []Value{subj("SomeLogic", map[string]Value{"user_id": IntV(42)})})
	if !contains(res.Outputs, "invoke Bar.run") {
		t.Fatalf("outputs = %v, want invoke Bar.run", res.Outputs)
	}
	if res.Trace[0].Chosen != "Bar" {
		t.Fatalf("chosen = %q", res.Trace[0].Chosen)
	}

	// 情况 2：DB 里没有 SomeLogic instance → 跳过，不报错
	src2 := `
logic SomeLogic {
  structure: { user_id: Int }
  capability: { run() -> Unit {} }
}
fn n6(x: SomeLogic) -> Unit {
  match x against LogicDB {
    case SomeLogic(e) => { invoke e.run() }
  }
}
`
	p2 := load(t, src2)
	res2 := mustRun(t, p2, "n6", []Value{subj("SomeLogic", map[string]Value{"user_id": IntV(42)})})
	if len(res2.Outputs) != 0 {
		t.Fatalf("no instance → 不应有输出，got %v", res2.Outputs)
	}
	if len(res2.Trace) != 1 || res2.Trace[0].Reason != "no-candidate" {
		t.Fatalf("trace = %+v, want one no-candidate record", res2.Trace)
	}
}

// ---------------- reuse / invoke / emit ----------------

func TestReuseOutputs(t *testing.T) {
	src := `
logic L {
  structure: { k: Int }
  capability: { run() -> Unit {} }
}
instance I : L { structure: { k: 1 } }

fn go(x: L) -> Unit {
  match x against LogicDB {
    case L(r) => reuse r
  }
}
`
	p := load(t, src)
	res := mustRun(t, p, "go", []Value{subj("L", map[string]Value{"k": IntV(1)})})
	if !contains(res.Outputs, "reuse I") {
		t.Fatalf("outputs = %v, want reuse I", res.Outputs)
	}
}

func TestInvokeRunsCapabilityAndLearn(t *testing.T) {
	src := `
logic L {
  structure: { k: Int }
  capability: {
    run() -> Unit {
      learn(7)
    }
  }
}
instance I : L { structure: { k: 1 } }

fn go(x: L) -> Unit {
  match x against LogicDB {
    case L(r) => { invoke r.run() }
  }
}
`
	p := load(t, src)
	res := mustRun(t, p, "go", []Value{subj("L", map[string]Value{"k": IntV(1)})})
	if !contains(res.Outputs, "invoke I.run") {
		t.Fatalf("outputs = %v", res.Outputs)
	}
	if !contains(res.Learns, "learn 7") {
		t.Fatalf("learns = %v", res.Learns)
	}
}

func TestEmitInNeuronHandler(t *testing.T) {
	src := `
neuron N {
  input i: Signal<Int>
  handle {
    emit i
  }
}
`
	p := load(t, src)
	res, err := p.RunHandler("N", []Value{IntV(7)})
	if err != nil {
		t.Fatalf("run handler: %v", err)
	}
	if !contains(res.Outputs, "emit 7") {
		t.Fatalf("outputs = %v, want emit 7", res.Outputs)
	}
}

// ---------------- MatchTrace JSONL ----------------

func TestMatchTraceJSONL(t *testing.T) {
	p := load(t, twoCand)
	mustRun(t, p, "pick", []Value{subj("L", map[string]Value{"a": IntV(1), "b": IntV(2)})})

	var buf bytes.Buffer
	if err := p.WriteTrace(&buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 JSONL line, got %d: %q", len(lines), buf.String())
	}
	var rec TraceRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("trace is not valid JSON: %v (%s)", err, lines[0])
	}
	if rec.Seq != 1 || rec.Arm != "L" || rec.Chosen != "Y" || rec.Reason != "strict" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Logic != "L" || rec.Subject != "<subject>" {
		t.Fatalf("record = %+v", rec)
	}
}

// ---------------- place：只实现 local ----------------

func TestPlaceLocalOnlyWithStub(t *testing.T) {
	src := `
logic L {
  structure: { k: Int }
  capability: { run() -> Unit {} }
}
instance I : L { structure: { k: 1 } }

place I at local where gpu_mem >= 1
place I at ["node-a", "node-b"] balance consistent_hash(k)
`
	p := load(t, src)
	placed := p.Placements()
	if placed["I"] != "local(stub)" {
		t.Fatalf("placements = %v", placed)
	}
	_, err := p.Run("nope", nil)
	if err == nil {
		t.Fatal("expected error for missing entry")
	}

	// stub warning 必须记录
	found := false
	for _, w := range p.warnings {
		if strings.Contains(w, "stub") && strings.Contains(w, "node-a") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing place stub warning, got %v", p.warnings)
	}
}

func TestPlaceLocalNoWarning(t *testing.T) {
	src := `
logic L {
  structure: { k: Int }
  capability: { run() -> Unit {} }
}
instance I : L { structure: { k: 1 } }
place I at local
`
	p := load(t, src)
	if p.Placements()["I"] != "local" {
		t.Fatalf("placements = %v", p.Placements())
	}
	for _, w := range p.warnings {
		if strings.Contains(w, "stub") {
			t.Fatalf("unexpected stub warning: %v", p.warnings)
		}
	}
}

// ---------------- §14 不做的功能：stub ----------------

func TestUnimplementedFeaturesAreStubs(t *testing.T) {
	p := load(t, "fn f() -> Unit { }\n")
	if p.Embed("hello") != nil {
		t.Fatal("embedding stub should return nil")
	}
	_ = p.CustomSimilarity(map[string]Value{"a": IntV(1)}, map[string]Value{"a": IntV(1)})
	if p.AutoDelta() {
		t.Fatal("auto delta must stay stubbed (false)")
	}
	if p.CrossNodeExactlyOnce() {
		t.Fatal("cross-node exactly_once must stay stubbed (false)")
	}
	for _, want := range []string{"embedding", "custom similarity", "delta", "exactly_once"} {
		if !contains(p.warnings, want) {
			t.Fatalf("missing stub warning for %q, got %v", want, p.warnings)
		}
	}
}

// ---------------- 控制流 / 数值 ----------------

func TestForAndIfExecution(t *testing.T) {
	src := `
fn sum_to(n: Int) -> Int {
  let mut total = 0
  for i in 0..=n {
    if i == 3 { continue }
    total = total + i
  }
  total
}
`
	p := load(t, src)
	res := mustRun(t, p, "sum_to", []Value{IntV(5)})
	// 0+1+2+4+5 = 12（跳过 3）
	if res.Returned.Kind != KInt || res.Returned.I != 12 {
		t.Fatalf("returned = %v, want 12", res.Returned)
	}
}

func TestBreakInFor(t *testing.T) {
	src := `
fn first_over(n: Int) -> Int {
  let mut found = -1
  for i in 0..n {
    if i * i > 20 {
      found = i
      break
    }
  }
  found
}
`
	p := load(t, src)
	res := mustRun(t, p, "first_over", []Value{IntV(10)})
	if res.Returned.I != 5 {
		t.Fatalf("returned = %v, want 5", res.Returned)
	}
}

// ---------------- 完整示例 ----------------

func TestRunDemoFile(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("../../examples/run_demo.neura"))
	if err != nil {
		t.Fatalf("read demo: %v", err)
	}
	f, perrs := parser.Parse(string(src))
	if len(perrs) != 0 {
		t.Fatalf("demo parse errors: %v", perrs)
	}
	p := NewProgram(f)

	eu, ok := p.DB.Lookup("RouterEU")
	if !ok {
		t.Fatal("RouterEU not loaded")
	}
	res, err := p.Run("dispatch", []Value{InstV(eu)})
	if err != nil {
		t.Fatalf("run dispatch: %v", err)
	}
	if !contains(res.Outputs, "invoke RouterEU.run") {
		t.Fatalf("outputs = %v", res.Outputs)
	}
	if !contains(res.Learns, "learn 1") {
		t.Fatalf("learns = %v", res.Learns)
	}
	if !contains(res.Warnings, "stub") {
		t.Fatalf("place at 非 local 应打 stub warning，got %v", res.Warnings)
	}
	if len(res.Trace) != 1 || res.Trace[0].Chosen != "RouterEU" {
		t.Fatalf("trace = %+v", res.Trace)
	}
}

// ---------------- 回归：Range 的迭代求值 ----------------

// 非零下界必须正确：5..10 = 35（不含 10），5..=10 = 45。
func TestRangeNonZeroLowerBound(t *testing.T) {
	src := `
fn sum_between(lo: Int, hi: Int) -> Int {
  let mut total = 0
  for i in lo..hi {
    total = total + i
  }
  total
}

fn sum_between_inclusive(lo: Int, hi: Int) -> Int {
  let mut total = 0
  for i in lo..=hi {
    total = total + i
  }
  total
}
`
	p := load(t, src)

	got := mustRun(t, p, "sum_between", []Value{IntV(5), IntV(10)}).Returned
	if got.I != 35 {
		t.Fatalf("5..10 = %d, want 35", got.I)
	}
	got = mustRun(t, p, "sum_between_inclusive", []Value{IntV(5), IntV(10)}).Returned
	if got.I != 45 { // 5+6+7+8+9+10
		t.Fatalf("5..=10 = %d, want 45", got.I)
	}

	// 0..n 的旧用例也不能回归
	res := mustRun(t, p, "sum_between", []Value{IntV(0), IntV(5)})
	if res.Returned.I != 10 {
		t.Fatalf("0..5 = %d, want 10", res.Returned.I)
	}
}

// ---------------- 回归：实参个数 ----------------

// 之前多传实参被静默丢弃（range_sum(5,10) 只算 range_sum(5) → 16），现在必须报错。
func TestRuntimeArityError(t *testing.T) {
	p := load(t, "fn one(n: Int) -> Int { n }\n")
	_, err := p.Run("one", []Value{IntV(5), IntV(10)})
	if err == nil {
		t.Fatal("多传实参必须报错")
	}
	if !strings.Contains(err.Error(), "需要 1 个参数") || !strings.Contains(err.Error(), "实际传入 2 个") {
		t.Fatalf("error = %v", err)
	}

	// 少传仍允许（逻辑类型参数会合成 <subject>，§17 N6 依赖这一点）
	res, err := p.Run("one", nil)
	if err != nil {
		t.Fatalf("少传实参应合成缺省值，got %v", err)
	}
	if res.Returned.Kind != KUnit {
		t.Fatalf("returned = %v", res.Returned)
	}
}

// ---------------- 回归：trace 为空要有可诊断记录 ----------------

func TestTraceRecordsNonInstanceSubject(t *testing.T) {
	src := `
logic L {
  structure: { k: Int }
  capability: { run() -> Unit {} }
}
fn f(n: Int) -> Unit {
  match n against LogicDB {
    case L => { }
  }
}
`
	p := load(t, src)
	res := mustRun(t, p, "f", []Value{IntV(1)})
	if len(res.Trace) != 1 {
		t.Fatalf("trace = %d records, want 1（subject 不是 instance 也要留痕）", len(res.Trace))
	}
	if res.Trace[0].Reason != "subject-not-instance" {
		t.Fatalf("reason = %q", res.Trace[0].Reason)
	}
}

func TestNoMatchMeansNoRecords(t *testing.T) {
	p := load(t, "fn f(n: Int) -> Int { n }\n")
	res := mustRun(t, p, "f", []Value{IntV(1)})
	if len(res.Trace) != 0 {
		t.Fatalf("trace = %d, want 0（没有 match 就没有记录）", len(res.Trace))
	}
	var buf bytes.Buffer
	if err := p.WriteTrace(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("trace 应为空，got %q", buf.String())
	}
}

// 完整示例里的双参数区间函数（用户报告的 range_sum(5,10) 场景）
func TestRunDemoSumBetween(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("../../examples/run_demo.neura"))
	if err != nil {
		t.Fatalf("read demo: %v", err)
	}
	f, perrs := parser.Parse(string(src))
	if len(perrs) != 0 {
		t.Fatalf("demo parse errors: %v", perrs)
	}
	p := NewProgram(f)
	res, err := p.Run("sum_between", []Value{IntV(5), IntV(10)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Returned.I != 35 {
		t.Fatalf("sum_between(5,10) = %d, want 35", res.Returned.I)
	}

	// 多传实参必须报错，而不是静默得到 16
	if _, err := p.Run("sum_between", []Value{IntV(5), IntV(10), IntV(7)}); err == nil {
		t.Fatal("sum_between 只接受 2 个实参，多传必须报错")
	}
}
