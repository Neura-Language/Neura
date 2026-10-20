package lexer

import (
	"testing"
)

// ---------- helpers ----------

func lexOK(t *testing.T, src string) []Token {
	t.Helper()
	toks, errs := Lex(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected lex errors for %q: %v", src, errs)
	}
	return toks
}

func noSemis(toks []Token) []Token {
	out := make([]Token, 0, len(toks))
	for _, t := range toks {
		if t.Kind != VirtualSemi {
			out = append(out, t)
		}
	}
	return out
}

func kindsOf(toks []Token) []Kind {
	out := make([]Kind, len(toks))
	for i, t := range toks {
		out[i] = t.Kind
	}
	return out
}

func lexemesOf(toks []Token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.Lexeme
	}
	return out
}

func countKind(toks []Token, k Kind) int {
	n := 0
	for _, t := range toks {
		if t.Kind == k {
			n++
		}
	}
	return n
}

func eqKinds(t *testing.T, got []Token, want []Kind) {
	t.Helper()
	g := kindsOf(got)
	if len(g) != len(want) {
		t.Fatalf("kind count = %d, want %d\n got=%v\nwant=%v", len(g), len(want), g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("kind[%d] = %s, want %s (got=%v want=%v)", i, g[i], want[i], g, want)
		}
	}
}

func eqLexemes(t *testing.T, got []Token, want []string) {
	t.Helper()
	g := lexemesOf(got)
	if len(g) != len(want) {
		t.Fatalf("lexeme count = %d, want %d\n got=%q\nwant=%q", len(g), len(want), g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("lexeme[%d] = %q, want %q (got=%q want=%q)", i, g[i], want[i], g, want)
		}
	}
}

// ---------- 验收要求的 5 个 case（虚拟分号另行单测） ----------

func TestRequiredCases(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		kinds  []Kind
		lexeme []string
	}{
		{"20ms->DurationLit", "20ms", []Kind{DurationLit, EOF}, []string{"20ms", ""}},
		{"1.5GB->SizeLit", "1.5GB", []Kind{SizeLit, EOF}, []string{"1.5GB", ""}},
		{"-55mV->[-,55,mV]", "-55mV", []Kind{Punct, IntLit, Ident, EOF}, []string{"-", "55", "mV", ""}},
		{"0..10->[0,..,10]", "0..10", []Kind{IntLit, Punct, IntLit, EOF}, []string{"0", "..", "10", ""}},
		{"Option\\n<Int> 不插 ;ᵥ", "Option\n<Int>", []Kind{Ident, Punct, Ident, Punct, EOF}, []string{"Option", "<", "Int", ">", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toks := noSemis(lexOK(t, c.src))
			eqKinds(t, toks, c.kinds)
			eqLexemes(t, toks, c.lexeme)
		})
	}
}

// 5 个 case 的插入行为也要钉死：只有 0..10 末尾那个 IntLit 会触发 ;ᵥ。
func TestRequiredCasesSemiStatus(t *testing.T) {
	if n := countKind(lexOK(t, "Option\n<Int>"), VirtualSemi); n != 0 {
		t.Fatalf("Option\\n<Int> should not insert ;ᵥ, got %d", n)
	}
	if n := countKind(lexOK(t, "0..10"), VirtualSemi); n != 1 {
		t.Fatalf("0..10 should insert exactly 1 ;ᵥ at EOF, got %d", n)
	}
	// .. 不得被拆成 . .，也不得参与字段访问
	for _, tok := range lexOK(t, "0..10") {
		if tok.Kind == Punct && tok.Lexeme == "." {
			t.Fatalf("'.' leaked: .. was split into two dots")
		}
	}
}

// ---------- 单位 / 数字 ----------

