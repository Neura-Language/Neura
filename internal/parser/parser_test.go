package parser

import (
	"os"
	"strings"
	"testing"

	"neura/internal/ast"
	"neura/internal/astdump"
	"neura/internal/diag"
	"neura/internal/lexer"
)

// ---------------- helpers ----------------

func parseOK(t *testing.T, src string) *ast.File {
	t.Helper()
	f, errs := Parse(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected diagnostics for %q:\n%v", src, errs)
	}
	return f
}

// expectSingle：只允许 1 条诊断（同时验证错误恢复没有级联）。
func expectSingle(t *testing.T, src string) diag.Error {
	t.Helper()
	_, errs := Parse(src)
	if len(errs) == 0 {
		t.Fatalf("expected a diagnostic for %q, got none", src)
	}
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 diagnostic for %q, got %d:\n%v", src, len(errs), errs)
	}
	return errs[0]
}

func tokenPos(t *testing.T, src string, n int, is func(lexer.Token) bool) diag.Pos {
	t.Helper()
	toks, _ := lexer.Lex(src)
	k := 0
	for _, tok := range toks {
		if is(tok) {
			if k == n {
				return diag.Pos{Line: tok.Line, Col: tok.Col}
			}
			k++
		}
	}
	t.Fatalf("token #%d not found in %q", n, src)
	return diag.Pos{}
}

func lexeme(s string) func(lexer.Token) bool {
	return func(t lexer.Token) bool { return t.Lexeme == s }
}

func fnDecl(t *testing.T, f *ast.File, i int) *ast.FnDecl {
	t.Helper()
	if i >= len(f.Decls) {
		t.Fatalf("no decl #%d", i)
	}
	fn, ok := f.Decls[i].(*ast.FnDecl)
	if !ok {
		t.Fatalf("decl #%d is %T, want *ast.FnDecl", i, f.Decls[i])
	}
	return fn
}

// ---------------- type 位置四分派 / tuple ----------------

func TestTypeFourWayDispatch(t *testing.T) {
	src := "fn t(a: (Int, String), b: (), c: Option<Int>, d: fn(Int) -> Int) -> Unit {}"
	f := parseOK(t, src)
	fn := fnDecl(t, f, 0)
	if len(fn.Params) != 4 {
		t.Fatalf("params = %d, want 4", len(fn.Params))
	}

	tup, ok := fn.Params[0].Type.(*ast.TupleType)
	if !ok || len(tup.Elems) != 2 {
		t.Fatalf("a: want TupleType(2), got %T %+v", fn.Params[0].Type, fn.Params[0].Type)
	}
	unit, ok := fn.Params[1].Type.(*ast.TupleType)
	if !ok || len(unit.Elems) != 0 {
		t.Fatalf("b: want TupleType(0), got %T", fn.Params[1].Type)
	}
	named, ok := fn.Params[2].Type.(*ast.NamedType)
	if !ok || named.Name != "Option" || len(named.Args) != 1 {
		t.Fatalf("c: want NamedType Option<Int>, got %+v", fn.Params[2].Type)
	}
	fnt, ok := fn.Params[3].Type.(*ast.FnType)
	if !ok || len(fnt.Params) != 1 {
		t.Fatalf("d: want FnType, got %T", fn.Params[3].Type)
	}
	if res, ok := fnt.Result.(*ast.NamedType); !ok || res.Name != "Int" {
		t.Fatalf("d: want -> Int, got %+v", fnt.Result)
	}
}

// N7：fn g(p: (Int)) -> Unit {} —— 单元素括号在类型位置非法，span 指向 "("。
func TestParenSingleTypeRejected(t *testing.T) {
	src := "fn g(p: (Int)) -> Unit {}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeParenSingleType {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeParenSingleType)
	}
	want := tokenPos(t, src, 1, lexeme("(")) // 第二个 "(" 就是类型位置的括号
	if err.Pos != want {
		t.Fatalf("span = %s, want %s (来自 lexer 实际 token)", err.Pos, want)
	}
	if !strings.Contains(err.Msg, "单元素括号在类型位置非法") {
		t.Fatalf("message not readable: %q", err.Msg)
	}
}

