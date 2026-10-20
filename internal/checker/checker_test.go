package checker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neura/internal/ast"
	"neura/internal/diag"
	"neura/internal/lexer"
	"neura/internal/parser"
)

// ---------------- helpers ----------------

func parseCheck(t *testing.T, src string) Result {
	t.Helper()
	f, perrs := parser.Parse(src)
	if len(perrs) != 0 {
		t.Fatalf("parse errors (checker test expected clean parse):\n%v", perrs)
	}
	return Check(f)
}

func codes(res Result) []string {
	out := make([]string, len(res.Errors))
	for i, e := range res.Errors {
		out[i] = e.Code
	}
	return out
}

func hasCode(res Result, code string) bool {
	for _, e := range res.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}

func findCode(res Result, code string) (diag.Error, bool) {
	for _, e := range res.Errors {
		if e.Code == code {
			return e, true
		}
	}
	return diag.Error{}, false
}

// tokenPos 返回第 n 个 lexeme 相同 token 的位置（n 从 0 起，全文件范围）。
func tokenPos(t *testing.T, src, lex string, n int) diag.Pos {
	t.Helper()
	return tokenPosFrom(t, src, lex, n, 1)
}

// tokenPosFrom 只在 startLine 之后找 token（避开 §17 前置定义块里的同名 token）。
func tokenPosFrom(t *testing.T, src, lex string, n, startLine int) diag.Pos {
	t.Helper()
	toks, _ := lexer.Lex(src)
	k := 0
	for _, tok := range toks {
		if tok.Line < startLine || tok.Lexeme != lex {
			continue
		}
		if k == n {
			return diag.Pos{Line: tok.Line, Col: tok.Col}
		}
		k++
	}
	t.Fatalf("token %q #%d after line %d not found", lex, n, startLine)
	return diag.Pos{}
}

// bodyStartLine 返回 "// ---- Nxx ----" 标记之后的第一行行号。
func bodyStartLine(t *testing.T, src, name string) int {
	t.Helper()
	marker := "// ---- " + name
	idx := strings.Index(src, marker)
	if idx < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	return 1 + strings.Count(src[:idx], "\n") + 1
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// prelude 与 testdata/prelude.neura 一致（§17 前置定义块）。
const prelude = `
struct Substance { amount: Float; temperature: Float }
struct Tool      { id: Int }

fn is_liquid(s: Substance) -> Bool { s.temperature > 0.0 }
fn compute_min(s: Size) -> Size { s }

logic SomeLogic {
  structure: { user_id: Int }
  capability: { run() -> Unit {} }
}

instance Bar : SomeLogic {
  structure: { user_id: 42 }
}

neuron Src { output o: Signal<Int> }
neuron Dst { input i: Signal<Int> }
`

// ---------------- 规则 14：跳过 + deprecated ----------------

func TestRule14DeprecatedSkipped(t *testing.T) {
	res := Check(&ast.File{})
	found := false
	for _, l := range res.Logs {
		if l == DeprecatedRule14Log {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing deprecated log, got %v", res.Logs)
	}
	if DeprecatedRule14Log != "rule 14 deprecated, see rule 37" {
		t.Fatalf("log text = %q", DeprecatedRule14Log)
	}
	for _, e := range res.Errors {
		if strings.Contains(e.Code, "14") {
			t.Fatalf("rule 14 must be skipped, got %v", e)
		}
	}
}

// ---------------- §17 负例 N01–N12 ----------------

func TestNegativesN01toN12(t *testing.T) {
	cases := []struct {
		file   string
		code   string // "" 表示 [runtime-test]，编译期必须无错
		token  string
		tokIdx int
	}{
		{"N01", diag.CodeSameLineStmt, "let", 1},
		{"N02", diag.CodeHazardNoWith, "hazardous", 0},
		{"N03", diag.CodeHazardNoGuard, "logic", 0},
		{"N04", diag.CodeUnknownWhereIdent, "foo_bar", 0},
		{"N05", diag.CodeHashFieldNotFound, "nonexistent", 0},
		{"N06", "", "", 0},
		{"N07", diag.CodeParenSingleType, "(", 1},
		{"N08", diag.CodeUnitOutsidePhysics, "-", 0},
		{"N09", diag.CodeWrongPolicy, "retry", 0},
		{"N10", diag.CodeWrongPolicy, "failover", 0},
		{"N11", diag.CodeBreakOutsideLoop, "break", 0},
		{"N12", diag.CodeWhereNotPure, "bad_fn", 1},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			src := readFile(t, filepath.Join("../../testdata/neg", c.file+".neura"))
			f, perrs := parser.Parse(src)

			var res Result
			if len(perrs) == 0 {
				res = Check(f)
			}

			if c.code == "" {
				if len(perrs) != 0 || len(res.Errors) != 0 {
					t.Fatalf("%s should compile clean, got parse=%v check=%v", c.file, perrs, res.Errors)
				}
				return
			}

			start := bodyStartLine(t, src, c.file)

			// parser 阶段的错误码也算在内（N01/N02/N07/N09/N10）
			if len(perrs) > 0 {
				if perrs[0].Code != c.code {
					t.Fatalf("%s: code = %s, want %s (msg=%s)", c.file, perrs[0].Code, c.code, perrs[0].Msg)
				}
				if len(perrs) != 1 {
					t.Fatalf("%s: expected exactly 1 diagnostic, got %v", c.file, perrs)
				}
				want := tokenPosFrom(t, src, c.token, c.tokIdx, start)
				if perrs[0].Pos != want {
					t.Fatalf("%s: span = %s, want %s（必须落在真实 token 上）", c.file, perrs[0].Pos, want)
				}
				return
			}

			err, ok := findCode(res, c.code)
			if !ok {
				t.Fatalf("%s: missing %s, got %v", c.file, c.code, codes(res))
			}
			if len(res.Errors) != 1 {
				t.Fatalf("%s: expected exactly 1 diagnostic, got %v", c.file, res.Errors)
			}
			want := tokenPosFrom(t, src, c.token, c.tokIdx, start)
			if err.Pos != want {
				t.Fatalf("%s: span = %s, want %s（必须落在真实 token 上）", c.file, err.Pos, want)
			}
			if err.Msg == "" || strings.Contains(err.Msg, "[待") {
				t.Fatalf("%s: message not readable: %q", c.file, err.Msg)
			}
		})
	}
}