func TestUnitsAndNumbers(t *testing.T) {
	cases := []struct {
		src  string
		kind Kind
	}{
		{"20ms", DurationLit},
		{"3s", DurationLit},
		{"500us", DurationLit},
		{"100ns", DurationLit},
		{"2h", DurationLit},
		{"1.5GB", SizeLit},
		{"0GB", SizeLit},
		{"512KiB", SizeLit},
		{"8MiB", SizeLit},
		{"42", IntLit},
		{"3.14", FloatLit},
		{"1e9", FloatLit},
	}
	for _, c := range cases {
		toks := lexOK(t, c.src)
		if toks[0].Kind != c.kind {
			t.Errorf("%q kind = %s, want %s", c.src, toks[0].Kind, c.kind)
		}
		if toks[0].Lexeme != c.src {
			t.Errorf("%q lexeme = %q, want whole text", c.src, toks[0].Lexeme)
		}
	}
}

// 单位完整匹配失败必须整段回退，不许部分匹配后回退（20m(V) 之类）。
func TestUnitNoPartialBacktrack(t *testing.T) {
	toks := lexOK(t, "55mV")
	eqKinds(t, noSemis(toks), []Kind{IntLit, Ident, EOF})

	toks = lexOK(t, "20msx")
	eqKinds(t, noSemis(toks), []Kind{IntLit, Ident, EOF})
	if toks[1].Lexeme != "msx" {
		t.Fatalf("want ident msx, got %q", toks[1].Lexeme)
	}
}

// ---------- ..= / ~= / -> ----------

func TestRangeAndArrowAndMatchOps(t *testing.T) {
	toks := noSemis(lexOK(t, "0..=10"))
	eqKinds(t, toks, []Kind{IntLit, Punct, IntLit, EOF})
	if toks[1].Lexeme != "..=" {
		t.Fatalf("want ..=, got %q", toks[1].Lexeme)
	}

	toks = noSemis(lexOK(t, "x ~= y"))
	eqKinds(t, toks, []Kind{Ident, Punct, Ident, EOF})
	if toks[1].Lexeme != "~=" {
		t.Fatalf("want ~=, got %q", toks[1].Lexeme)
	}

	toks = noSemis(lexOK(t, "Src.o -> Dst.i"))
	eqKinds(t, toks, []Kind{QualifiedIdent, Punct, QualifiedIdent, EOF})
	eqLexemes(t, toks, []string{"Src.o", "->", "Dst.i", ""})
}

// .. 不参与 postfix 字段访问：a..b 不能合成 QualifiedIdent
func TestRangeNotFieldAccess(t *testing.T) {
	toks := noSemis(lexOK(t, "a..b"))
	eqKinds(t, toks, []Kind{Ident, Punct, Ident, EOF})
	eqLexemes(t, toks, []string{"a", "..", "b", ""})
}

// ---------- QualifiedIdent ----------

func TestQualifiedIdent(t *testing.T) {
	toks := noSemis(lexOK(t, "SomeLogic.run"))
	eqKinds(t, toks, []Kind{QualifiedIdent, EOF})
	if toks[0].Lexeme != "SomeLogic.run" || toks[0].FirstSegment() != "SomeLogic" {
		t.Fatalf("bad qualified ident: %+v", toks[0])
	}

	// 数字点后不合成（x.0 之类留给 parser 处理）
	toks = noSemis(lexOK(t, "x.0"))
	eqKinds(t, toks, []Kind{Ident, Punct, IntLit, EOF})
}

// ---------- 关键字 / 内建类型名 ----------

func TestKeywords(t *testing.T) {
	src := "fn let mut if else for in match case against return break continue do " +
		"struct logic instance neuron synapse place at with where hazardous strict guard " +
		"emit invoke reuse learn output input capability structure local balance failover retry on_failure"
	toks := noSemis(lexOK(t, src))
	for _, tok := range toks {
		if tok.Kind == EOF {
			continue
		}
		if tok.Kind != Keyword {
			t.Errorf("%q kind = %s, want Keyword", tok.Lexeme, tok.Kind)
		}
	}
	// 标识符不是关键字
	if lexOK(t, "foo")[0].Kind != Ident {
		t.Fatal("foo should be Ident")
	}
}