// ---------------- 语句分隔 ----------------

// N1：同为一条语句行内紧跟下一条语句 → E-SAME-LINE-STMT，span 指向第 2 条语句的起始 token。
func TestSameLineStmt(t *testing.T) {
	src := "fn f1() -> Unit {\n  let x = 1 let y = 2\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeSameLineStmt {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeSameLineStmt)
	}
	want := tokenPos(t, src, 1, lexeme("let")) // 第二个 let
	if err.Pos != want {
		t.Fatalf("span = %s, want %s (第 2 条语句起始)", err.Pos, want)
	}
}

// ;ᵥ 与 ; 等价，显式 ; 之后的同行语句不报错。
func TestSemiEquivalence(t *testing.T) {
	withSemi := parseOK(t, "fn f() -> Unit {\n  let x = 1;\n  let y = 2;\n}")
	withNewline := parseOK(t, "fn f() -> Unit {\n  let x = 1\n  let y = 2\n}")
	sameLine := parseOK(t, "fn f() -> Unit {\n  let x = 1; let y = 2\n}")

	for name, f := range map[string]*ast.File{"semi": withSemi, "newline": withNewline, "sameLine": sameLine} {
		body := fnDecl(t, f, 0).Body
		if len(body.Stmts) != 2 {
			t.Fatalf("%s: stmts = %d, want 2", name, len(body.Stmts))
		}
		for i, s := range body.Stmts {
			if _, ok := s.(*ast.LetStmt); !ok {
				t.Fatalf("%s: stmt[%d] = %T, want LetStmt", name, i, s)
			}
		}
	}
}

// ---------------- failure_policy / failover_policy ----------------

// N9：place ... failover retry(3) → failover 位置出现 retry，E-WRONG-POLICY。
func TestWrongPolicyRetryInFailover(t *testing.T) {
	src := `place Bar at "a" failover retry(3)`
	err := expectSingle(t, src)
	if err.Code != diag.CodeWrongPolicy {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeWrongPolicy)
	}
	want := tokenPos(t, src, 0, lexeme("retry"))
	if err.Pos != want {
		t.Fatalf("span = %s, want %s", err.Pos, want)
	}
	if !strings.Contains(err.Msg, "retry 属于 failure_policy") {
		t.Fatalf("message = %q", err.Msg)
	}
}

// N10：synapse ... on_failure failover → on_failure 位置出现 failover，E-WRONG-POLICY。
func TestWrongPolicyFailoverInOnFailure(t *testing.T) {
	src := "synapse Src.o -> Dst.i on_failure failover"
	err := expectSingle(t, src)
	if err.Code != diag.CodeWrongPolicy {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeWrongPolicy)
	}
	want := tokenPos(t, src, 0, lexeme("failover"))
	if err.Pos != want {
		t.Fatalf("span = %s, want %s", err.Pos, want)
	}
	if !strings.Contains(err.Msg, "failover 属于 failover_policy") {
		t.Fatalf("message = %q", err.Msg)
	}
}

func TestPoliciesAcceptedInRightPlace(t *testing.T) {
	src := "synapse Src.o -> Dst.i on_failure retry(3) failover failover"
	f := parseOK(t, src)
	sy := f.Decls[0].(*ast.SynapseDecl)
	if sy.Src.Parts[0] != "Src" || sy.Src.Parts[1] != "o" {
		t.Fatalf("src = %+v", sy.Src)
	}
	if sy.Dst.Parts[0] != "Dst" || sy.Dst.Parts[1] != "i" {
		t.Fatalf("dst = %+v", sy.Dst)
	}
	if sy.OnFailure == nil || sy.OnFailure.Kind != "retry" || len(sy.OnFailure.Args) != 1 {
		t.Fatalf("on_failure = %+v", sy.OnFailure)
	}
	if sy.FailoverP == nil || sy.FailoverP.Kind != "failover" {
		t.Fatalf("failover = %+v", sy.FailoverP)
	}
}

// ---------------- Range（KNOWN-03 / 规则 36） ----------------

