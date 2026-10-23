// Package parser 是 Neura v0.3.7 的递归下降解析器（LL(1) + 少量 lookahead）。
//
// 规范 §2–§11 的 EBNF 不在此输入包内，本实现仅覆盖输入包中可确证的产生式
// （§17 全部示例、v0.3.7 修复决策、Phase 2 必做项），并对推断部分标注「重建」。
// 未实现 §14 明确不做的功能。
package parser

import (
	"fmt"
	"sort"
	"strings"

	"neura/internal/ast"
	"neura/internal/diag"
	"neura/internal/lexer"
)

// bailout 用于声明级错误恢复（panic/recover）。
type bailout struct{}

type parser struct {
	toks    []lexer.Token
	pos     int
	errs    []diag.Error
	forIter int // >0 时处于 for 的迭代 expr 位置（Range 合法）
}

// Parse 词法 + 语法，返回 AST 与按位置排序的诊断。
func Parse(src string) (*ast.File, []diag.Error) {
	toks, lexErrs := lexer.Lex(src)
	f, errs := ParseTokens(toks)
	out := make([]diag.Error, 0, len(lexErrs)+len(errs))
	for _, e := range lexErrs {
		out = append(out, diag.Error{
			Code: diag.CodeLexError,
			Pos:  diag.Pos{Line: e.Line, Col: e.Col},
			Msg:  e.Msg,
		})
	}
	out = append(out, errs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pos.Line != out[j].Pos.Line {
			return out[i].Pos.Line < out[j].Pos.Line
		}
		return out[i].Pos.Col < out[j].Pos.Col
	})
	return f, out
}

func ParseTokens(toks []lexer.Token) (*ast.File, []diag.Error) {
	if len(toks) == 0 || toks[len(toks)-1].Kind != lexer.EOF {
		line, col := 1, 1
		if len(toks) > 0 {
			last := toks[len(toks)-1]
			line, col = last.Line, last.EndCol()
		}
		toks = append(toks, lexer.Token{Kind: lexer.EOF, Line: line, Col: col})
	}
	p := &parser{toks: toks}
	f := p.parseFile()
	return f, p.errs
}

// ---------------- token 工具 ----------------

func (p *parser) cur() lexer.Token { return p.toks[p.pos] }

func (p *parser) peek(k int) lexer.Token {
	i := p.pos + k
	if i >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[i]
}

func (p *parser) advance() lexer.Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) prev() lexer.Token {
	if p.pos == 0 {
		return p.toks[0]
	}
	return p.toks[p.pos-1]
}

func (p *parser) atEOF() bool { return p.cur().Kind == lexer.EOF }

func (p *parser) atPunct(s string) bool {
	t := p.cur()
	return t.Kind == lexer.Punct && t.Lexeme == s
}

func (p *parser) atKw(s string) bool {
	t := p.cur()
	return t.Kind == lexer.Keyword && t.Lexeme == s
}

// isSemi：;ᵥ 与 ; 等价（Phase 2 必做项）。
func (p *parser) isSemi() bool {
	t := p.cur()
	return t.Kind == lexer.VirtualSemi || (t.Kind == lexer.Punct && t.Lexeme == ";")
}

func posOf(t lexer.Token) diag.Pos { return diag.Pos{Line: t.Line, Col: t.Col} }

func describe(t lexer.Token) string {
	switch t.Kind {
	case lexer.EOF:
		return "文件结束"
	case lexer.VirtualSemi:
		return ";ᵥ"
	case lexer.Punct, lexer.Keyword:
		return fmt.Sprintf("%q", t.Lexeme)
	default:
		return fmt.Sprintf("%s %q", t.Kind, t.Lexeme)
	}
}

func (p *parser) errorAt(t lexer.Token, code, msg string) {
	p.errs = append(p.errs, diag.Error{Code: code, Pos: posOf(t), Msg: msg})
}

func (p *parser) fail(t lexer.Token, code, msg string) {
	p.errorAt(t, code, msg)
	panic(bailout{})
}

func (p *parser) expectPunct(s string) lexer.Token {
	t := p.cur()
	if t.Kind == lexer.Punct && t.Lexeme == s {
		return p.advance()
	}
	p.fail(t, diag.CodeUnexpectedToken, fmt.Sprintf("期望 %q，实际是 %s", s, describe(t)))
	return t
}

func (p *parser) expectIdent(what string) string {
	t := p.cur()
	if t.Kind == lexer.Ident {
		p.advance()
		return t.Lexeme
	}
	p.fail(t, diag.CodeUnexpectedToken, "期望"+what+"（标识符），实际是 "+describe(t))
	return ""
}

// ---------------- 分隔符 / 软关键字 ----------------
//
// Neura 的 ;ᵥ 只在「终止集合 + 换行」时插入，而字段列表允许用换行、; 或 , 分隔。
// 所以字段/绑定/端口列表统一走 skipSemis / skipSeparators + endField，
// 不再假设「一个字段后一定紧跟一个 ;ᵥ」。

// skipSemis 跳过任意个 ; / ;ᵥ（参数表、泛型实参、调用实参里只可能有分号）。
func (p *parser) skipSemis() {
	for p.isSemi() {
		p.advance()
	}
}

// skipSeparators 跳过任意个 ; / ;ᵥ / ,（字段、绑定、端口列表）。
func (p *parser) skipSeparators() {
	for p.isSemi() || p.atPunct(",") {
		p.advance()
	}
}

