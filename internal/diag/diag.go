// Package diag 定义错误码、位置和诊断结构。parser（Phase 2）、checker（Phase 3）
// 共用同一套 Error，保证 §17 的 expected_error_span 由实际输出直接回填（阶段 B）。
package diag

import "fmt"

// Pos 是源码位置：Line 从 1 起；Col 按 KNOWN-05 从行首第 1 个字符（rune）数起，含缩进空格。
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

func (p Pos) IsZero() bool { return p.Line == 0 && p.Col == 0 }

// Error 是编译期诊断：错误码 + 精确 span 起点 + 可读 message。
type Error struct {
	Code string
	Pos  Pos
	Msg  string
}

func (e Error) Error() string {
	return fmt.Sprintf("%d:%d: %s: %s", e.Pos.Line, e.Pos.Col, e.Code, e.Msg)
}

// 错误码。§12 注册表 + v0.3.7 追加项 + 本实现需要的语法码。
const (
	// 词法
	CodeLexError = "E-LEX-ERROR"

	// 语法（Phase 2）
	CodeUnexpectedToken = "E-UNEXPECTED-TOKEN"
	CodeExpectedType    = "E-EXPECTED-TYPE"
	CodeParenSingleType = "E-PAREN-SINGLE-TYPE" // N7
	CodeSameLineStmt    = "E-SAME-LINE-STMT"    // N1
	CodeMissingSep      = "E-MISSING-SEPARATOR"
	CodeWrongPolicy     = "E-WRONG-POLICY"      // N9 / N10
	CodeRangeOutsideFor = "E-RANGE-OUTSIDE-FOR" // KNOWN-03 / 规则 36
	CodeAssignLHS       = "E-ASSIGN-LHS"        // KNOWN-06
	CodeHazardNoWith    = "E-HAZARD-NO-WITH"    // N2（hazardous 位置错误在 parser 就能判定）

	// 语义（Phase 3）
	CodeHazardNoGuard      = "E-HAZARD-NO-GUARD"      // N3
	CodeUnknownWhereIdent  = "E-UNKNOWN-WHERE-IDENT"  // N4
	CodeHashFieldNotFound  = "E-HASH-FIELD-NOT-FOUND" // N5
	CodeUnitOutsidePhysics = "E-UNIT-OUTSIDE-PHYSICS" // N8
	CodeBreakOutsideLoop   = "E-BREAK-OUTSIDE-LOOP"   // N11
	CodeWhereNotPure       = "E-WHERE-NOT-PURE"       // N12

	// 语义（Phase 3：类型检查与结构性规则）
	CodeDupDecl             = "E-DUP-DECL"
	CodeUnknownType         = "E-UNKNOWN-TYPE"
	CodeDupField            = "E-DUP-FIELD"
	CodeInstanceLogic       = "E-INSTANCE-LOGIC-NOT-FOUND"
	CodeBindingMissing      = "E-BINDING-MISSING"
	CodeBindingExtra        = "E-BINDING-EXTRA"
	CodeBindingType         = "E-BINDING-TYPE"
	CodeUnknownFn           = "E-UNKNOWN-FN"
	CodeUnknownMethod       = "E-UNKNOWN-METHOD"
	CodeNotCallable         = "E-NOT-CALLABLE"
	CodeArgCount            = "E-ARG-COUNT"
	CodeArgType             = "E-ARG-TYPE"
	CodeReturnType          = "E-RETURN-TYPE"
	CodeMissingReturn       = "E-MISSING-RETURN"
	CodeUnknownIdent        = "E-UNKNOWN-IDENT"
	CodeUnknownField        = "E-UNKNOWN-FIELD"
	CodeTypeMismatch        = "E-TYPE-MISMATCH"
	CodeCondNotBool         = "E-COND-NOT-BOOL"
	CodeForNotRange         = "E-FOR-NOT-RANGE"
	CodeAssignImmutable     = "E-ASSIGN-IMMUTABLE"
	CodeEmitContext         = "E-EMIT-CONTEXT"
	CodeContinueOutsideLoop = "E-CONTINUE-OUTSIDE-LOOP"
	CodeUnknownNeuron       = "E-UNKNOWN-NEURON"
	CodePortNotFound        = "E-PORT-NOT-FOUND"
	CodePortType            = "E-PORT-TYPE-MISMATCH"
	CodePortDirection       = "E-PORT-DIRECTION"
	CodePlaceTarget         = "E-PLACE-TARGET-NOT-FOUND"
	CodeMatchTarget         = "E-MATCH-TARGET"
	CodeMatchPattern        = "E-MATCH-PATTERN"
	CodeSelfOutside         = "E-SELF-OUTSIDE-LOGIC"
)

