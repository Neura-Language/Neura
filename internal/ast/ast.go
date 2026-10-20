// Package ast 定义 Neura v0.3.7 的 AST。
//
// 字段命名约定（与规范术语对齐，避免 §2/§9 混淆）：
//   - struct 用 Field（name: Type）
//   - instance structure 用 Binding（name: value），值是表达式
//   - logic 的 structure 用 Field，capability 用 FnSig
package ast

import "neura/internal/diag"

// File 是一个 .neura 文件。
type File struct {
	Decls []Decl
}

type Decl interface{ declNode() }
type Stmt interface{ stmtNode() }
type Expr interface{ exprNode() }
type Type interface{ typeNode() }

// ---------------- 类型 ----------------

// NamedType 覆盖 type 位置的内建类型名 / 限定名 / 泛型实参：
// Int, Option<Int>, Signal<Int>, Map<String, Int>, SomeLogic。
type NamedType struct {
	At   diag.Pos
	Name string
	Args []Type
}

// TupleType 只表示 ()、(T,)、(T, U)。单元素无尾逗号的 (T) 在 parser 阶段报
// E-PAREN-SINGLE-TYPE，不会构造出 TupleType{1 elem}。
type TupleType struct {
	At    diag.Pos
	Elems []Type
}

// FnType 是 type 位置的 fn 类型：fn(Int, Int) -> Int。
type FnType struct {
	At     diag.Pos
	Params []Type
	Result Type
}

func (*NamedType) typeNode() {}
func (*TupleType) typeNode() {}
func (*FnType) typeNode()    {}

// ---------------- 表达式 ----------------

type Ident struct {
	At   diag.Pos
	Name string
}

// QualifiedRef 来自 lexer 的 QualifiedIdent（Src.o / SomeLogic.run），Parts 已按 '.' 拆开。
type QualifiedRef struct {
	At    diag.Pos
	Parts []string
}

// Literal.Kind 取值：int / float / string / duration / size / bool。
type Literal struct {
	At   diag.Pos
	Kind string
	Text string
}

// UnitExpr 表示 physics 记法的数值+单位：55mV 的 55 与 mV。
// 词法上 55mV 是 IntLit + Ident，parser 仅在「紧邻」时合成 UnitExpr，
// 供 checker 规则 20 的 [ "-" ] (int|float) ident 模式匹配。
type UnitExpr struct {
	At    diag.Pos
	Value Expr // Literal
	Unit  string
}

type RangeExpr struct {
	At        diag.Pos // 整个区间表达式起点
	OpAt      diag.Pos // .. 或 ..= 的位置（E-RANGE-OUTSIDE-FOR 的 span）
	Lo, Hi    Expr
	Inclusive bool // ..=
}

type UnaryExpr struct {
	At diag.Pos
	Op string // - !
	X  Expr
}

type BinaryExpr struct {
	At diag.Pos
	Op string
	L  Expr
	R  Expr
}

type CallExpr struct {
	At   diag.Pos
	Fn   Expr
	Args []Expr
}

type FieldExpr struct {
	At   diag.Pos
	X    Expr
	Name string
}

type ParenExpr struct {
	At diag.Pos
	X  Expr
}

func (*Ident) exprNode()        {}
func (*QualifiedRef) exprNode() {}
func (*Literal) exprNode()      {}
func (*UnitExpr) exprNode()     {}
func (*RangeExpr) exprNode()    {}
func (*UnaryExpr) exprNode()    {}
func (*BinaryExpr) exprNode()   {}
func (*CallExpr) exprNode()     {}
func (*FieldExpr) exprNode()    {}
func (*ParenExpr) exprNode()    {}

// ---------------- 语句 ----------------

type Block struct {
	At    diag.Pos
	Stmts []Stmt
}

type LetStmt struct {
	At      diag.Pos
	Name    string
	Mutable bool
	Type    Type // 可为 nil
	Value   Expr
}

type AssignStmt struct {
	At     diag.Pos
	Target Expr
	Value  Expr
}

type ExprStmt struct {
	At diag.Pos
	X  Expr
}

type DoStmt struct {
	At   diag.Pos
	Body *Block
}

type ForStmt struct {
	At   diag.Pos
	Var  string
	Iter Expr
	Body *Block
}