// softKeywords 是可以出现在「名字位置」的上下文关键字（on signal … from …），
// 这样 struct 里叫 signal / from 的字段依然可用。
var softKeywords = map[string]bool{"on": true, "signal": true, "from": true}

func (p *parser) atIdentName(name string) bool {
	t := p.cur()
	return (t.Kind == lexer.Ident || t.Kind == lexer.Keyword) && t.Lexeme == name
}

// expectIdentSoft 用于端口名 / signal 名这类位置：允许软关键字当名字。
func (p *parser) expectIdentSoft(what string) string {
	t := p.cur()
	if t.Kind == lexer.Ident || (t.Kind == lexer.Keyword && softKeywords[t.Lexeme]) {
		p.advance()
		return t.Lexeme
	}
	p.fail(t, diag.CodeUnexpectedToken, "期望"+what+"（标识符），实际是 "+describe(t))
	return ""
}

// endField 处理 struct / logic structure 的字段分隔：; / ;ᵥ / , / 换行 / } 都合法。
// 其余情况给出精确诊断（而不是落到下一轮 expectIdent 的「期望字段名」）。
func (p *parser) endField(kind string) {
	if p.isSemi() || p.atPunct(",") {
		p.advance()
		return
	}
	if p.atPunct("}") || p.atEOF() {
		return
	}
	if p.atPunct("=") {
		bad := p.cur()
		p.advance()
		_ = p.parseExpr() // 吞掉默认值，避免级联
		p.errorAt(bad, diag.CodeUnexpectedToken,
			kind+"只声明 name: Type，不能带默认值（是否漏了 ; ？）")
		return
	}
	if p.cur().Line == p.prev().Line {
		p.errorAt(p.cur(), diag.CodeSameLineStmt, "同行"+kind+"必须用 ; 或 , 分隔")
	}
}

// endBinding 同 endField，但 instance structure 的值是表达式（没有默认值语法）。
func (p *parser) endBinding() {
	if p.isSemi() || p.atPunct(",") {
		p.advance()
		return
	}
	if p.atPunct("}") || p.atEOF() {
		return
	}
	if p.cur().Line == p.prev().Line {
		p.errorAt(p.cur(), diag.CodeSameLineStmt, "同行 binding 必须用 ; 或 , 分隔")
	}
}

// ---------------- 顶层 ----------------

var declStart = map[string]bool{
	"fn": true, "struct": true, "logic": true, "instance": true,
	"neuron": true, "synapse": true, "place": true,
}

func (p *parser) parseFile() *ast.File {
	f := &ast.File{}
	for !p.atEOF() {
		if p.isSemi() {
			p.advance()
			continue
		}
		d := p.parseDecl()
		if d != nil {
			f.Decls = append(f.Decls, d)
		}
		if p.isSemi() {
			p.advance()
			continue
		}
		if p.atEOF() {
			break
		}
		// 顶层声明是自定界的：下一个声明关键字可以直接开始新声明，
		// 不强求 ;ᵥ（例如 synapse 以 failover 关键字结尾时 lexer 不会插 ;ᵥ）。
		if p.cur().Kind == lexer.Keyword && declStart[p.cur().Lexeme] {
			continue
		}
		p.errorAt(p.cur(), diag.CodeUnexpectedToken, "顶层声明之间缺少分隔，实际是 "+describe(p.cur()))
		p.advance()
	}
	return f
}

func (p *parser) parseDecl() (d ast.Decl) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
			p.syncDecl()
			d = nil
		}
	}()
	t := p.cur()
	if t.Kind != lexer.Keyword || !declStart[t.Lexeme] {
		p.fail(t, diag.CodeUnexpectedToken,
			"期望顶层声明（fn/struct/logic/instance/neuron/synapse/place），实际是 "+describe(t))
	}
	switch t.Lexeme {
	case "fn":
		return p.parseFnDecl()
	case "struct":
		return p.parseStructDecl()
	case "logic":
		return p.parseLogicDecl()
	case "instance":
		return p.parseInstanceDecl()
	case "neuron":
		return p.parseNeuronDecl()
	case "synapse":
		return p.parseSynapseDecl()
	case "place":
		return p.parsePlaceDecl()
	}
	p.fail(t, diag.CodeUnexpectedToken, "未知顶层声明 "+t.Lexeme)
	return nil
}

// syncDecl 在声明级错误后跳到下一个顶层声明或语句边界。
func (p *parser) syncDecl() {
	depth := 0
	for !p.atEOF() {
		t := p.cur()
		if depth == 0 && t.Kind == lexer.Keyword && declStart[t.Lexeme] {
			return
		}
		if t.Kind == lexer.Punct {
			switch t.Lexeme {
			case "{":
				depth++
			case "}":
				p.advance()
				if depth > 0 {
					depth--
				}
				if depth == 0 {
					return
				}
				continue
			}
		}
		if depth == 0 && (t.Kind == lexer.VirtualSemi || (t.Kind == lexer.Punct && t.Lexeme == ";")) {
			p.advance()
			return
		}
		p.advance()
	}
}

// ---------------- 声明 ----------------

func (p *parser) parseFnDecl() *ast.FnDecl {
	fnTok := p.advance()
	name := p.expectIdent("函数名")
	params := p.parseParams()
	p.expectPunct("->")
	res := p.parseType()
	body := p.parseBlock()
	return &ast.FnDecl{At: posOf(fnTok), Name: name, Params: params, Result: res, Body: body}
}