func TestBuiltinTypesAreIdentsAndExcludedFromTerminators(t *testing.T) {
	if len(BuiltinTypes) != 20 {
		t.Fatalf("builtin type count = %d, want 20 (B-5)", len(BuiltinTypes))
	}
	for name := range BuiltinTypes {
		// 内建类型名 -> Ident
		toks := lexOK(t, name)
		if toks[0].Kind != Ident {
			t.Errorf("%q kind = %s, want Ident", name, toks[0].Kind)
		}
		if !toks[0].IsBuiltinType() {
			t.Errorf("%q IsBuiltinType = false", name)
		}
		// 行尾不触发 ;ᵥ
		toks = lexOK(t, "let x: "+name+"\nlet y: "+name)
		if n := countKind(toks, VirtualSemi); n != 0 {
			t.Errorf("builtin %q should not terminate statement, got %d ;ᵥ", name, n)
		}
	}
	// KNOWN-03：Range 不是内建类型名，也不是关键字（for 专用语法糖）
	toks := lexOK(t, "Range")
	if toks[0].Kind != Ident || BuiltinTypes["Range"] {
		t.Fatalf("Range must be plain Ident and absent from prim_type, got %s", toks[0].Kind)
	}
}

// ---------- 虚拟分号 ----------

func TestVirtualSemiInsertion(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int // 期望 ;ᵥ 个数
	}{
		{"int_at_eol", "let x = 1\nlet y = 2", 2},
		{"same_line_no_semi", "let x = 1 let y = 2", 1}, // 只有 EOF 处那一个
		{"after_rparen", "foo()\nbar()", 2},
		{"after_rbrace", "fn f() -> Unit {}\nfn g() -> Unit {}", 2},
		{"after_ident", "x\ny", 2},
		{"blank_lines", "let x = 1\n\n\nlet y = 2", 2},
		{"eof_terminator", "let x = 1", 1},
		{"eof_non_terminator", "fn f() -> Unit {", 0},
		{"keyword_return", "return\nx", 2},
		{"semi_lexeme_and_pos", "let x = 1\nlet y = 2", 2},
		{"explicit_semi_no_extra", "let x = 1;\nlet y = 2;", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := countKind(lexOK(t, c.src), VirtualSemi); n != c.want {
				t.Fatalf(";ᵥ count = %d, want %d (src=%q)", n, c.want, c.src)
			}
		})
	}

	// ;ᵥ 的 lexeme 与位置
	toks := lexOK(t, "let x = 1\nlet y = 2")
	for _, tok := range toks {
		if tok.Kind == VirtualSemi {
			if tok.Lexeme != ";ᵥ" {
				t.Fatalf("virtual semi lexeme = %q, want ;ᵥ", tok.Lexeme)
			}
			if tok.Line != 1 || tok.Col != 10 {
				t.Fatalf("first ;ᵥ at %d:%d, want 1:10", tok.Line, tok.Col)
			}
			break
		}
	}
}

// §1.3 五条例外：行首 token 抑制插入。
func TestSuppressRules(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		// 1. ( [ { ,
		{"lparen", "x\n(y)"},
		{"lbracket", "x\n[y]"},
		{"lbrace", "x\n{y}"},
		{"comma", "x\n, y"},
		// 2. 二元运算符
		{"plus", "x\n+ y"},
		{"minus", "x\n- y"},
		{"star", "x\n* y"},
		{"slash", "x\n/ y"},
		{"percent", "x\n% y"},
		{"eqeq", "x\n== y"},
		{"neq", "x\n!= y"},
		{"lt", "x\n< y"},
		{"le", "x\n<= y"},
		{"gt", "x\n> y"},
		{"ge", "x\n>= y"},
		{"andand", "x\n&& y"},
		{"oror", "x\n|| y"},
		// 3. .
		{"dot", "x\n.y"},
		// 4. =（B-2 跨行赋值）
		{"assign", "self.last\n  = Some(e)"},
		// 6. 行首 else（决策 6 新增例外）
		{"else_after_block", "if x { y }\nelse { z }"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toks := lexOK(t, c.src)
			// 只看「第 2 行第一个 token 之前」是否被插了 ;ᵥ（行尾/EOF 的合法插入不算）
			k := -1
			for i, tok := range toks {
				if tok.Line == 2 && tok.Kind != VirtualSemi {
					k = i
					break
				}
			}
			if k < 0 {
				t.Fatalf("no token on line 2: %v", toks)
			}
			for i := 0; i < k; i++ {
				if toks[i].Kind == VirtualSemi {
					t.Fatalf(";ᵥ should be suppressed before line-leading %q, got %v", c.src, toks)
				}
			}
		})
	}
}

