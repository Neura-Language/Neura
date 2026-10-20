package lexer

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind 是 token 类别。Phase 1 要求的类别全集。
type Kind int

const (
	EOF Kind = iota
	Illegal
	Ident
	QualifiedIdent
	IntLit
	FloatLit
	StringLit
	DurationLit
	SizeLit
	Keyword
	Punct
	VirtualSemi
)

var kindNames = map[Kind]string{
	EOF:            "EOF",
	Illegal:        "Illegal",
	Ident:          "Ident",
	QualifiedIdent: "QualifiedIdent",
	IntLit:         "IntLit",
	FloatLit:       "FloatLit",
	StringLit:      "StringLit",
	DurationLit:    "DurationLit",
	SizeLit:        "SizeLit",
	Keyword:        "Keyword",
	Punct:          "Punct",
	VirtualSemi:    "VirtualSemi",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Token 的位置按 KNOWN-05：line 从 1 起；col 从行首第 1 个字符（rune）数起，含缩进空格。
type Token struct {
	Kind   Kind
	Lexeme string
	Line   int
	Col    int
}

// EndCol 返回紧接 token 最后一个字符之后的列号（= Col + rune 长度）。
func (t Token) EndCol() int { return t.Col + utf8.RuneCountInString(t.Lexeme) }

func (t Token) String() string {
	return fmt.Sprintf("%d:%d %s %q", t.Line, t.Col, t.Kind, t.Lexeme)
}

// FirstSegment 返回 QualifiedIdent 的第一段（内建类型判定 / Phase 2 分派用）。
func (t Token) FirstSegment() string {
	if i := strings.IndexByte(t.Lexeme, '.'); i >= 0 {
		return t.Lexeme[:i]
	}
	return t.Lexeme
}

// IsBuiltinType 报告 token 是否内建类型名。内建类型名是 Ident，但排除在 ;ᵥ 终止集合外。
func (t Token) IsBuiltinType() bool {
	if t.Kind != Ident {
		return false
	}
	return BuiltinTypes[t.FirstSegment()]
}

// IsBuiltinTypeHead 对 Ident/QualifiedIdent 都判定（Option.x 的 head 也是内建类型名）。
func (t Token) IsBuiltinTypeHead() bool {
	if t.Kind != Ident && t.Kind != QualifiedIdent {
		return false
	}
	return BuiltinTypes[t.FirstSegment()]
}

// BuiltinTypes 是 B-5 核对后的 20 个内建类型名。注意：不含 Range（KNOWN-03，Range 是 for 专用语法糖）。
var BuiltinTypes = map[string]bool{
	"Int": true, "Int32": true, "Int64": true,
	"Float": true, "F32": true, "F64": true,
	"Bool": true, "String": true, "Unit": true,
	"Bytes": true, "Never": true, "Duration": true,
	"Size": true, "Signal": true, "Option": true,
	"Result": true, "Vec": true, "Set": true,
	"Map": true, "Graph": true,
}

// Keywords 是关键字表（单一来源，便于 §1 关键字表补齐后一处修改）。
var Keywords = map[string]bool{
	"fn": true, "let": true, "mut": true, "if": true, "else": true,
	"for": true, "in": true, "match": true, "case": true, "against": true,
	"return": true, "break": true, "continue": true, "do": true,
	"struct": true, "logic": true, "instance": true, "neuron": true, "synapse": true,
	"place": true, "at": true, "with": true, "where": true,
	"hazardous": true, "strict": true, "guard": true,
	"emit": true, "invoke": true, "reuse": true, "learn": true,
	"output": true, "input": true, "capability": true, "structure": true, "handle": true,
	"local": true, "balance": true, "failover": true, "retry": true, "on_failure": true,
	"true": true, "false": true,
	"use": true, "as": true, "is": true, "delta": true, "similar": true, "exact": true,
}

// DurationUnits / SizeUnits：完整匹配单位（不回退）。
var DurationUnits = map[string]bool{
	"ns": true, "us": true, "µs": true, "ms": true,
	"s": true, "m": true, "h": true, "d": true,
}

var SizeUnits = map[string]bool{
	"B": true, "KB": true, "MB": true, "GB": true, "TB": true, "PB": true,
	"KiB": true, "MiB": true, "GiB": true, "TiB": true,
}