func (p *parser) parseParams() []*ast.Param {
	p.expectPunct("(")
	params := []*ast.Param{}
	for !p.atPunct(")") && !p.atEOF() {
		p.skipSemis()
		if p.atPunct(")") {
			break
		}
		nameTok := p.cur()
		name := p.expectIdent("参数名")
		p.expectPunct(":")
		typ := p.parseType()
		params = append(params, &ast.Param{At: posOf(nameTok), Name: name, Type: typ})
		if p.atPunct(",") {
			p.advance()
			continue
		}
		break
	}
	p.expectPunct(")")
	return params
}

// parseType：type 位置四分派（( / 内建或限定名 / fn；内建与限定名共用 NamedType 分支）。
func (p *parser) parseType() ast.Type {
	t := p.cur()
	switch {
	case t.Kind == lexer.Punct && t.Lexeme == "(":
		return p.parseParenType()
	case t.Kind == lexer.Keyword && t.Lexeme == "fn":
		p.advance()
		p.expectPunct("(")
		params := []ast.Type{}
		for !p.atPunct(")") && !p.atEOF() {
			params = append(params, p.parseType())
			if p.atPunct(",") {
				p.advance()
				continue
			}
			break
		}
		p.expectPunct(")")
		p.expectPunct("->")
		res := p.parseType()
		return &ast.FnType{At: posOf(t), Params: params, Result: res}
	case t.Kind == lexer.Ident || t.Kind == lexer.QualifiedIdent:
		// KNOWN-03：Range 已从 prim_type 删除，显式声明在 parser 阶段就报错。
		if t.Lexeme == "Range" {
			p.fail(t, diag.CodeRangeOutsideFor, "Range 是 for 专用类型，不能显式声明（只用 a..b / a..=b）")
		}
		p.advance()
		var args []ast.Type
		if p.atPunct("<") {
			p.advance()
			for !p.atPunct(">") && !p.atEOF() {
				args = append(args, p.parseType())
				if p.atPunct(",") {
					p.advance()
					continue
				}
				break
			}
			p.expectPunct(">")
		}
		return &ast.NamedType{At: posOf(t), Name: t.Lexeme, Args: args}
	}
	p.fail(t, diag.CodeExpectedType, "期望一个类型，实际是 "+describe(t))
	return nil
}

// parseParenType：接受 ()、(T,)、(T, U)，拒绝 (T)（N7）。
func (p *parser) parseParenType() ast.Type {
	open := p.advance() // (
	var elems []ast.Type
	trailingComma := false
	for !p.atPunct(")") && !p.atEOF() {
		elems = append(elems, p.parseType())
		if p.atPunct(",") {
			p.advance()
			trailingComma = true
			continue
		}
		trailingComma = false
		break
	}
	p.expectPunct(")")
	if len(elems) == 1 && !trailingComma {
		p.fail(open, diag.CodeParenSingleType, "单元素括号在类型位置非法；元组请写 (T,) 或 (T, U)")
	}
	return &ast.TupleType{At: posOf(open), Elems: elems}
}

func (p *parser) parseStructDecl() *ast.StructDecl {
	t := p.advance()
	name := p.expectIdent("struct 名")
	p.expectPunct("{")
	d := &ast.StructDecl{At: posOf(t), Name: name}
	for !p.atPunct("}") && !p.atEOF() {
		p.skipSeparators()
		if p.atPunct("}") || p.atEOF() {
			break
		}
		fTok := p.cur()
		fname := p.expectIdentSoft("字段名")
		p.expectPunct(":")
		// struct 用 field：值是类型
		ft := p.parseType()
		d.Fields = append(d.Fields, &ast.Field{At: posOf(fTok), Name: fname, Type: ft})
		p.endField("字段")
	}
	p.expectPunct("}")
	return d
}

func (p *parser) parseLogicDecl() *ast.LogicDecl {
	t := p.advance()
	name := p.expectIdent("logic 名")
	d := &ast.LogicDecl{At: posOf(t), Name: name}

	if p.atKw("with") {
		p.advance()
		for {
			m := p.cur()
			if m.Kind != lexer.Ident && m.Kind != lexer.Keyword {
				p.fail(m, diag.CodeUnexpectedToken, "with 后期望 modifier，实际是 "+describe(m))
			}
			d.Modifiers = append(d.Modifiers, m.Lexeme)
			p.advance()
			if p.atPunct(",") {
				p.advance()
				continue
			}
			break
		}
	}

	// N2：hazardous 只能作为 with 的 modifier，裸写在 logic 头里在 parser 阶段即报错。
	// （N3 的 with hazardous 在此合法，E-HAZARD-NO-GUARD 由 Phase 3 checker 判。）
	if p.atKw("hazardous") {
		p.errorAt(p.cur(), diag.CodeHazardNoWith, "hazardous 必须作为 with 的 modifier")
		p.advance()
	}
	if p.atKw("strict") {
		p.advance()
		d.Strict = true
	}
	if p.atKw("guard") {
		p.advance()
		d.Guard = p.parseExpr()
	}

	p.expectPunct("{")
	for !p.atPunct("}") && !p.atEOF() {
		if p.isSemi() {
			p.advance()
			continue
		}
		switch {
		case p.atKw("structure"):
			p.advance()
			p.expectPunct(":")
			p.expectPunct("{")
			for !p.atPunct("}") && !p.atEOF() {
				p.skipSeparators()
				if p.atPunct("}") || p.atEOF() {
					break
				}
				fTok := p.cur()
				fname := p.expectIdentSoft("structure 字段名")
				p.expectPunct(":")
				ft := p.parseType()
				d.Structure = append(d.Structure, &ast.Field{At: posOf(fTok), Name: fname, Type: ft})
				p.endField("字段")
			}
			p.expectPunct("}")
		case p.atKw("capability"):
			p.advance()
			p.expectPunct(":")
			p.expectPunct("{")
			for !p.atPunct("}") && !p.atEOF() {
				p.skipSemis()
				if p.atPunct("}") || p.atEOF() {
					break
				}
				d.Capability = append(d.Capability, p.parseFnSig())
				p.skipSemis()
			}
			p.expectPunct("}")
		default:
			p.fail(p.cur(), diag.CodeUnexpectedToken,
				"logic 体内只允许 structure / capability，实际是 "+describe(p.cur()))
		}
	}
	p.expectPunct("}")
	return d
}