func TestRangeTypeRejected(t *testing.T) {
	src := "fn f() -> Unit {\n  let r: Range = 0..10\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeRangeOutsideFor {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeRangeOutsideFor)
	}
	want := tokenPos(t, src, 0, lexeme("Range"))
	if err.Pos != want {
		t.Fatalf("span = %s, want %s", err.Pos, want)
	}
}

func TestRangeExprOutsideForRejected(t *testing.T) {
	src := "fn f() -> Unit {\n  let r = 0..10\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeRangeOutsideFor {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeRangeOutsideFor)
	}
	want := tokenPos(t, src, 0, lexeme(".."))
	if err.Pos != want {
		t.Fatalf("span = %s, want %s (指向 .. token)", err.Pos, want)
	}
}

func TestRangeInForAccepted(t *testing.T) {
	f := parseOK(t, "fn f() -> Unit {\n  for i in 0..10 { }\n  for j in 0..=3 { }\n}")
	body := fnDecl(t, f, 0).Body
	if len(body.Stmts) != 2 {
		t.Fatalf("stmts = %d, want 2", len(body.Stmts))
	}
	for i, want := range []bool{false, true} {
		fs, ok := body.Stmts[i].(*ast.ForStmt)
		if !ok {
			t.Fatalf("stmt[%d] = %T, want ForStmt", i, body.Stmts[i])
		}
		r, ok := fs.Iter.(*ast.RangeExpr)
		if !ok {
			t.Fatalf("iter[%d] = %T, want RangeExpr", i, fs.Iter)
		}
		if r.Inclusive != want {
			t.Fatalf("iter[%d].Inclusive = %v, want %v", i, r.Inclusive, want)
		}
	}
}

// ---------------- E-ASSIGN-LHS（KNOWN-06） ----------------

func TestAssignLHSKnownMessage(t *testing.T) {
	src := "fn f() -> Unit {\n  let x = 1 = 2\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeAssignLHS {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeAssignLHS)
	}
	const want = "= 左侧必须是变量，不能是字面量 1（是否漏了 ; ？）"
	if err.Msg != want {
		t.Fatalf("message = %q\nwant      %q", err.Msg, want)
	}
}

func TestAssignToIdentAndFieldOK(t *testing.T) {
	src := "fn f(e: SomeLogic) -> Unit {\n  let mut x = 1\n  x = 2\n  e.last = 3\n}"
	parseOK(t, src)
}

// ---------------- struct field vs instance binding ----------------

func TestStructFieldIsTypeInstanceBindingIsExpr(t *testing.T) {
	src := "struct S { a: Int }\ninstance I : S { structure: { a: 42 } }"
	f := parseOK(t, src)
	if len(f.Decls) != 2 {
		t.Fatalf("decls = %d, want 2", len(f.Decls))
	}

	sd := f.Decls[0].(*ast.StructDecl)
	if len(sd.Fields) != 1 {
		t.Fatalf("fields = %d", len(sd.Fields))
	}
	if nt, ok := sd.Fields[0].Type.(*ast.NamedType); !ok || nt.Name != "Int" {
		t.Fatalf("struct field type = %T %+v, want NamedType Int", sd.Fields[0].Type, sd.Fields[0].Type)
	}

	id := f.Decls[1].(*ast.InstanceDecl)
	if id.Logic != "S" || len(id.Bindings) != 1 {
		t.Fatalf("instance = %+v", id)
	}
	b := id.Bindings[0]
	if b.Name != "a" {
		t.Fatalf("binding name = %q", b.Name)
	}
	lit, ok := b.Value.(*ast.Literal)
	if !ok || lit.Kind != "int" || lit.Text != "42" {
		t.Fatalf("binding value = %T %+v, want int literal 42", b.Value, b.Value)
	}
}

// struct 里写值（binding 语法）应当报 E-EXPECTED-TYPE，反证两者不混用。
func TestStructRejectsValueSyntax(t *testing.T) {
	src := "struct S { a: 42 }"
	err := expectSingle(t, src)
	if err.Code != diag.CodeExpectedType {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeExpectedType)
	}
}

// ---------------- logic_arm_rhs 五分派 ----------------