// IfStmt.Else 为 nil / *Block / *IfStmt（else if）。
type IfStmt struct {
	At   diag.Pos
	Cond Expr
	Then *Block
	Else Stmt
}

// MatchStmt：match x against LogicDB { case T(bind) => ... }
type MatchStmt struct {
	At      diag.Pos
	Subject Expr
	Against Expr
	Arms    []*Arm
}

// Arm.Pattern 是 case 后的类型/逻辑名；Bind 可以为空。Body 是 logic_arm_rhs 五分支之一。
type Arm struct {
	At      diag.Pos
	Pattern *QualifiedRef
	Bind    string
	Body    Stmt
}

type ReturnStmt struct {
	At    diag.Pos
	Value Expr // 可为 nil
}

type BreakStmt struct{ At diag.Pos }
type ContinueStmt struct{ At diag.Pos }

type EmitStmt struct {
	At diag.Pos
	X  Expr
}

type InvokeStmt struct {
	At diag.Pos
	X  Expr
}

type ReuseStmt struct {
	At diag.Pos
	X  Expr
}

type LearnStmt struct {
	At diag.Pos
	X  Expr
}

// Block 也可作为 Stmt 出现（if 的 else 分支直接是 Block）。
func (*Block) stmtNode()        {}
func (*LetStmt) stmtNode()      {}
func (*AssignStmt) stmtNode()   {}
func (*ExprStmt) stmtNode()     {}
func (*DoStmt) stmtNode()       {}
func (*ForStmt) stmtNode()      {}
func (*IfStmt) stmtNode()       {}
func (*MatchStmt) stmtNode()    {}
func (*ReturnStmt) stmtNode()   {}
func (*BreakStmt) stmtNode()    {}
func (*ContinueStmt) stmtNode() {}
func (*EmitStmt) stmtNode()     {}
func (*InvokeStmt) stmtNode()   {}
func (*ReuseStmt) stmtNode()    {}
func (*LearnStmt) stmtNode()    {}

// ---------------- 声明 ----------------

type FnDecl struct {
	At     diag.Pos
	Name   string
	Params []*Param
	Result Type
	Body   *Block
}

type Param struct {
	At   diag.Pos
	Name string
	Type Type
}

type StructDecl struct {
	At     diag.Pos
	Name   string
	Fields []*Field
}

type Field struct {
	At   diag.Pos
	Name string
	Type Type
}

// LogicDecl：hazardous 等修饰符放在 Modifiers（来自 with 列表）。
type LogicDecl struct {
	At         diag.Pos
	Name       string
	Modifiers  []string
	Strict     bool
	Guard      Expr
	Structure  []*Field
	Capability []*FnSig
}

type FnSig struct {
	At     diag.Pos
	Name   string
	Params []*Param
	Result Type
	Body   *Block // 可为 nil
}

// InstanceDecl.structure 用 Binding（name: value）。
type InstanceDecl struct {
	At       diag.Pos
	Name     string
	Logic    string
	Bindings []*Binding
}

type Binding struct {
	At    diag.Pos
	Name  string
	Value Expr
}

type NeuronDecl struct {
	At      diag.Pos
	Name    string
	Ports   []*Port
	Handler *Block
}

type Port struct {
	At   diag.Pos
	Dir  string // input / output
	Name string
	Type Type
}

type SynapseDecl struct {
	At        diag.Pos
	Src       *QualifiedRef
	Dst       *QualifiedRef
	OnFailure *Policy // failure_policy
	FailoverP *Policy // failover_policy
}

// Policy：Kind 是策略名（retry / failover / round_robin / consistent_hash ...）。
type Policy struct {
	At   diag.Pos
	Kind string
	Args []Expr
}

type PlaceDecl struct {
	At        diag.Pos
	Name      string
	AtSpec    *AtSpec
	Balance   *Policy
	Where     Expr
	FailoverP *Policy
	OnFailure *Policy
}

type AtSpec struct {
	At    diag.Pos
	Local bool
	Nodes []string
}

func (*FnDecl) declNode()       {}
func (*StructDecl) declNode()   {}
func (*LogicDecl) declNode()    {}
func (*InstanceDecl) declNode() {}
func (*NeuronDecl) declNode()   {}
func (*SynapseDecl) declNode()  {}
func (*PlaceDecl) declNode()    {}