func (p *parser) parseFnSig() *ast.FnSig {
	t := p.cur()
	name := p.expectIdent("capability 方法名")
	params := p.parseParams()
	var res ast.Type
	if p.atPunct("->") {
		p.advance()
		res = p.parseType()
	}
	var body *ast.Block
	if p.atPunct("{") {
		body = p.parseBlock()
	}
	return &ast.FnSig{At: posOf(t), Name: name, Params: params, Result: res, Body: body}
}

func (p *parser) parseInstanceDecl() *ast.InstanceDecl {
	t := p.advance()
	name := p.expectIdent("instance 名")
	p.expectPunct(":")
	ref := p.parseQualifiedRef()
	d := &ast.InstanceDecl{At: posOf(t), Name: name, Logic: strings.Join(ref.Parts, ".")}

	p.expectPunct("{")
	for !p.atPunct("}") && !p.atEOF() {
		p.skipSemis()
		if p.atPunct("}") || p.atEOF() {
			break
		}
		if !p.atKw("structure") {
			p.fail(p.cur(), diag.CodeUnexpectedToken,
				"instance 体内只允许 structure，实际是 "+describe(p.cur()))
		}
		p.advance()
		p.expectPunct(":")
		p.expectPunct("{")
		for !p.atPunct("}") && !p.atEOF() {
			p.skipSeparators()
			if p.atPunct("}") || p.atEOF() {
				break
			}
			kTok := p.cur()
			bname := p.expectIdentSoft("binding 名")
			p.expectPunct(":")
			// instance structure 用 binding：值是表达式（不是类型）
			val := p.parseExpr()
			d.Bindings = append(d.Bindings, &ast.Binding{At: posOf(kTok), Name: bname, Value: val})
			p.endBinding()
		}
		p.expectPunct("}")
	}
	p.expectPunct("}")
	return d
}

func (p *parser) parseNeuronDecl() *ast.NeuronDecl {
	t := p.advance()
	name := p.expectIdent("neuron 名")
	p.expectPunct("{")
	d := &ast.NeuronDecl{At: posOf(t), Name: name}
	for !p.atPunct("}") && !p.atEOF() {
		p.skipSeparators()
		if p.atPunct("}") || p.atEOF() {
			break
		}
		switch {
		case p.atKw("input"), p.atKw("output"):
			dir := p.advance()
			pTok := p.cur()
			pname := p.expectIdentSoft("端口名")
			p.expectPunct(":")
			pt := p.parseType()
			d.Ports = append(d.Ports, &ast.Port{At: posOf(pTok), Dir: dir.Lexeme, Name: pname, Type: pt})
		case p.atIdentName("on"):
			// EBNF：on signal <name>: <Type> from <source> { ... }
			d.Signals = append(d.Signals, p.parseSignalHandler())
		case p.atKw("handle"):
			// legacy 别名（早期实现用），语义等价于一个匿名 on signal
			p.advance()
			d.Handler = p.parseBlock()
		default:
			p.fail(p.cur(), diag.CodeUnexpectedToken,
				"neuron 体内只允许 input / output / on signal … from … / handle（legacy），实际是 "+describe(p.cur()))
		}
		p.skipSemis()
	}
	p.expectPunct("}")
	return d
}

// parseSignalHandler：on signal <name>: <Type> from <source> <Block>
func (p *parser) parseSignalHandler() *ast.SignalHandler {
	onTok := p.advance() // on
	if !p.atIdentName("signal") {
		p.errorAt(p.cur(), diag.CodeUnexpectedToken,
			"on 后期望 signal，实际是 "+describe(p.cur()))
		p.skipBalancedBraces()
		return &ast.SignalHandler{At: posOf(onTok)}
	}
	p.advance()
	h := &ast.SignalHandler{At: posOf(onTok)}
	h.Name = p.expectIdentSoft("signal 名")
	p.expectPunct(":")
	h.Type = p.parseType()
	if !p.atIdentName("from") {
		// 就地恢复：报一条精确错误，绝不把 { … } 体丢给上层造成级联
		p.errorAt(p.cur(), diag.CodeUnexpectedToken,
			"on signal 后期望 from，实际是 "+describe(p.cur()))
		p.skipBalancedBraces()
		return h
	}
	p.advance()
	ref := p.parseQualifiedRef()
	h.From = strings.Join(ref.Parts, ".")
	h.Body = p.parseBlock()
	return h
}

