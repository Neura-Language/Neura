package lexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Options 控制两个可关闭的行为（Phase 2 调试时有用）。
type Options struct {
	JoinQualifiedIdents bool // Ident(.Ident)* 合成 QualifiedIdent（parser 按 . 再拆）
	InsertVirtualSemi   bool // 插入 ;ᵥ
}

func DefaultOptions() Options {
	return Options{JoinQualifiedIdents: true, InsertVirtualSemi: true}
}

// Error 是词法错误（原始行列，不填空 span）。
type Error struct {
	Line int
	Col  int
	Msg  string
}

func (e Error) Error() string { return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg) }

// Lex 是 Phase 1 主入口：输入源码，输出 token 流（含 ;ᵥ 与 EOF）和词法错误。
func Lex(src string) ([]Token, []Error) {
	return LexWithOptions(src, DefaultOptions())
}

func LexWithOptions(src string, opts Options) ([]Token, []Error) {
	l := &Lexer{src: src, line: 1, col: 1, opts: opts}
	raw := l.scanAll()
	if !opts.InsertVirtualSemi {
		return raw, l.errs
	}
	return insertVirtualSemis(raw), l.errs
}

type Lexer struct {
	src  string
	off  int // byte offset
	line int
	col  int // rune column, 1-based（KNOWN-05）
	opts Options
	errs []Error
}

func (l *Lexer) Errors() []Error { return l.errs }

// ---------- 低层扫描 ----------

func (l *Lexer) hasPrefix(p string) bool { return strings.HasPrefix(l.src[l.off:], p) }

func (l *Lexer) peekRune() rune {
	if l.off >= len(l.src) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.off:])
	return r
}

