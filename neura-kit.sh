#!/usr/bin/env bash
set -euo pipefail
SPEC="${1:?用法: $0 <规范.txt> [输出.txt]}"
OUT="${2:-neura-kit.txt}"
[ -f "$SPEC" ] || { echo "找不到 $SPEC"; exit 1; }

{
cat <<'HEADER'
================================================================
  NEURA v0.3.7 —— 编译器实现输入包（单文件）
================================================================

【怎么用】
  1. 本文件 = 规范全文 + 6 条已知矛盾决策 + 4 阶段工作流 + 验收标准
  2. 把本文件整个扔给 AI（Claude/GPT/DeepSeek）
  3. AI 每次只输出一个 Phase，你验收后说 "继续 Phase N+1"
  4. 不要一次要全部 4 Phase

================================================================
  第一部分：Neura v0.3.7 规范全文
================================================================

HEADER
cat "$SPEC"
cat <<'DECISION_EOF'

================================================================
  第二部分：已知矛盾决策单（AI 必须按此实现，不按规范原文）
================================================================

KNOWN-01  N10 的 col=51 是错的
  实际 v0.3.7 示例改成 Src.o -> Dst.i 后，failover 的 f 在 col 33。
  决策：不硬编码列号，span 由 parser 输出，错误码 E-WRONG-POLICY。

KNOWN-02  N12 的 col=27 是错的
  col=27 是 gpu_mem 的 p，bad_fn 的 b 在 col 35。
  决策：同上，不硬编码。

KNOWN-03  Range 类型矛盾
  §2.2 prim_type 含 "Range"，规则 36 说不可以显式声明。
  决策：从 prim_type 删除 "Range"。Range 是 for 专用语法糖，
       let r: Range = 0..10 在 parser 阶段报 E-RANGE-OUTSIDE-FOR。

KNOWN-04  规则 14 已废弃
  原语义（do block 禁 emit）已并入规则 37。
  决策：checker 跳过规则 14，打印 "rule 14 deprecated, see rule 37"。

KNOWN-05  span 列号基准
  决策：列号从行首第 1 字符数起，含缩进空格。
       例 "  let x = -55mV" 的 - 在 col 11。

KNOWN-06  let x = 1 = 2 的错误信息
  决策：E-ASSIGN-LHS，message 用：
       "= 左侧必须是变量，不能是字面量 1（是否漏了 ; ？）"

================================================================
  第三部分：4 阶段工作流
================================================================

PHASE 1 —— LEXER
  目标：输入 .neura，输出 Vec<Token>
  必读：§1 词法、KNOWN-03、KNOWN-05
  必须实现：
    - Token 枚举：Ident/QualifiedIdent/IntLit/FloatLit/StringLit/
      DurationLit/SizeLit/Keyword/Punct/VirtualSemi/EOF
    - duration_lit/size_lit 完整匹配不回退（55mV→55+mV，20ms→DurationLit）
    - ..  和 ..=  独立 token
    - ~= 独立 token（仅 logic_arm）
    - 虚拟分号 ;ᵥ（§1.3 终止集合 + 5 条例外）
    - 20 个内建类型名生成 ident 但排除终止集合
  测试用例（必须过）：
    20ms -> [DurationLit]
    1.5GB -> [SizeLit]
    -55mV -> [-, 55, mV]
    0..10 -> [0, .., 10]
    Option\n<Int> -> [Option, <, Int]（不插 ;ᵥ）
  验收：cargo test lexer

PHASE 2 —— PARSER
  前提：Phase 1 通过
  必读：§2-§11 EBNF、KNOWN-01、KNOWN-03
  必须实现：
    - 递归下降 + LL(1) + 少量 lookahead
    - type 位置四分派（( / 内建 / qualified / fn）
    - tuple_type 拒绝 (T)，接受 ()/(T,)/(T,U)
    - logic_arm_rhs 五分派
    - failure_policy 与 failover_policy 严格分开
    - instance structure 用 binding，struct 用 field
    - ;ᵥ 与 ; 等价
    - 错误码至少 5 个（见 §12 注册表）
  验收：cargo test parser + parse 完整示例 --dump-ast

PHASE 3 —— CHECKER
  前提：Phase 2 通过
  必读：§12、KNOWN-04、§17 负例/正例
  必须实现（优先）：
    规则 1-2/4-5/9/10/17/19/20/28/29/34/35+39/36/37
    跳过规则 14
  验收：全部负例 N01-N12 报正确 error_code，正例 P01-P07 通过

PHASE 4 —— RUNTIME（最小）
  前提：Phase 3 通过
  必读：§6/§7/§10
  实现：
    - LogicDB：内存版，倒排索引，纯函数打分，确定性 tie-break
    - match：strict 先 → 相似 → guard → 排序
    - reuse / invoke / emit / MatchTrace(JSONL)
    - place 只实现 local，at 打 stub warning
  不实现（打 stub）：
    embedding / custom similarity / 自动 delta / 跨节点 exactly_once
  验收：cargo run -- run 完整示例

================================================================
  第四部分：验收标准
================================================================

Phase 1 通过 = 5 个 lexer test case 全过 + --dump-tokens 含 ;ᵥ
Phase 2 通过 = 完整示例 parse 无错 + AST dump 清晰
Phase 3 通过 = N01-N12 报错 + P01-P07 通过 + 规则 14 跳过
Phase 4 通过 = 完整示例能运行 + tie-break 顺序对

全部通过 = Neura v0.3.7 最小可运行版本完成。

================================================================
  第五部分：AI 开场白模板（复制下面这段给 AI）
================================================================

---
你是 Neura 语言编译器的实现者。我给你一份 txt 文件，包含：
规范全文、6 条已知矛盾决策、4 阶段工作流、验收标准。

请：
1. 先复述你对 6 个 KNOWN 决策的理解（一句话一个）
2. 确认你准备从 Phase 1 (Lexer) 开始
3. 等待我确认，不要主动进入 Phase 2

每次只输出一个 Phase 的代码。我跑验收通过后说 "继续 Phase N+1"。

不要做：
- 不要一次输出全部 4 Phase
- 不要硬编码 KNOWN-01/02 的旧列号
- 不要实现 §14 明确不做的功能
---

================================================================
  文件结束
================================================================
DECISION_EOF
} > "$OUT"

echo "生成: $OUT"
echo "行数: $(wc -l < "$OUT")"
echo "字节: $(wc -c < "$OUT")"