// skipBalancedBraces 跳过一个完整的 { … }（不消费外层 }）。
func (p *parser) skipBalancedBraces() {
	if !p.atPunct("{") {
		return
	}
	depth := 0
	for !p.atEOF() {
		switch {
		case p.atPunct("{"):
			depth++
			p.advance()
		case p.atPunct("}"):
			depth--
			p.advance()
			if depth == 0 {
				return
			}
		default:
			p.advance()
		}
	}
}

func (p *parser) parseSynapseDecl() *ast.SynapseDecl {
	t := p.advance()
	src := p.parseQualifiedRef()
	if !p.atPunct("->") {
		p.fail(p.cur(), diag.CodeUnexpectedToken, "synapse 期望 ->，实际是 "+describe(p.cur()))
	}
	p.advance()
	dst := p.parseQualifiedRef()
	d := &ast.SynapseDecl{At: posOf(t), Src: src, Dst: dst}
	for {
		switch {
		case p.atKw("on_failure"):
			p.advance()
			d.OnFailure = p.parseFailurePolicy()
		case p.atKw("failover"):
			p.advance()
			d.FailoverP = p.parseFailoverPolicy()
		default:
			return d
		}
	}
}

// parseFailurePolicy：failure_policy = retry(n)。failover 出现在这里 → E-WRONG-POLICY（N10）。
func (p *parser) parseFailurePolicy() *ast.Policy {
	t := p.cur()
	if t.Kind == lexer.Keyword && t.Lexeme == "failover" {
		p.errorAt(t, diag.CodeWrongPolicy, "failover 属于 failover_policy，不能用在 on_failure 位置")
		p.advance()
		p.skipPolicyArgs()
		return &ast.Policy{At: posOf(t), Kind: "failover(invalid)"}
	}
	if t.Kind == lexer.Keyword && t.Lexeme == "retry" {
		p.advance()
		return &ast.Policy{At: posOf(t), Kind: "retry", Args: p.parsePolicyArgs()}
	}
	p.fail(t, diag.CodeUnexpectedToken,
		"on_failure 后期望 failure_policy（retry(n)），实际是 "+describe(t))
	return nil
}

// parseFailoverPolicy：failover_policy = failover(n?)。retry 出现在这里 → E-WRONG-POLICY（N9）。
func (p *parser) parseFailoverPolicy() *ast.Policy {
	t := p.cur()
	if t.Kind == lexer.Keyword && t.Lexeme == "retry" {
		p.errorAt(t, diag.CodeWrongPolicy, "retry 属于 failure_policy，不能用在 failover 位置")
		p.advance()
		p.skipPolicyArgs()
		return &ast.Policy{At: posOf(t), Kind: "retry(invalid)"}
	}
	if t.Kind == lexer.Keyword && t.Lexeme == "failover" {
		p.advance()
		return &ast.Policy{At: posOf(t), Kind: "failover", Args: p.parsePolicyArgs()}
	}
	p.fail(t, diag.CodeUnexpectedToken,
		"failover 后期望 failover_policy，实际是 "+describe(t))
	return nil
}

func (p *parser) parsePolicyArgs() []ast.Expr {
	args := []ast.Expr{}
	if !p.atPunct("(") {
		return args
	}
	p.advance()
	for !p.atPunct(")") && !p.atEOF() {
		args = append(args, p.parseExpr())
		if p.atPunct(",") {
			p.advance()
			continue
		}
		break
	}
	p.expectPunct(")")
	return args
}

// skipPolicyArgs 在 policy 报错后吞掉 ( … )，避免级联错误。
func (p *parser) skipPolicyArgs() {
	if !p.atPunct("(") {
		return
	}
	depth := 0
	for !p.atEOF() {
		switch {
		case p.atPunct("("):
			depth++
			p.advance()
		case p.atPunct(")"):
			depth--
			p.advance()
			if depth == 0 {
				return
			}
		default:
			p.advance()
		}
	}
}

func (p *parser) parsePlaceDecl() *ast.PlaceDecl {
	t := p.advance()
	name := p.expectIdent("place 目标名")
	if !p.atKw("at") {
		p.fail(p.cur(), diag.CodeUnexpectedToken, "place 期望 at，实际是 "+describe(p.cur()))
	}
	p.advance()
	d := &ast.PlaceDecl{At: posOf(t), Name: name, AtSpec: p.parseAtSpec()}
	for {
		switch {
		case p.atKw("balance"):
			p.advance()
			bTok := p.cur()
			kind := p.expectIdent("balance 策略名")
			d.Balance = &ast.Policy{At: posOf(bTok), Kind: kind, Args: p.parsePolicyArgs()}
		case p.atKw("where"):
			p.advance()
			d.Where = p.parseExpr()
		case p.atKw("failover"):
			p.advance()
			d.FailoverP = p.parseFailoverPolicy()
		case p.atKw("on_failure"):
			p.advance()
			d.OnFailure = p.parseFailurePolicy()
		default:
			return d
		}
	}
}