// advance 前进一个 rune 并维护 line/col。
func (l *Lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isIdentStart(r rune) bool {
	if r == '_' {
		return true
	}
	return unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	if r == '_' {
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isUnitLetter：单位串只由 ASCII 字母或 µ 组成，这样 55mV 的 "mV" 会整段取出再判定。
func isUnitLetter(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	return r == 'µ'
}

// skipTrivia 跳过空白与注释。注释不产生 token（§1.3 例外 5：注释行不触发 ;ᵥ）。
func (l *Lexer) skipTrivia() {
	for l.off < len(l.src) {
		switch l.src[l.off] {
		case ' ', '\t', '\r', '\n':
			l.advance()
		case '/':
			if l.hasPrefix("//") {
				for l.off < len(l.src) && l.src[l.off] != '\n' {
					l.advance()
				}
			} else if l.hasPrefix("/*") {
				startLine, startCol := l.line, l.col
				l.advance()
				l.advance()
				for l.off < len(l.src) && !l.hasPrefix("*/") {
					l.advance()
				}
				if l.hasPrefix("*/") {
					l.advance()
					l.advance()
				} else {
					l.errs = append(l.errs, Error{startLine, startCol, "unterminated block comment"})
				}
			} else {
				return // 除法运算符
			}
		default:
			return
		}
	}
}

func (l *Lexer) scanAll() []Token {
	// UTF-8 BOM 容忍
	if l.hasPrefix("\ufeff") {
		l.off += len("\ufeff")
	}
	toks := make([]Token, 0, 64)
	for {
		l.skipTrivia()
		if l.off >= len(l.src) {
			toks = append(toks, Token{Kind: EOF, Line: l.line, Col: l.col})
			return toks
		}
		r := l.peekRune()
		switch {
		case r == '"' || r == '\'':
			toks = append(toks, l.scanString(r))
		case isDigit(l.src[l.off]):
			toks = append(toks, l.scanNumber())
		case isIdentStart(r):
			toks = append(toks, l.scanIdent())
		default:
			if t, ok := l.scanPunct(); ok {
				toks = append(toks, t)
			} else {
				toks = append(toks, l.illegal(r))
			}
		}
	}
}

// puncts 按长度降序，保证 ..= 先于 .. 先于 . 匹配（§1.2 / KNOWN 决策：不拆分）。
var puncts = []string{
	"..=",
	"->", "=>", "==", "!=", "<=", ">=", "&&", "||", "~=",
	"..",
	"+", "-", "*", "/", "%", "!", "=", "<", ">", ".", ",", ":", ";",
	"(", ")", "{", "}", "[", "]",
}

func (l *Lexer) scanPunct() (Token, bool) {
	for _, p := range puncts {
		if l.hasPrefix(p) {
			t := Token{Kind: Punct, Lexeme: p, Line: l.line, Col: l.col}
			for range p {
				l.advance()
			}
			return t, true
		}
	}
	return Token{}, false
}

func (l *Lexer) illegal(r rune) Token {
	line, col, start := l.line, l.col, l.off
	l.advance()
	l.errs = append(l.errs, Error{line, col, fmt.Sprintf("unexpected character %q", r)})
	return Token{Kind: Illegal, Lexeme: l.src[start:l.off], Line: line, Col: col}
}

func (l *Lexer) scanString(quote rune) Token {
	line, col, start := l.line, l.col, l.off
	l.advance() // 开引号
	for l.off < len(l.src) {
		r := l.peekRune()
		if r == '\\' {
			l.advance()
			if l.off < len(l.src) {
				l.advance()
			}
			continue
		}
		if r == quote {
			l.advance()
			return Token{Kind: StringLit, Lexeme: l.src[start:l.off], Line: line, Col: col}
		}
		if r == '\n' {
			break
		}
		l.advance()
	}
	l.errs = append(l.errs, Error{line, col, "unterminated string literal"})
	return Token{Kind: Illegal, Lexeme: l.src[start:l.off], Line: line, Col: col}
}

// scanNumber：整数/浮点 + 单位后缀。单位串整段取出、精确匹配，不匹配则整段回退（不回退到部分匹配）。
func (l *Lexer) scanNumber() Token {
	line, col, start := l.line, l.col, l.off
	kind := IntLit

	for l.off < len(l.src) && isDigit(l.src[l.off]) {
		l.advance()
	}

	// 小数部分：'.' 后面必须是数字，且不能是 '..' / '..='（0..10 不能被吃成 0.）
	if l.hasPrefix(".") && !l.hasPrefix("..") && l.off+1 < len(l.src) && isDigit(l.src[l.off+1]) {
		kind = FloatLit
		l.advance()
		for l.off < len(l.src) && isDigit(l.src[l.off]) {
			l.advance()
		}
	}

	// 指数部分：1e9 / 1.5e-3
	if l.off < len(l.src) && (l.src[l.off] == 'e' || l.src[l.off] == 'E') {
		p := l.off + 1
		signed := false
		if p < len(l.src) && (l.src[p] == '+' || l.src[p] == '-') {
			signed = true
			p++
		}
		if p < len(l.src) && isDigit(l.src[p]) {
			kind = FloatLit
			l.advance() // e
			if signed {
				l.advance()
			}
			for l.off < len(l.src) && isDigit(l.src[l.off]) {
				l.advance()
			}
		}
	}

	// 单位后缀
	if isUnitLetter(l.peekRune()) {
		unitStart, unitCol := l.off, l.col
		for l.off < len(l.src) && isUnitLetter(l.peekRune()) {
			l.advance()
		}
		run := l.src[unitStart:l.off]
		switch {
		case DurationUnits[run]:
			kind = DurationLit
		case SizeUnits[run]:
			kind = SizeLit
		default:
			// 完整匹配失败：整段单位字母退回，交给下一轮当 Ident。
			// 55mV -> IntLit "55" + Ident "mV"；不会回退成 55m(V)。
			l.off = unitStart
			l.col = unitCol
		}
	}

	return Token{Kind: kind, Lexeme: l.src[start:l.off], Line: line, Col: col}
}

func (l *Lexer) scanIdent() Token {
	line, col, start := l.line, l.col, l.off
	for l.off < len(l.src) && isIdentPart(l.peekRune()) {
		l.advance()
	}
	name := l.src[start:l.off]
	kind := Ident
	if Keywords[name] {
		kind = Keyword
	}

	// QualifiedIdent：Ident '.' Ident（'.' 不是 '..' / '..='，且点后必须是 ident 起始）。
	// '..' 不参与字段访问（§1.1）；parser 需要成员链时按 '.' 拆 Lexeme。
	if l.opts.JoinQualifiedIdents && kind == Ident {
		for l.hasPrefix(".") && !l.hasPrefix("..") {
			if l.off+1 >= len(l.src) {
				break
			}
			r, _ := utf8.DecodeRuneInString(l.src[l.off+1:])
			if !isIdentStart(r) {
				break
			}
			l.advance() // '.'
			for l.off < len(l.src) && isIdentPart(l.peekRune()) {
				l.advance()
			}
			kind = QualifiedIdent
		}
	}

	return Token{Kind: kind, Lexeme: l.src[start:l.off], Line: line, Col: col}
}

// ---------- 虚拟分号 ;ᵥ ----------

// IsTerminator：触发 ;ᵥ 插入的 token 集合。
// 默认集合（用户已确认）：) ] } / Ident·QualifiedIdent（内建类型名除外）/ 各字面量 / return break continue。
func IsTerminator(t Token) bool {
	switch t.Kind {
	case IntLit, FloatLit, StringLit, DurationLit, SizeLit:
		return true
	case Ident, QualifiedIdent:
		return !t.IsBuiltinTypeHead() // 20 个内建类型名排除在终止集合外
	case Keyword:
		switch t.Lexeme {
		case "return", "break", "continue":
			return true
		}
		return false
	case Punct:
		switch t.Lexeme {
		case ")", "]", "}":
			return true
		}
		return false
	}
	return false
}

// suppressPunct：§1.3 例外 1-4 是行首 token 抑制，例外 5 是注释行不产生 token，
// 例外 6（决策新增）由 SuppressInsert 单独处理（keyword else）。
var suppressPunct = map[string]bool{
	// 1. 行首 ( [ { ,
	"(": true, "[": true, "{": true, ",": true,
	// 2. 行首二元运算符
	"+": true, "-": true, "*": true, "/": true, "%": true,
	"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
	"&&": true, "||": true,
	// 3. 行首 .
	".": true,
	// 4. 行首 =（跨行赋值 self.last \n = Some(e)）
	"=": true,
}

// SuppressInsert 报告下一行首 token 是否抑制 ;ᵥ 插入。
func SuppressInsert(next Token) bool {
	// 例外 6（Phase 1 决策 6 采纳）：行首 else 抑制，允许
	//   }
	//   else { ... }
	// 的换行写法（§1.3 原文只列 5 条，本条为决策新增）。
	if next.Kind == Keyword && next.Lexeme == "else" {
		return true
	}
	return next.Kind == Punct && suppressPunct[next.Lexeme]
}

func insertVirtualSemis(raw []Token) []Token {
	out := make([]Token, 0, len(raw)+len(raw)/4)
	for i, t := range raw {
		out = append(out, t)
		if t.Kind == EOF {
			break
		}
		if !IsTerminator(t) {
			continue
		}
		var next Token
		if i+1 < len(raw) {
			next = raw[i+1]
		}
		// 同一行：不插（N1 需要 lexer 保留「同行多语句」给 parser 报 E-SAME-LINE-STMT）
		if next.Kind != EOF && next.Line == t.Line {
			continue
		}
		if next.Kind != EOF && SuppressInsert(next) {
			continue
		}
		out = append(out, Token{
			Kind:   VirtualSemi,
			Lexeme: ";ᵥ",
			Line:   t.Line,
			Col:    t.EndCol(),
		})
	}
	return out
}
