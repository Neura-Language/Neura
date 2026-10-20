# Neura v0.3.7 —— Go 最小可用编译器 + 运行时

`neura` 是 Neura v0.3.7 的参考实现：**Lexer → Parser → Checker → Runtime** 四阶段齐全，
带虚拟分号、AST dump、错误码注册表、§17 阶段 B 的 span 回填、以及可复现的 MatchTrace。

```
Go 1.21+ / 标准库零依赖 / 7472 行 Go / 178 组测试断言全绿
```

## 快速开始

```bash
git clone … && cd neura          # 或直接用当前目录
go test ./...                    # 全部测试
go build -o neura-bin ./cmd/neura

# 编译期
./neura-bin --dump-tokens examples/lex_demo.neura
./neura-bin --dump-ast    examples/full.neura
./neura-bin --check       examples/full.neura        # parser + checker
./neura-bin --dump-errors testdata/neg/N12.neura     # line:col CODE msg

# 运行期
./neura-bin run examples/run_demo.neura --entry dispatch --arg @RouterEU --trace /tmp/trace.jsonl
./neura-bin run examples/run_demo.neura --handler Worker --arg 42     # emit 路径
./neura-bin run examples/run_demo.neura --entry sum_between --arg 5 --arg 10
```

## CLI 参考

| 命令 / 参数 | 说明 |
|---|---|
| `--dump-tokens <file>` | 词法 token 流（`;ᵥ` 标 `#inserted`，内建类型标 `#builtin-type`） |
| `--dump-ast <file>` | 缩进式 AST dump（字段顺序即结构体字段顺序） |
| `--check <file>` | parser + checker 全量静态检查（顺带打印 `rule 14 deprecated…`） |
| `--dump-errors <file>` | 诊断列表：`line:col CODE message` |
| `--emit-expected <files…>` | 输出 JSON（`{file, diagnostics[]}`），§17 阶段 B 的回填数据源 |
| `run <file> --entry <fn>` | 以某函数为入口执行（默认 `main`） |
| `run … --handler <neuron>` | 直接执行某个 neuron 的 `handle { … }`（Phase 4 无事件循环，emit 靠它可达） |
| `run … --arg <v>` | 实参：`@实例名` / 整数 / 其他按字符串；可重复，按顺序绑定 |
| `run … --trace <path>` | MatchTrace JSONL 输出；**0 条记录时不生成文件并提示原因** |
| `run … --no-check` | 跳过静态检查（不推荐，仅供调试） |

## 目录结构

```
cmd/neura/main.go          CLI（编译期命令 + run 子命令）
internal/lexer/            Phase 1：token、;ᵥ 插入（§1.3 六条例外）
internal/ast/              AST 定义（struct 用 Field / instance 用 Binding）
internal/astdump/          反射式 AST 打印
internal/parser/           Phase 2：递归下降 + 语句/声明级错误恢复
internal/checker/          Phase 3：规则 1/2/4/5/9/10/17/19/20/28/29/34/35/36/37/39 + N3/N4/N5/N11/N12
internal/runtime/          Phase 4：LogicDB、match、reuse/invoke/emit、MatchTrace
internal/diag/             错误码 + Pos/Error（parser/checker 共用）
examples/                  lex_demo / full / run_demo
testdata/prelude.neura     §17 前置定义块（原文）
testdata/neg/N01–N12.neura §17 负例（prelude + 原文片段）
testdata/pos/P01–P07.neura 正例（parse+check OK）
testdata/expected_errors.json  §17 阶段 B 回填（--emit-expected 生成）
scripts/gen_expected.sh    重新生成上表
```

## 实现范围