func (p *parser) parseAtSpec() *ast.AtSpec {
	t := p.cur()
	switch {
	case t.Kind == lexer.Keyword && t.Lexeme == "local":
		p.advance()
		return &ast.AtSpec{At: posOf(t), Local: true}
	case t.Kind == lexer.StringLit:
		p.advance()
		return &ast.AtSpec{At: posOf(t), Nodes: []string{unquoteLiteral(t.Lexeme)}}
	case t.Kind == lexer.Punct && t.Lexeme == "[":
		p.advance()
		spec := &ast.AtSpec{At: posOf(t)}
		for !p.atPunct("]") && !p.atEOF() {
			n := p.cur()
			if n.Kind != lexer.StringLit {
				p.fail(n, diag.CodeUnexpectedToken, "at 列表只接受字符串节点名，实际是 "+describe(n))
			}
			p.advance()
			spec.Nodes = append(spec.Nodes, unquoteLiteral(n.Lexeme))
			if p.atPunct(",") {
				p.advance()
				continue
			}
			break
		}
		p.expectPunct("]")
		return spec
	}
	p.fail(t, diag.CodeUnexpectedToken,
		"at 后期望 local / \"node\" / [\"a\",\"b\"]，实际是 "+describe(t))
	return nil
}

// ---------------- 语句 ----------------

func (p *parser) parseBlock() *ast.Block {
	open := p.cur()
	p.expectPunct("{")
	b := &ast.Block{At: posOf(open)}
	for !p.atPunct("}") && !p.atEOF() {
		if p.isSemi() {
			p.advance()
			continue
		}
		s := p.parseStmtSafe()
		if s != nil {
			b.Stmts = append(b.Stmts, s)
		}
		p.endStmt()
	}
	p.expectPunct("}")
	return b
}

// parseStmtSafe 把语句级语法错误恢复限制在块内，避免把整个声明丢掉。
func (p *parser) parseStmtSafe() (s ast.Stmt) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
			p.syncStmt()
			s = nil
		}
	}()
	return p.parseStmt()
}

// syncStmt 跳到本条语句的 ; / ;ᵥ，或让 parseBlock 遇到 } 收尾。
func (p *parser) syncStmt() {
	depth := 0
	for !p.atEOF() {
		t := p.cur()
		if t.Kind == lexer.Punct {
			switch t.Lexeme {
			case "{":
				depth++
			case "}":
				if depth == 0 {
					return
				}
				depth--
				p.advance()
				continue
			}
		}
		if depth == 0 && (t.Kind == lexer.VirtualSemi || (t.Kind == lexer.Punct && t.Lexeme == ";")) {
			p.advance()
			return
		}
		p.advance()
	}
}

// endStmt 检查语句分隔：; / ;ᵥ / } / EOF 合法；同行紧邻下一条语句 → E-SAME-LINE-STMT（N1）。
func (p *parser) endStmt() {
	if p.isSemi() {
		p.advance()
		return
	}
	if p.atPunct("}") || p.atEOF() {
		return
	}
	if p.cur().Line == p.prev().Line {
		p.errorAt(p.cur(), diag.CodeSameLineStmt, "同行多条语句必须用 ; 分隔")
		return
	}
	p.errorAt(p.cur(), diag.CodeMissingSep, "语句之间缺少 ; 分隔")
}

func (p *parser) parseStmt() ast.Stmt {
	t := p.cur()
	if t.Kind == lexer.Keyword {
		switch t.Lexeme {
		case "let":
			return p.parseLet()
		case "do":
			p.advance()
			return &ast.DoStmt{At: posOf(t), Body: p.parseBlock()}
		case "for":
			return p.parseFor()
		case "if":
			return p.parseIf()
		case "match":
			return p.parseMatch()
		case "return":
			p.advance()
			if p.isSemi() || p.atPunct("}") || p.atEOF() {
				return &ast.ReturnStmt{At: posOf(t)}
			}
			return &ast.ReturnStmt{At: posOf(t), Value: p.parseExpr()}
		case "break":
			p.advance()
			return &ast.BreakStmt{At: posOf(t)}
		case "continue":
			p.advance()
			return &ast.ContinueStmt{At: posOf(t)}
		case "emit":
			p.advance()
			return &ast.EmitStmt{At: posOf(t), X: p.parseExpr()}
		case "invoke":
			p.advance()
			return &ast.InvokeStmt{At: posOf(t), X: p.parseExpr()}
		case "reuse":
			p.advance()
			return &ast.ReuseStmt{At: posOf(t), X: p.parseExpr()}
		case "learn":
			p.advance()
			p.expectPunct("(")
			x := p.parseExpr()
			p.expectPunct(")")
			return &ast.LearnStmt{At: posOf(t), X: x}
		}
	}

	x := p.parseExpr()
	if p.atPunct("=") {
		eq := p.advance()
		val := p.parseExpr()
		if !assignable(x) {
			p.errorAt(eq, diag.CodeAssignLHS, assignLHSMsg(x))
		}
		return &ast.AssignStmt{At: exprPos(x), Target: x, Value: val}
	}
	return &ast.ExprStmt{At: exprPos(x), X: x}
}