func TestLogicArmRHSFiveWay(t *testing.T) {
	src := `fn d(x: SomeLogic) -> Unit {
  match x against LogicDB {
    case A(e) => { invoke e.run() }
    case B => do { emit x }
    case C => reuse x
    case D => invoke x.run()
    case E => emit x
    case F => 1
  }
}`
	f := parseOK(t, src)
	m, ok := fnDecl(t, f, 0).Body.Stmts[0].(*ast.MatchStmt)
	if !ok {
		t.Fatalf("stmt = %T, want MatchStmt", fnDecl(t, f, 0).Body.Stmts[0])
	}
	if len(m.Arms) != 6 {
		t.Fatalf("arms = %d, want 6", len(m.Arms))
	}

	if _, ok := m.Arms[0].Body.(*ast.DoStmt); !ok {
		t.Fatalf("arm0 = %T, want DoStmt（{ invoke ... }）", m.Arms[0].Body)
	}
	if _, ok := m.Arms[1].Body.(*ast.DoStmt); !ok {
		t.Fatalf("arm1 = %T, want DoStmt（do { emit ... }）", m.Arms[1].Body)
	}
	if _, ok := m.Arms[2].Body.(*ast.ReuseStmt); !ok {
		t.Fatalf("arm2 = %T, want ReuseStmt", m.Arms[2].Body)
	}
	if _, ok := m.Arms[3].Body.(*ast.InvokeStmt); !ok {
		t.Fatalf("arm3 = %T, want InvokeStmt", m.Arms[3].Body)
	}
	if _, ok := m.Arms[4].Body.(*ast.EmitStmt); !ok {
		t.Fatalf("arm4 = %T, want EmitStmt", m.Arms[4].Body)
	}
	if _, ok := m.Arms[5].Body.(*ast.ExprStmt); !ok {
		t.Fatalf("arm5 = %T, want ExprStmt", m.Arms[5].Body)
	}

	if m.Arms[0].Bind != "e" || len(m.Arms[0].Pattern.Parts) != 1 {
		t.Fatalf("arm0 pattern/bind = %+v / %q", m.Arms[0].Pattern, m.Arms[0].Bind)
	}

	// do-block 里是 InvokeStmt
	body := m.Arms[0].Body.(*ast.DoStmt)
	if len(body.Body.Stmts) != 1 {
		t.Fatalf("do-block stmts = %d", len(body.Body.Stmts))
	}
	if _, ok := body.Body.Stmts[0].(*ast.InvokeStmt); !ok {
		t.Fatalf("do-block stmt = %T, want InvokeStmt", body.Body.Stmts[0])
	}
}

// ---------------- hazardous（N2 / N3） ----------------

// N2：logic Bad1 hazardous { ... } → E-HAZARD-NO-WITH，span 指向 hazardous。
func TestHazardWithoutWith(t *testing.T) {
	src := "logic Bad1 hazardous {\n  structure: { x: Int }\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeHazardNoWith {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeHazardNoWith)
	}
	want := tokenPos(t, src, 0, lexeme("hazardous"))
	if err.Pos != want {
		t.Fatalf("span = %s, want %s", err.Pos, want)
	}
}

// N3：logic Bad2 with hazardous { ... } 语法合法，E-HAZARD-NO-GUARD 属 Phase 3。
func TestHazardWithParses(t *testing.T) {
	src := "logic Bad2 with hazardous {\n  structure: { x: Int }\n}"
	f := parseOK(t, src)
	ld := f.Decls[0].(*ast.LogicDecl)
	if len(ld.Modifiers) != 1 || ld.Modifiers[0] != "hazardous" {
		t.Fatalf("modifiers = %v, want [hazardous]", ld.Modifiers)
	}
	if ld.Strict {
		t.Fatal("Strict should be false")
	}
	if len(ld.Structure) != 1 || ld.Structure[0].Name != "x" {
		t.Fatalf("structure = %+v", ld.Structure)
	}
}

// ---------------- parser 不该越权的两处 ----------------

// N11：break 在循环外的判定属 checker；parser 必须照常解析。
func TestBreakOutsideLoopParses(t *testing.T) {
	parseOK(t, "fn f2() -> Unit {\n  break\n}")
	parseOK(t, "fn f3() -> Unit {\n  continue\n}")
}