| 阶段 | 内容 | 验收 |
|---|---|---|
| 1 Lexer | 11 类 token、`..`/`..=`/`~=` 独立 token、单位整段匹配不回退、虚拟分号 + 6 条例外（含行首 `=`、行首 `else`）、20 个内建类型名走 Ident 但排除终止集合 | 5 个必过 case + 16 个测试 |
| 2 Parser | type 四分派、tuple 拒绝 `(T)`、logic_arm_rhs 五分派、failure/failover policy 严格分开、instance binding vs struct field、`;ᵥ`≡`;` | 完整示例 parse 无错、25 个测试 |
| 3 Checker | 类型检查、纯函数判定、Range、emit 上下文、端口匹配、错误码注册表、规则 14 跳过 | N01–N12 正确码/span、P01–P07 通过、20 个测试 |
| 4 Runtime | LogicDB 三套索引、match 顺序、reuse/invoke/emit/learn、MatchTrace、place 仅 local | 完整示例可运行、tie-break 可复现、21 个测试 |

## 错误码注册表

| 阶段 | 错误码 |
|---|---|
| lexer | `E-LEX-ERROR` |
| parser | `E-UNEXPECTED-TOKEN` `E-EXPECTED-TYPE` `E-PAREN-SINGLE-TYPE` `E-SAME-LINE-STMT` `E-MISSING-SEPARATOR` `E-WRONG-POLICY` `E-RANGE-OUTSIDE-FOR` `E-ASSIGN-LHS` `E-HAZARD-NO-WITH` |
| checker | `E-HAZARD-NO-GUARD` `E-UNKNOWN-WHERE-IDENT` `E-HASH-FIELD-NOT-FOUND` `E-UNIT-OUTSIDE-PHYSICS` `E-BREAK-OUTSIDE-LOOP` `E-CONTINUE-OUTSIDE-LOOP` `E-WHERE-NOT-PURE` `E-DUP-DECL` `E-UNKNOWN-TYPE` `E-DUP-FIELD` `E-INSTANCE-LOGIC-NOT-FOUND` `E-BINDING-MISSING/EXTRA/TYPE` `E-UNKNOWN-FN` `E-UNKNOWN-METHOD` `E-NOT-CALLABLE` `E-ARG-COUNT` `E-ARG-TYPE` `E-RETURN-TYPE` `E-MISSING-RETURN` `E-UNKNOWN-IDENT` `E-UNKNOWN-FIELD` `E-TYPE-MISMATCH` `E-COND-NOT-BOOL` `E-FOR-NOT-RANGE` `E-ASSIGN-IMMUTABLE` `E-EMIT-CONTEXT` `E-UNKNOWN-NEURON` `E-PORT-NOT-FOUND` `E-PORT-TYPE-MISMATCH` `E-PORT-DIRECTION` `E-PLACE-TARGET-NOT-FOUND` `E-MATCH-TARGET` `E-MATCH-PATTERN` `E-SELF-OUTSIDE-LOGIC` |

规则 14 已废弃（语义并入 37）：checker 跳过并打印 `rule 14 deprecated, see rule 37`。

## §17 阶段 B：span 回填结果

`testdata/expected_errors.json` 由 `--emit-expected` 生成（**parser/checker 实际输出，不是人工数**），
`internal/checker/expected_test.go` 做阻断式对拍：任何 span/错误码/message 漂移都会失败，直到重新生成并 review diff。

下表 `正文行:列` 已扣除 25 行前置定义块（prelude 23 行 + 空行 + 标记行）：

| 用例 | 错误码 | 实际 span | message |
|---|---|---|---|
| N01 | `E-SAME-LINE-STMT` | 2:13（文件内 27:13） | 同行多条语句必须用 ; 分隔 |
| N02 | `E-HAZARD-NO-WITH` | 1:12（26:12） | hazardous 必须作为 with 的 modifier |
| N03 | `E-HAZARD-NO-GUARD` | 1:1（26:1） | hazardous logic 必须 strict 或带 guard |
| N04 | `E-UNKNOWN-WHERE-IDENT` | 1:32（26:32） | 未知标识符 foo_bar |
| N05 | `E-HASH-FIELD-NOT-FOUND` | 1:48（26:48） | consistent_hash 字段 nonexistent 不在 Bar 的 structure 中 |
| N06 | — | — | 无编译期错误（`[runtime-test]`） |
| N07 | `E-PAREN-SINGLE-TYPE` | 1:9（26:9） | 单元素括号在类型位置非法 |
| N08 | `E-UNIT-OUTSIDE-PHYSICS` | 2:11（27:11） | unit_lit 仅在 physics_value 位置合法 |
| N09 | `E-WRONG-POLICY` | 1:27（26:27） | retry 属于 failure_policy |
| N10 | `E-WRONG-POLICY` | **1:35**（26:35） | failover 属于 failover_policy |
| N11 | `E-BREAK-OUTSIDE-LOOP` | 2:3（27:3） | break 只能出现在循环体内 |
| N12 | `E-WHERE-NOT-PURE` | **5:37**（30:37） | where 表达式的函数必须是纯函数（含 learn 日志副作用） |