// Registry 是错误码注册表（CI 按码对拍，不依赖规则编号）。
var Registry = []struct {
	Code string
	Rule string
	Msg  string
}{
	{CodeUnexpectedToken, "parser", "此处不应出现该 token"},
	{CodeExpectedType, "parser", "期望一个类型"},
	{CodeParenSingleType, "parser", "单元素括号在类型位置非法"},
	{CodeSameLineStmt, "parser", "同行多条语句必须用 ; 分隔"},
	{CodeMissingSep, "parser", "语句之间缺少 ; 分隔"},
	{CodeWrongPolicy, "parser", "policy 用错位置（failure_policy / failover_policy 严格分开）"},
	{CodeRangeOutsideFor, "36", "a..b 仅在 for 的 expr 位置合法"},
	{CodeAssignLHS, "11", "= 左侧不能是字面量"},
	{CodeHazardNoWith, "parser", "hazardous 必须作为 with 的 modifier"},
	{CodeHazardNoGuard, "checker", "hazardous logic 必须 strict 或带 guard"},
	{CodeUnknownWhereIdent, "checker", "where 中引用了未知标识符"},
	{CodeHashFieldNotFound, "checker", "consistent_hash 字段不在 structure 中"},
	{CodeUnitOutsidePhysics, "20", "unit_lit 仅在 physics_value 位置合法"},
	{CodeBreakOutsideLoop, "checker", "break 只能出现在循环体内"},
	{CodeWhereNotPure, "35", "where 表达式的函数必须是纯函数"},
	{CodeLexError, "lexer", "词法错误"},

	// Phase 3 规则（编号按 v0.3.7 清单；规则 14 已废弃，见日志输出）
	{CodeDupDecl, "1", "顶层声明重名"},
	{CodeUnknownType, "2", "引用了未知类型"},
	{CodeDupField, "4", "字段重复声明"},
	{CodeInstanceLogic, "5", "instance 指向的 logic 不存在"},
	{CodeBindingMissing, "5", "instance structure 缺少 logic 字段"},
	{CodeBindingExtra, "5", "instance structure 多出 logic 没有的字段"},
	{CodeBindingType, "5", "binding 的值类型与 logic 字段类型不符"},
	{CodeUnknownFn, "9", "调用了未定义的函数"},
	{CodeUnknownMethod, "9", "调用了未定义的 capability 方法"},
	{CodeNotCallable, "9", "该值不可调用"},
	{CodeArgCount, "10", "调用参数个数不符"},
	{CodeArgType, "10", "调用参数类型不符"},
	{CodeReturnType, "17", "返回类型不符"},
	{CodeMissingReturn, "17", "非 Unit 返回类型的函数体为空"},
	{CodeSelfOutside, "19", "self 只能出现在 logic capability 内"},
	{CodeUnknownNeuron, "28", "synapse 引用了未定义的 neuron"},
	{CodePortNotFound, "28", "synapse 引用的端口不存在"},
	{CodePortDirection, "28", "synapse 两端端口方向不对（左 output / 右 input）"},
	{CodePortType, "29", "synapse 两端端口类型不匹配"},
	{CodePlaceTarget, "34", "place 目标未声明"},
	{CodeEmitContext, "37", "emit 仅在 neuron handler 内合法"},
	{CodeUnknownIdent, "checker", "未知标识符"},
	{CodeUnknownField, "checker", "未知字段"},
	{CodeTypeMismatch, "checker", "类型不匹配"},
	{CodeCondNotBool, "checker", "if 条件必须是 Bool"},
	{CodeForNotRange, "checker", "for 的迭代值必须是 Range"},
	{CodeAssignImmutable, "checker", "给不可变变量赋值"},
	{CodeContinueOutsideLoop, "checker", "continue 只能出现在循环体内"},
	{CodeMatchTarget, "checker", "match 的 against 必须是 LogicDB"},
	{CodeMatchPattern, "checker", "case 分支不是该 logic 的 instance"},
}