// N4/N5/N12：where / consistent_hash 里的未知标识符属 checker；parser 只建 AST。
func TestWhereIdentsAreNotResolvedByParser(t *testing.T) {
	src := `logic L { structure: { a: Int } }
place L at local where foo_bar >= 1
place L at ["a","b"] balance consistent_hash(nonexistent)
place L at local where gpu_mem >= bad_fn()`
	f := parseOK(t, src)
	if len(f.Decls) != 4 {
		t.Fatalf("decls = %d, want 4", len(f.Decls))
	}
	p1 := f.Decls[1].(*ast.PlaceDecl)
	if !p1.AtSpec.Local || p1.Where == nil {
		t.Fatalf("place#1 = %+v", p1)
	}
	if _, ok := p1.Where.(*ast.BinaryExpr); !ok {
		t.Fatalf("where = %T, want BinaryExpr", p1.Where)
	}
	p2 := f.Decls[2].(*ast.PlaceDecl)
	if p2.Balance == nil || p2.Balance.Kind != "consistent_hash" || len(p2.Balance.Args) != 1 {
		t.Fatalf("balance = %+v", p2.Balance)
	}
	if len(p2.AtSpec.Nodes) != 2 || p2.AtSpec.Nodes[0] != "a" {
		t.Fatalf("at nodes = %v", p2.AtSpec.Nodes)
	}
}

// ---------------- else 换行（决策 6） ----------------

func TestElseOnOwnLine(t *testing.T) {
	src := "fn f(n: Int) -> Int {\n  if n > 0 {\n    n\n  }\n  else {\n    0\n  }\n}"
	f := parseOK(t, src)
	iff, ok := fnDecl(t, f, 0).Body.Stmts[0].(*ast.IfStmt)
	if !ok {
		t.Fatalf("stmt = %T, want IfStmt", fnDecl(t, f, 0).Body.Stmts[0])
	}
	if iff.Else == nil {
		t.Fatal("else branch missing")
	}
	if _, ok := iff.Else.(*ast.Block); !ok {
		t.Fatalf("else = %T, want *ast.Block", iff.Else)
	}
}

func TestElseIfChain(t *testing.T) {
	src := "fn f(n: Int) -> Int {\n" +
		"  if n < 0 {\n    0\n  } else if n == 0 {\n    1\n  } else {\n    2\n  }\n}"
	f := parseOK(t, src)
	iff := fnDecl(t, f, 0).Body.Stmts[0].(*ast.IfStmt)
	if _, ok := iff.Else.(*ast.IfStmt); !ok {
		t.Fatalf("else = %T, want *ast.IfStmt", iff.Else)
	}
}

// ---------------- unit_lit 的 AST 形态 ----------------

func TestUnitExprAST(t *testing.T) {
	src := "fn f() -> Unit {\n  let x = -55mV\n  let y = 55mV\n}"
	f := parseOK(t, src)
	body := fnDecl(t, f, 0).Body

	l0 := body.Stmts[0].(*ast.LetStmt)
	un, ok := l0.Value.(*ast.UnaryExpr)
	if !ok || un.Op != "-" {
		t.Fatalf("stmt0 value = %T, want UnaryExpr(-)", l0.Value)
	}
	u, ok := un.X.(*ast.UnitExpr)
	if !ok || u.Unit != "mV" {
		t.Fatalf("stmt0 inner = %T, want UnitExpr mV", un.X)
	}
	if lit, ok := u.Value.(*ast.Literal); !ok || lit.Text != "55" {
		t.Fatalf("unit value = %+v", u.Value)
	}

	l1 := body.Stmts[1].(*ast.LetStmt)
	if u2, ok := l1.Value.(*ast.UnitExpr); !ok || u2.Unit != "mV" {
		t.Fatalf("stmt1 value = %T, want UnitExpr mV", l1.Value)
	}
}

// ---------------- 错误码注册表 ----------------