**与 §17 人工数/旧决策的差异（这正是 v0.3.7 要暴露的）**

| 用例 | §17 人工数 | KNOWN 决策 | 实际输出 | 结论 |
|---|---|---|---|---|
| N04 | 1:32 | — | 1:32 | 一致 |
| N05 | 1:48 | — | 1:48 | 一致 |
| N09 | 1:27 | — | 1:27 | 一致 |
| N10 | 1:51 | KNOWN-01 说 33 | **1:35** | 人工数与旧决策都不对，35 才是 `failover` 的 `f` |
| N12 | 5:27 | KNOWN-02 说 35 | **5:37** | 27 是 `gpu_mem` 的 `p`，35 是 `= `，37 才是 `bad_fn` 的 `b` |
| N01/N02/N03/N07/N08/N11 | 3:13 / 2:12 / 1:1 / 1:9 / 2:11 / 2:3 | — | 与之一致（行号差 1 是原文片段多一行注释） | 一致 |

重新生成：

```bash
bash scripts/gen_expected.sh     # = go build + neura-bin --emit-expected testdata/neg/N*.neura
go test ./internal/checker -run TestExpectedSpansBackfill
```

## 运行时设计

**LogicDB**：`byName` / `byLogic` / `byField` 三套索引；所有遍历出口都排序，不让 map 顺序泄漏。

**match 求值顺序**（`match x against LogicDB { case P => … }`）：

1. 候选 = 与该 subject 同 logic 的实例，且模式 `P` 命中（logic 名或 instance 名）；
2. `strict` = 字段集合相同且逐字段规范值相同；
3. `similarity` = 相同字段值个数 / 字段并集大小（纯函数，值域 [0,1]）；
4. `guard` = 该 logic 的 `guard <expr>`，用实例字段求值；
5. 排序：strict 优先 → 相似度降序 → **实例名升序**（确定性 tie-break）；
6. 取首个 → 绑定 `case P(bind)` 的 bind → 执行 arm；无候选/guard 全拒 → 跳过（§17 N6 语义）。

**MatchTrace**：每条 arm 求值一行 JSON，字段固定、无时间戳、候选已排序，因此两次运行字节级一致：

```json
{"seq":1,"subject":"RouterEU","logic":"Router","arm":"RouterEU",
 "candidates":[{"instance":"RouterEU","logic":"Router","strict":true,"score":1,"guard":true}],
 "chosen":"RouterEU","reason":"strict","bind":"e"}
```

`reason ∈ {strict, similarity, no-candidate, guard-rejected, subject-not-instance}`。
**只有 `match … against LogicDB` 会产生记录**；不含 match 的入口（如 `classify`）trace 为 0 条，
CLI 会提示「未生成文件」而不是留下一个 0 字节空文件。

**语句语义**：`emit` → 输出队列（仅 neuron handler 合法）；`invoke` → 执行 capability 方法；
`reuse` → 复用匹配到的实例；`learn` → 日志副作用（返回 Unit，不污染返回值）。

**place**：只实现 `local`；`at "node"` / `at ["a","b"]` 打 `stub` warning 并按 local 处理。

**§14 明确不做（全部显式 stub）**：embedding 相似度、custom similarity、自动 delta、跨节点 exactly_once。

## 设计假设 / 重建部分

规范 §2–§11 EBNF 与 §12 规则正文不在输入包内，以下为实现时重建并已在代码注释标注：