// ---------------- 正例 P01–P07 ----------------

func TestPositivesP01toP07(t *testing.T) {
	files, err := filepath.Glob("../../testdata/pos/P*.neura")
	if err != nil || len(files) != 7 {
		t.Fatalf("positives: %v (%d files)", err, len(files))
	}
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			src := readFile(t, path)
			f, perrs := parser.Parse(src)
			if len(perrs) != 0 {
				t.Fatalf("parse errors: %v", perrs)
			}
			res := Check(f)
			if len(res.Errors) != 0 {
				t.Fatalf("expected parse+check OK, got %v", res.Errors)
			}
		})
	}
}

// ---------------- 规则 35 / 39：纯函数判定 ----------------

func TestPurityRule39LocalMutIsPure(t *testing.T) {
	src := prelude + `
logic PL {
  structure: { k: Int }
  capability: { run() -> Unit {} }
}
instance PI : PL { structure: { k: 1 } }

fn local_mut(n: Int) -> Int {
  let mut x = n
  x = x + 1
  if x > 10 {
    x = 0
  }
  x
}

place PI at local where local_mut(1) >= 1
`
	res := parseCheck(t, src)
	if len(res.Errors) != 0 {
		t.Fatalf("局部 mut 赋值不算副作用（规则 39），where 应通过，got %v", res.Errors)
	}
}

func TestPurityLearnIsImpure(t *testing.T) {
	src := prelude + `
fn leak(n: Int) -> Int {
  learn(n)
  n
}
place Bar at local where leak(1) >= 1
`
	res := parseCheck(t, src)
	err, ok := findCode(res, diag.CodeWhereNotPure)
	if !ok {
		t.Fatalf("learn 必须使函数非纯（规则 35），got %v", codes(res))
	}
	if !strings.Contains(err.Msg, "learn") {
		t.Fatalf("message 须点明 learn 副作用，got %q", err.Msg)
	}
	if err.Pos != tokenPos(t, src, "leak", 1) {
		t.Fatalf("span = %s, want 调用处 leak token", err.Pos)
	}
}