func (p *parser) parseLet() ast.Stmt {
	letTok := p.advance()
	mutable := false
	if p.atKw("mut") {
		p.advance()
		mutable = true
	}
	name := p.expectIdent("变量名")
	var typ ast.Type
	if p.atPunct(":") {
		p.advance()
		typ = p.parseType()
	}
	p.expectPunct("=")
	val := p.parseExpr()

	// let x = 1 = 2 → E-ASSIGN-LHS（KNOWN-06 的 message 逐字一致）
	if p.atPunct("=") {
		eq := p.advance()
		p.errorAt(eq, diag.CodeAssignLHS, assignLHSMsg(val))
		_ = p.parseExpr()
	}
	return &ast.LetStmt{At: posOf(letTok), Name: name, Mutable: mutable, Type: typ, Value: val}
}

func (p *parser) parseFor() ast.Stmt {
	forTok := p.advance()
	name := p.expectIdent("迭代变量名")
	if !p.atKw("in") {
		p.fail(p.cur(), diag.CodeUnexpectedToken, "for 期望 in，实际是 "+describe(p.cur()))
	}
	p.advance()
	p.forIter++
	iter := p.parseExpr()
	p.forIter--
	body := p.parseBlock()
	return &ast.ForStmt{At: posOf(forTok), Var: name, Iter: iter, Body: body}
}

func (p *parser) parseIf() ast.Stmt {
	ifTok := p.advance()
	cond := p.parseExpr()
	then := p.parseBlock()
	n := &ast.IfStmt{At: posOf(ifTok), Cond: cond, Then: then}
	if p.atKw("else") {
		p.advance()
		if p.atKw("if") {
			n.Else = p.parseIf()
		} else {
			n.Else = p.parseBlock()
		}
	}
	return n
}

func (p *parser) parseMatch() ast.Stmt {
	mTok := p.advance()
	subject := p.parseExpr()
	if !p.atKw("against") {
		p.fail(p.cur(), diag.CodeUnexpectedToken, "match 期望 against，实际是 "+describe(p.cur()))
	}
	p.advance()
	against := p.parseExpr()
	p.expectPunct("{")
	n := &ast.MatchStmt{At: posOf(mTok), Subject: subject, Against: against}
	for p.atKw("case") {
		caseTok := p.advance()
		pat := p.parseQualifiedRef()
		bind := ""
		if p.atPunct("(") {
			p.advance()
			bind = p.expectIdent("绑定名")
			p.expectPunct(")")
		}
		if !p.atPunct("=>") {
			p.fail(p.cur(), diag.CodeUnexpectedToken, "case 期望 =>，实际是 "+describe(p.cur()))
		}
		p.advance()
		n.Arms = append(n.Arms, &ast.Arm{
			At:      posOf(caseTok),
			Pattern: pat,
			Bind:    bind,
			Body:    p.parseLogicArmRHS(),
		})
		if p.isSemi() {
			p.advance()
		}
	}
	p.expectPunct("}")
	return n
}

// parseLogicArmRHS：logic_arm_rhs 五分派（do-block / reuse / invoke / emit / expr）。
// `{ ... }` 与 `do { ... }` 都归为 DoStmt。
func (p *parser) parseLogicArmRHS() ast.Stmt {
	t := p.cur()
	if t.Kind == lexer.Keyword {
		switch t.Lexeme {
		case "do":
			p.advance()
			return &ast.DoStmt{At: posOf(t), Body: p.parseBlock()}
		case "reuse":
			p.advance()
			return &ast.ReuseStmt{At: posOf(t), X: p.parseExpr()}
		case "invoke":
			p.advance()
			return &ast.InvokeStmt{At: posOf(t), X: p.parseExpr()}
		case "emit":
			p.advance()
			return &ast.EmitStmt{At: posOf(t), X: p.parseExpr()}
		}
	}
	if t.Kind == lexer.Punct && t.Lexeme == "{" {
		return &ast.DoStmt{At: posOf(t), Body: p.parseBlock()}
	}
	x := p.parseExpr()
	return &ast.ExprStmt{At: exprPos(x), X: x}
}

// ---------------- 表达式 ----------------

var binPrec = map[string]int{
	"||": 1, "&&": 2,
	"==": 3, "!=": 3, "~=": 3,
	"<": 4, "<=": 4, ">": 4, ">=": 4,
	"+": 5, "-": 5,
	"*": 6, "/": 6, "%": 6,
}

func (p *parser) parseExpr() ast.Expr {
	left := p.parseBinary(1)
	if p.atPunct("..") || p.atPunct("..=") {
		op := p.advance()
		// 规则 36 / KNOWN-03：区间只在 for 的 expr 位置合法
		if p.forIter == 0 {
			p.errorAt(op, diag.CodeRangeOutsideFor, "a..b 仅在 for 的 expr 位置合法")
		}
		right := p.parseBinary(1)
		return &ast.RangeExpr{
			At:        exprPos(left),
			OpAt:      posOf(op),
			Lo:        left,
			Hi:        right,
			Inclusive: op.Lexeme == "..=",
		}
	}
	return left
}

func (p *parser) parseBinary(min int) ast.Expr {
	left := p.parseUnary()
	for {
		t := p.cur()
		if t.Kind != lexer.Punct {
			break
		}
		pr, ok := binPrec[t.Lexeme]
		if !ok || pr < min {
			break
		}
		p.advance()
		right := p.parseBinary(pr + 1)
		left = &ast.BinaryExpr{At: exprPos(left), Op: t.Lexeme, L: left, R: right}
	}
	return left
}