- 顶层声明自定界（声明之间不强制 `;ᵥ`；`synapse … failover` 以关键字结尾时 lexer 不插 `;ᵥ`）。
- `logic` 头：`logic Name [with mods] [strict] [guard expr] { … }`；裸 `hazardous` 在 parser 即报 `E-HAZARD-NO-WITH`。
- `neuron` 体：`input` / `output` / `handle`；`capability` 体：`name(params) [-> Type] [Block]`。
- `instance` 体只允许 `structure: { name: value }`（binding）；`struct` 用 `name: Type`（field）。
- `failure_policy = retry(n)`；`failover_policy = failover[(…)]`，互用位置立即 `E-WRONG-POLICY`。
- 隐式尾表达式作返回值（§17 前置块 `fn is_liquid(...) -> Bool { s.temperature > 0.0 }` 依赖它）。
- `where` 可引用的标识符 = 内建资源（`gpu_mem/cpu_mem/gpu_util/now`）+ 目标 structure 字段。
- 规则编号语义：1 重名、2 未知类型、4 字段重复、5 instance↔logic、9 未知函数、10 参数、17 返回、19 self 上下文。
- 运行时入口/实参：`--arg` 按顺序绑定；实参**多传报错**，少传按类型合成（logic → 空字段 `<subject>`）。
- `is_hazardous` / `hash_of` 为确定性占位实现（FNV-1a）。

## 已知限制

- 无 `use` 导入、闭包、泛型形参、字符串插值（输入包未出现这些产生式）。
- 无事件循环；neuron 只在 `--handler` 下执行一次。
- `physics_value` 位置未定义，因此 unit_lit 在 expr 位置一律报 `E-UNIT-OUTSIDE-PHYSICS`。
- `place … where` 不参与运行时选择（部署是 stub）；静态校验仍在 checker。

## 本轮修复记录（用户报告的两个问题）

| 现象 | 根因 | 修复 |
|---|---|---|
| `range_sum(5, 10)` 返回 16 | **不是 Range 求值 bug**：`range_sum` 只有一个参数，运行时把第 2 个实参静默丢弃，等价于 `range_sum(5)` = (0..5)+(0..=3) = 10+6 = 16 | 运行时增加实参个数硬校验（`range_sum 需要 1 个参数，实际传入 2 个`），并在 `TestRangeNonZeroLowerBound` 覆盖非零下界（5..10=35、5..=10=45） |
| `classify --trace` 生成 0 字节文件 | `classify` 里没有 `match … against LogicDB`，本来就不产生记录；但 CLI 仍创建了空文件，看起来像 trace 写入丢失 | 0 条记录时不建文件并明确提示；另给「subject 不是 instance」补一条 `subject-not-instance` 记录，任何 match 尝试都留痕 |

回归测试：`TestRangeNonZeroLowerBound`、`TestRuntimeArityError`、`TestRunDemoSumBetween`、
`TestTraceRecordsNonInstanceSubject`、`TestNoMatchMeansNoRecords`。

## CI 建议

```bash
gofmt -l .                 # 必须为空
go vet ./...
go test ./... -count=1

# span 回填守护（阶段 B，阻断式）
bash scripts/gen_expected.sh && git diff --exit-code testdata/expected_errors.json

# 虚拟分号状态不能悄悄变化
./neura-bin --dump-tokens examples/lex_demo.neura > /tmp/tokens.txt
```

## 六个 KNOWN 决策的落地位置

| 决策 | 落地 |
|---|---|
| KNOWN-01 / 02：不硬编码 N10/N12 列号 | 代码零列号常量；`expected_errors.json` 由实际输出生成，实测 35 / 37 |
| KNOWN-03：Range 从 prim_type 删除 | `internal/parser` type 位置拒绝 `Range`；`internal/ast` 无 Range 类型 |
| KNOWN-04：规则 14 废弃 | `checker.Check` 跳过并输出 `rule 14 deprecated, see rule 37` |
| KNOWN-05：列号从行首含缩进 | `internal/lexer` 用 rune 列计数；`TestColumnBasis` 钉住 `-` 在 col 11 |
| KNOWN-06：`let x = 1 = 2` 文案 | `internal/parser` `assignLHSMsg` 逐字一致，测试用 `==` 比对 |