func TestPurityWhitelistIsPure(t *testing.T) {
	src := prelude + `
fn check_it(n: Int) -> Bool { is_hazardous(n) }
fn key_of(n: Int) -> Int { hash_of(n) }
place Bar at local where check_it(1)
place Bar at local where key_of(1) >= 0
`
	res := parseCheck(t, src)
	if len(res.Errors) != 0 {
		t.Fatalf("is_hazardous / hash_of 应视为纯，got %v", res.Errors)
	}
}

func TestPurityExternalCallIsImpure(t *testing.T) {
	src := prelude + `
place Bar at local where mystery(1) >= 1
`
	res := parseCheck(t, src)
	if !hasCode(res, diag.CodeUnknownFn) || !hasCode(res, diag.CodeWhereNotPure) {
		t.Fatalf("外部调用应同时报未知函数与非纯，got %v", codes(res))
	}
}

// ---------------- 规则 37：emit 上下文 ----------------

func TestEmitContexts(t *testing.T) {
	t.Run("fn_illegal", func(t *testing.T) {
		res := parseCheck(t, "fn e() -> Unit {\n  emit 1\n}\n")
		if _, ok := findCode(res, diag.CodeEmitContext); !ok {
			t.Fatalf("fn block 禁止 emit（规则 37），got %v", codes(res))
		}
	})

	t.Run("neuron_handler_legal", func(t *testing.T) {
		res := parseCheck(t, "neuron N {\n  input i: Signal<Int>\n  handle {\n    emit i\n  }\n}\n")
		if len(res.Errors) != 0 {
			t.Fatalf("neuron handler 内 emit 合法，got %v", res.Errors)
		}
	})

	t.Run("do_block_in_handler_illegal", func(t *testing.T) {
		res := parseCheck(t, "neuron N2 {\n  input i: Signal<Int>\n  handle {\n    do {\n      emit i\n    }\n  }\n}\n")
		if _, ok := findCode(res, diag.CodeEmitContext); !ok {
			t.Fatalf("do block 禁止 emit（规则 37），got %v", codes(res))
		}
	})

	t.Run("capability_illegal", func(t *testing.T) {
		res := parseCheck(t, "logic L {\n  capability: { run() -> Unit { emit 1 } }\n}\n")
		if _, ok := findCode(res, diag.CodeEmitContext); !ok {
			t.Fatalf("capability 内 emit 非法，got %v", codes(res))
		}
	})
}

// ---------------- 规则 36：Range ----------------

func TestRangeStaysParserTerritory(t *testing.T) {
	// parser 阶段即拒绝（KNOWN-03），checker 不应重复报或崩
	src := "fn f() -> Unit {\n  let r: Range = 0..10\n}"
	_, perrs := parser.Parse(src)
	if len(perrs) != 1 || perrs[0].Code != diag.CodeRangeOutsideFor {
		t.Fatalf("want single E-RANGE-OUTSIDE-FOR from parser, got %v", perrs)
	}
}

// ---------------- 规则 28 / 29：端口 ----------------

func TestPortTypeMismatch(t *testing.T) {
	src := `
neuron A { output o: Signal<Int> }
neuron B { input i: Signal<String> }
synapse A.o -> B.i
`
	res := parseCheck(t, src)
	err, ok := findCode(res, diag.CodePortType)
	if !ok {
		t.Fatalf("want E-PORT-TYPE-MISMATCH, got %v", codes(res))
	}
	if !strings.Contains(err.Msg, "Signal<Int>") || !strings.Contains(err.Msg, "Signal<String>") {
		t.Fatalf("message 须给出两端类型，got %q", err.Msg)
	}
}

func TestPortMissingAndDirection(t *testing.T) {
	src := `
neuron A { output o: Signal<Int> }
neuron B { input i: Signal<Int> }
neuron C { input j: Signal<Int> }
synapse A.x -> B.i
synapse C.j -> B.i
synapse Z.o -> B.i
`
	res := parseCheck(t, src)
	counts := map[string]int{}
	for _, e := range res.Errors {
		counts[e.Code]++
	}
	if counts[diag.CodePortNotFound] != 1 || counts[diag.CodePortDirection] != 1 || counts[diag.CodeUnknownNeuron] != 1 {
		t.Fatalf("want 1 each of NOT-FOUND/DIRECTION/UNKNOWN-NEURON, got %v", res.Errors)
	}
}