func TestErrorRegistry(t *testing.T) {
	if len(diag.Registry) < 5 {
		t.Fatalf("registry has %d codes, want >= 5（Phase 2 验收）", len(diag.Registry))
	}
	seen := map[string]bool{}
	for _, r := range diag.Registry {
		if r.Code == "" || r.Msg == "" {
			t.Fatalf("bad registry entry: %+v", r)
		}
		if seen[r.Code] {
			t.Fatalf("duplicate code %s", r.Code)
		}
		seen[r.Code] = true
	}
	for _, must := range []string{
		diag.CodeSameLineStmt, diag.CodeParenSingleType, diag.CodeWrongPolicy,
		diag.CodeRangeOutsideFor, diag.CodeAssignLHS, diag.CodeHazardNoWith,
	} {
		if !seen[must] {
			t.Fatalf("%s missing from registry", must)
		}
	}
}

// ---------------- 完整示例 ----------------

func TestFullExampleParses(t *testing.T) {
	src, err := os.ReadFile("../../examples/full.neura")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	f, errs := Parse(string(src))
	if len(errs) != 0 {
		t.Fatalf("full.neura diagnostics:\n%v", errs)
	}
	if len(f.Decls) != 18 {
		t.Fatalf("decls = %d, want 18", len(f.Decls))
	}

	dump := astdump.String(f)
	for _, want := range []string{
		"FnDecl", "StructDecl", "LogicDecl", "InstanceDecl",
		"NeuronDecl", "SynapseDecl", "PlaceDecl", "MatchStmt", "ForStmt",
	} {
		if !strings.Contains(dump, want) {
			t.Fatalf("AST dump missing %s", want)
		}
	}
}

// AST dump 必须稳定可读：同一输入两次 dump 完全一致。
func TestDumpDeterministic(t *testing.T) {
	src := "fn f() -> Unit {\n  let x = 1\n  emit x\n}"
	a := astdump.String(parseOK(t, src))
	b := astdump.String(parseOK(t, src))
	if a != b {
		t.Fatal("dump is not deterministic")
	}
	if !strings.Contains(a, `Name: "f"`) {
		t.Fatalf("dump missing name:\n%s", a)
	}
}

// ---------------- 回归：struct 多行字段（无显式 ;） ----------------

func TestStructMultilineFields(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"no_semi", "struct S {\n  a: Int\n  b: Float\n}", []string{"a", "b"}},
		{"no_semi_user_like", "struct Substance {\n  amount: Float\n  temperature: Float\n}", []string{"amount", "temperature"}},
		{"blank_line", "struct S {\n  a: Int\n\n  b: Float\n}", []string{"a", "b"}},
		{"comment", "struct S {\n  // c\n  a: Int // t\n  b: Float\n}", []string{"a", "b"}},
		{"comma", "struct S {\n  a: Int,\n  b: Float,\n}", []string{"a", "b"}},
		{"semi", "struct S {\n  a: Int;\n  b: Float;\n}", []string{"a", "b"}},
		{"trailing_comma", "struct S {\n  a: Int,\n  b: Float,\n}", []string{"a", "b"}},
		{"crlf", "struct S {\r\n  a: Int\r\n  b: Float\r\n}\r\n", []string{"a", "b"}},
		{"brace_same_line", "struct S { a: Int\n  b: Float }", []string{"a", "b"}},
		{"soft_keyword_names", "struct S {\n  signal: Int\n  from: Float\n  on: Bool\n}", []string{"signal", "from", "on"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := parseOK(t, c.src)
			sd, ok := f.Decls[0].(*ast.StructDecl)
			if !ok {
				t.Fatalf("decl = %T, want *ast.StructDecl", f.Decls[0])
			}
			if len(sd.Fields) != len(c.want) {
				t.Fatalf("fields = %d, want %d (%+v)", len(sd.Fields), len(c.want), sd.Fields)
			}
			for i, name := range c.want {
				if sd.Fields[i].Name != name {
					t.Fatalf("field[%d] = %q, want %q", i, sd.Fields[i].Name, name)
				}
			}
		})
	}
}

func TestStructMultilineUserType(t *testing.T) {
	f := parseOK(t, "struct T { z: Int }\nstruct S {\n  t: T\n  n: Int\n}")
	if len(f.Decls) != 2 {
		t.Fatalf("decls = %d, want 2", len(f.Decls))
	}
	sd := f.Decls[1].(*ast.StructDecl)
	if len(sd.Fields) != 2 || sd.Fields[0].Name != "t" || sd.Fields[1].Name != "n" {
		t.Fatalf("fields = %+v", sd.Fields)
	}
}