func (p *parser) parseUnary() ast.Expr {
	t := p.cur()
	if t.Kind == lexer.Punct && (t.Lexeme == "-" || t.Lexeme == "!") {
		p.advance()
		return &ast.UnaryExpr{At: posOf(t), Op: t.Lexeme, X: p.parseUnary()}
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() ast.Expr {
	x := p.parsePrimary()
	for {
		switch {
		case p.atPunct("("):
			p.advance()
			args := []ast.Expr{}
			for !p.atPunct(")") && !p.atEOF() {
				args = append(args, p.parseExpr())
				if p.atPunct(",") {
					p.advance()
					continue
				}
				break
			}
			p.expectPunct(")")
			x = &ast.CallExpr{At: exprPos(x), Fn: x, Args: args}
		case p.atPunct("."):
			p.advance()
			nameTok := p.cur()
			if nameTok.Kind != lexer.Ident {
				p.fail(nameTok, diag.CodeUnexpectedToken,
					"字段访问期望标识符，实际是 "+describe(nameTok))
			}
			p.advance()
			x = &ast.FieldExpr{At: exprPos(x), X: x, Name: nameTok.Lexeme}
		default:
			return x
		}
	}
}

func (p *parser) parsePrimary() ast.Expr {
	t := p.cur()
	switch t.Kind {
	case lexer.IntLit, lexer.FloatLit, lexer.StringLit, lexer.DurationLit, lexer.SizeLit:
		p.advance()
		lit := &ast.Literal{At: posOf(t), Kind: litKind(t.Kind), Text: t.Lexeme}
		// unit_lit：(int|float) ident 紧邻 → UnitExpr（供规则 20 的 span 判定）
		if (t.Kind == lexer.IntLit || t.Kind == lexer.FloatLit) &&
			p.cur().Kind == lexer.Ident &&
			p.cur().Line == t.Line && p.cur().Col == t.EndCol() {
			u := p.advance()
			return &ast.UnitExpr{At: posOf(t), Value: lit, Unit: u.Lexeme}
		}
		return lit
	case lexer.Ident:
		p.advance()
		return &ast.Ident{At: posOf(t), Name: t.Lexeme}
	case lexer.QualifiedIdent:
		p.advance()
		return &ast.QualifiedRef{At: posOf(t), Parts: strings.Split(t.Lexeme, ".")}
	case lexer.Keyword:
		switch t.Lexeme {
		case "true", "false":
			p.advance()
			return &ast.Literal{At: posOf(t), Kind: "bool", Text: t.Lexeme}
		case "learn":
			p.advance()
			p.expectPunct("(")
			arg := p.parseExpr()
			p.expectPunct(")")
			return &ast.CallExpr{
				At:   posOf(t),
				Fn:   &ast.Ident{At: posOf(t), Name: "learn"},
				Args: []ast.Expr{arg},
			}
		}
	case lexer.Punct:
		if t.Lexeme == "(" {
			p.advance()
			x := p.parseExpr()
			p.expectPunct(")")
			return &ast.ParenExpr{At: posOf(t), X: x}
		}
	}
	p.fail(t, diag.CodeUnexpectedToken, "期望表达式，实际是 "+describe(t))
	return nil
}

func litKind(k lexer.Kind) string {
	switch k {
	case lexer.IntLit:
		return "int"
	case lexer.FloatLit:
		return "float"
	case lexer.StringLit:
		return "string"
	case lexer.DurationLit:
		return "duration"
	case lexer.SizeLit:
		return "size"
	}
	return "?"
}

func exprPos(e ast.Expr) diag.Pos {
	switch n := e.(type) {
	case *ast.Ident:
		return n.At
	case *ast.QualifiedRef:
		return n.At
	case *ast.Literal:
		return n.At
	case *ast.UnitExpr:
		return n.At
	case *ast.RangeExpr:
		return n.At
	case *ast.UnaryExpr:
		return n.At
	case *ast.BinaryExpr:
		return n.At
	case *ast.CallExpr:
		return n.At
	case *ast.FieldExpr:
		return n.At
	case *ast.ParenExpr:
		return n.At
	}
	return diag.Pos{}
}

func assignable(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.QualifiedRef, *ast.FieldExpr:
		return true
	}
	return false
}

// assignLHSMsg 与 KNOWN-06 的 message 逐字一致。
func assignLHSMsg(e ast.Expr) string {
	if lit, ok := e.(*ast.Literal); ok {
		return fmt.Sprintf("= 左侧必须是变量，不能是字面量 %s（是否漏了 ; ？）", lit.Text)
	}
	return "= 左侧必须是变量（是否漏了 ; ？）"
}

// unquoteLiteral 去掉字符串字面量的引号并处理常见转义。
// AST 里存语义字符串（节点地址要能直接拨号），不是原始 token 文本。
func unquoteLiteral(s string) string {
	if len(s) < 2 || (s[0] != '"' && s[0] != '\'') {
		return s
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			i++
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(body[i])
			}
			continue
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

func (p *parser) parseQualifiedRef() *ast.QualifiedRef {
	t := p.cur()
	switch t.Kind {
	case lexer.QualifiedIdent:
		p.advance()
		return &ast.QualifiedRef{At: posOf(t), Parts: strings.Split(t.Lexeme, ".")}
	case lexer.Ident:
		p.advance()
		return &ast.QualifiedRef{At: posOf(t), Parts: []string{t.Lexeme}}
	}
	p.fail(t, diag.CodeUnexpectedToken, "期望标识符，实际是 "+describe(t))
	return nil
}