func TestPortTypesMatchOK(t *testing.T) {
	src := `
neuron A { output o: Signal<Int> }
neuron B { input i: Signal<Int> }
synapse A.o -> B.i on_failure retry(3) failover failover
`
	res := parseCheck(t, src)
	if len(res.Errors) != 0 {
		t.Fatalf("want clean, got %v", res.Errors)
	}
}

// ---------------- N3 hazardous guard ----------------

func TestHazardousNeedsGuardOrStrict(t *testing.T) {
	bad := "logic H with hazardous {\n  structure: { x: Int }\n}\n"
	res := parseCheck(t, bad)
	err, ok := findCode(res, diag.CodeHazardNoGuard)
	if !ok {
		t.Fatalf("want E-HAZARD-NO-GUARD, got %v", codes(res))
	}
	if err.Pos != (diag.Pos{Line: 1, Col: 1}) {
		t.Fatalf("span = %s, want logic 关键字处 1:1", err.Pos)
	}

	for _, ok := range []string{
		"logic H with hazardous strict {\n  structure: { x: Int }\n}\n",
		"logic H with hazardous guard 1 > 0 {\n  structure: { x: Int }\n}\n",
	} {
		if res := parseCheck(t, ok); len(res.Errors) != 0 {
			t.Fatalf("hazardous + strict/guard 应通过，got %v", res.Errors)
		}
	}
}

// ---------------- 类型检查 ----------------

func TestTypeChecks(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{"let_mismatch", "fn f() -> Unit {\n  let x: Int = \"s\"\n}\n", diag.CodeTypeMismatch},
		{"assign_immutable", "fn f() -> Unit {\n  let x = 1\n  x = 2\n}\n", diag.CodeAssignImmutable},
		{"return_type", "fn f() -> Int {\n  \"s\"\n}\n", diag.CodeReturnType},
		{"missing_return", "fn f() -> Int {\n}\n", diag.CodeMissingReturn},
		{"arg_count", "fn a(x: Int) -> Unit { }\nfn b() -> Unit { a() }\n", diag.CodeArgCount},
		{"arg_type", "fn a(x: Int) -> Unit { }\nfn b() -> Unit { a(\"s\") }\n", diag.CodeArgType},
		{"unknown_fn", "fn b() -> Unit { mystery() }\n", diag.CodeUnknownFn},
		{"cond_not_bool", "fn f() -> Unit {\n  if 1 { }\n}\n", diag.CodeCondNotBool},
		{"for_not_range", "fn f() -> Unit {\n  for i in 1 { }\n}\n", diag.CodeForNotRange},
		{"continue_outside", "fn f() -> Unit {\n  continue\n}\n", diag.CodeContinueOutsideLoop},
		{"self_outside", "fn f() -> Unit {\n  self.n = 1\n}\n", diag.CodeSelfOutside},
		{"unknown_field", "struct S { a: Int }\nfn f(s: S) -> Unit {\n  let x = s.b\n}\n", diag.CodeUnknownField},
		{"unknown_ident", "fn f() -> Unit {\n  nope\n}\n", diag.CodeUnknownIdent},
		{"binary_mismatch", "fn f() -> Unit {\n  let x = 1 + \"s\"\n}\n", diag.CodeTypeMismatch},
		{"not_callable", "fn f() -> Unit {\n  let x = 1\n  x()\n}\n", diag.CodeNotCallable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parseCheck(t, c.src)
			if !hasCode(res, c.code) {
				t.Fatalf("want %s, got %v", c.code, res.Errors)
			}
		})
	}
}

// 隐式尾表达式（§17 前置块里 is_liquid / compute_min 就是这种写法）
func TestImplicitTailReturn(t *testing.T) {
	res := parseCheck(t, prelude)
	if len(res.Errors) != 0 {
		t.Fatalf("prelude 必须干净，got %v", res.Errors)
	}
	res = parseCheck(t, "fn f(b: Bool) -> Int {\n  if b { 1 } else { 2 }\n}\n")
	if len(res.Errors) != 0 {
		t.Fatalf("两侧同型的 if 尾表达式应通过，got %v", res.Errors)
	}
	res = parseCheck(t, "fn f(b: Bool) -> Int {\n  if b { 1 } else { \"s\" }\n}\n")
	if !hasCode(res, diag.CodeTypeMismatch) {
		t.Fatalf("两侧类型不同应报错，got %v", res.Errors)
	}
}