// 字段后多一个 token（典型：写了默认值）要给出精确诊断，而不是「期望字段名」。
func TestStructFieldDefaultValueDiagnostic(t *testing.T) {
	src := "struct S {\n  a: Int = 1\n  b: Int\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeUnexpectedToken {
		t.Fatalf("code = %s", err.Code)
	}
	if !strings.Contains(err.Msg, "不能带默认值") {
		t.Fatalf("message = %q", err.Msg)
	}
	if err.Pos != tokenPos(t, src, 0, lexeme("=")) {
		t.Fatalf("span = %s, want = token", err.Pos)
	}
}

// 同一行两个字段必须用分隔符：报 E-SAME-LINE-STMT 而不是「期望字段名」。
func TestStructSameLineFieldsRejected(t *testing.T) {
	src := "struct S { a: Int b: Int }"
	err := expectSingle(t, src)
	if err.Code != diag.CodeSameLineStmt {
		t.Fatalf("code = %s, want %s", err.Code, diag.CodeSameLineStmt)
	}
	if err.Pos != tokenPos(t, src, 0, lexeme("b")) {
		t.Fatalf("span = %s, want 第 2 个字段名 b", err.Pos)
	}
}

// ---------------- on signal（EBNF） ----------------

func TestOnSignalHandler(t *testing.T) {
	src := `neuron N {
  input i: Signal<Int>
  output o: Signal<Bool>
  on signal amount: Float from tx {
    emit o
  }
}`
	f := parseOK(t, src)
	nd := f.Decls[0].(*ast.NeuronDecl)
	if len(nd.Ports) != 2 {
		t.Fatalf("ports = %d", len(nd.Ports))
	}
	if len(nd.Signals) != 1 {
		t.Fatalf("signals = %d, want 1", len(nd.Signals))
	}
	sh := nd.Signals[0]
	if sh.Name != "amount" {
		t.Fatalf("name = %q", sh.Name)
	}
	if sh.From != "tx" {
		t.Fatalf("from = %q", sh.From)
	}
	if nt, ok := sh.Type.(*ast.NamedType); !ok || nt.Name != "Float" {
		t.Fatalf("type = %T %+v", sh.Type, sh.Type)
	}
	if sh.Body == nil || len(sh.Body.Stmts) != 1 {
		t.Fatalf("body = %+v", sh.Body)
	}
	if _, ok := sh.Body.Stmts[0].(*ast.EmitStmt); !ok {
		t.Fatalf("body stmt = %T", sh.Body.Stmts[0])
	}
	if nd.Handler != nil {
		t.Fatal("on signal 不应写入 legacy Handler")
	}
}

func TestOnSignalMultipleAndQualifiedSource(t *testing.T) {
	src := `neuron N {
  on signal a: Float from tx { }
  on signal b: Int from db.events { }
}`
	f := parseOK(t, src)
	nd := f.Decls[0].(*ast.NeuronDecl)
	if len(nd.Signals) != 2 {
		t.Fatalf("signals = %d, want 2", len(nd.Signals))
	}
	if nd.Signals[1].From != "db.events" {
		t.Fatalf("from = %q, want db.events", nd.Signals[1].From)
	}
}

func TestOnSignalRequiresFrom(t *testing.T) {
	src := "neuron N {\n  on signal amount: Float { }\n}"
	err := expectSingle(t, src)
	if err.Code != diag.CodeUnexpectedToken || !strings.Contains(err.Msg, "from") {
		t.Fatalf("code=%s msg=%q", err.Code, err.Msg)
	}
}

// legacy `handle { }` 仍然可用（文档标注为别名）。
func TestLegacyHandleStillParses(t *testing.T) {
	f := parseOK(t, "neuron N {\n  input i: Signal<Int>\n  handle {\n    emit i\n  }\n}")
	nd := f.Decls[0].(*ast.NeuronDecl)
	if nd.Handler == nil || len(nd.Signals) != 0 {
		t.Fatalf("handler=%v signals=%d", nd.Handler, len(nd.Signals))
	}
}