// 例外 5：注释行不触发插入；块注释跨行不影响行号。
func TestComments(t *testing.T) {
	src := "let x = 1\n// 只有注释\nlet y = 2"
	toks := lexOK(t, src)
	if n := countKind(toks, VirtualSemi); n != 2 {
		t.Fatalf("comment-only line must not add ;ᵥ, got %d", n)
	}

	src = "// head\nlet x = \"a\\\"b\"\n/* c1\n   c2 */\nlet y = 2"
	toks = lexOK(t, src)
	var y *Token
	for i := range toks {
		if toks[i].Lexeme == "y" {
			y = &toks[i]
		}
	}
	if y == nil {
		t.Fatal("y not found")
	}
	if y.Line != 5 || y.Col != 5 {
		t.Fatalf("y at %d:%d, want 5:5", y.Line, y.Col)
	}
	if countKind(toks, VirtualSemi) != 2 {
		t.Fatalf("want 2 ;ᵥ, got %d", countKind(toks, VirtualSemi))
	}
}

// ---------- KNOWN-05 列号基准 ----------

func TestColumnBasis(t *testing.T) {
	toks := noSemis(lexOK(t, "  let x = -55mV"))
	//   col: 1-2 空格, 3 let, 7 x, 9 =, 11 -, 12 55, 14 mV
	want := []struct {
		lex string
		col int
		ln  int
	}{
		{"let", 3, 1}, {"x", 7, 1}, {"=", 9, 1}, {"-", 11, 1}, {"55", 12, 1}, {"mV", 14, 1},
	}
	for _, w := range want {
		found := false
		for _, tok := range toks {
			if tok.Lexeme == w.lex {
				found = true
				if tok.Col != w.col || tok.Line != w.ln {
					t.Fatalf("%q at %d:%d, want %d:%d", w.lex, tok.Line, tok.Col, w.ln, w.col)
				}
				break
			}
		}
		if !found {
			t.Fatalf("%q not found in %v", w.lex, toks)
		}
	}

	// 规则 20 的无负号情形：55 在 col 11
	toks = noSemis(lexOK(t, "  let x = 55mV"))
	for _, tok := range toks {
		if tok.Lexeme == "55" {
			if tok.Col != 11 {
				t.Fatalf("55 at col %d, want 11", tok.Col)
			}
			return
		}
	}
	t.Fatal("55 not found")
}

// ---------- 错误路径（Phase 1 只报原始行列，不填空 span） ----------

func TestIllegal(t *testing.T) {
	toks, errs := Lex("let x = @")
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	if errs[0].Line != 1 || errs[0].Col != 9 {
		t.Fatalf("error at %d:%d, want 1:9", errs[0].Line, errs[0].Col)
	}
	if countKind(toks, Illegal) != 1 {
		t.Fatalf("want 1 Illegal token, got %v", toks)
	}

	if _, errs := Lex("let x = \"abc"); len(errs) != 1 {
		t.Fatalf("want unterminated string error, got %v", errs)
	}
}

// ---------- Options ----------

func TestOptions(t *testing.T) {
	toks, _ := LexWithOptions("Src.o", Options{})
	eqKinds(t, noSemis(toks), []Kind{Ident, Punct, Ident, EOF})

	toks, _ = LexWithOptions("let x = 1\nlet y = 2", Options{JoinQualifiedIdents: true})
	if countKind(toks, VirtualSemi) != 0 {
		t.Fatal("InsertVirtualSemi=false should emit no ;ᵥ")
	}
}

// N1 的前提：同行多语句 lexer 不插 ;ᵥ，分隔留给 parser 报 E-SAME-LINE-STMT。
func TestSameLineStatementsKeepNoSeparator(t *testing.T) {
	toks := lexOK(t, "let x = 1 let y = 2")
	for i, tok := range toks {
		if tok.Kind != VirtualSemi {
			continue
		}
		if i == 0 || toks[i-1].Lexeme != "2" {
			t.Fatalf(";ᵥ inserted mid-line: %v", toks)
		}
	}
}