// ---------------- 声明 / instance / match ----------------

func TestDeclChecks(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{"dup_decl", "struct S { a: Int }\nstruct S { b: Int }\n", diag.CodeDupDecl},
		{"unknown_type", "fn f(x: Nope) -> Unit { }\n", diag.CodeUnknownType},
		{"dup_field", "struct S { a: Int; a: String }\n", diag.CodeDupField},
		{"instance_logic", "instance I : Nope { structure: { a: 1 } }\n", diag.CodeInstanceLogic},
		{"binding_missing", "logic L { structure: { a: Int; b: Int } capability: { run() -> Unit {} } }\ninstance I : L { structure: { a: 1 } }\n", diag.CodeBindingMissing},
		{"binding_extra", "logic L { structure: { a: Int } capability: { run() -> Unit {} } }\ninstance I : L { structure: { a: 1; c: 2 } }\n", diag.CodeBindingExtra},
		{"binding_type", "logic L { structure: { a: Int } capability: { run() -> Unit {} } }\ninstance I : L { structure: { a: \"s\" } }\n", diag.CodeBindingType},
		{"place_target", "place Nope at local where gpu_mem >= 1\n", diag.CodePlaceTarget},
		{"match_target", "logic L { structure: { a: Int } capability: { run() -> Unit {} } }\nfn f(x: L) -> Unit {\n  match x against Foo {\n    case L => 1\n  }\n}\n", diag.CodeMatchTarget},
		{"match_pattern", "logic L { structure: { a: Int } capability: { run() -> Unit {} } }\ninstance I : L { structure: { a: 1 } }\nstruct S { b: Int }\nfn f(x: L) -> Unit {\n  match x against LogicDB {\n    case S => 1\n  }\n}\n", diag.CodeMatchPattern},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parseCheck(t, c.src)
			if !hasCode(res, c.code) {
				t.Fatalf("want %s, got %v", c.code, res.Errors)
			}
		})
	}
}

// ---------------- unit_lit（规则 20 / N8）span ----------------

func TestUnitLitSpans(t *testing.T) {
	src := "fn f() -> Unit {\n  let x = -55mV\n  let y = 55mV\n}\n"
	res := parseCheck(t, src)
	if len(res.Errors) != 2 {
		t.Fatalf("want 2 diagnostics, got %v", res.Errors)
	}
	wantMinus := tokenPos(t, src, "-", 0)
	wantNum := tokenPos(t, src, "55", 1) // 第二个 55 属于 55mV
	got := map[string]bool{}
	for _, e := range res.Errors {
		if e.Code != diag.CodeUnitOutsidePhysics {
			t.Fatalf("code = %s", e.Code)
		}
		got[e.Pos.String()] = true
	}
	if !got[wantMinus.String()] {
		t.Fatalf("有 - 时 span 必须指向 -（%s），got %v", wantMinus, res.Errors)
	}
	if !got[wantNum.String()] {
		t.Fatalf("无 - 时 span 必须指向数值（%s），got %v", wantNum, res.Errors)
	}
}

// ---------------- where 标识符（N4 / N12） ----------------

func TestWhereIdentWhitelist(t *testing.T) {
	ok := prelude + "\nplace Bar at local where gpu_mem >= 1\n"
	if res := parseCheck(t, ok); len(res.Errors) != 0 {
		t.Fatalf("gpu_mem 是内建资源指标，应通过，got %v", res.Errors)
	}

	bad := prelude + "\nplace Bar at local where foo_bar >= 1\n"
	res := parseCheck(t, bad)
	err, found := findCode(res, diag.CodeUnknownWhereIdent)
	if !found {
		t.Fatalf("want E-UNKNOWN-WHERE-IDENT, got %v", codes(res))
	}
	if err.Pos != tokenPos(t, bad, "foo_bar", 0) {
		t.Fatalf("span = %s, want foo_bar token", err.Pos)
	}
}

// where 里可以引用目标 structure 的字段
func TestWhereTargetFields(t *testing.T) {
	src := prelude + "\nplace Bar at local where user_id >= 1\n"
	if res := parseCheck(t, src); len(res.Errors) != 0 {
		t.Fatalf("user_id 是 SomeLogic 的 structure 字段，应通过，got %v", res.Errors)
	}
}
