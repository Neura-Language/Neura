# Neura v0.3.7 —— Go 最小可用编译器 + 运行时

`neura` 是 Neura v0.3.7 的参考实现：**Lexer → Parser → Checker → Runtime** 四阶段齐全，
带虚拟分号、AST dump、错误码注册表、§17 阶段 B 的 span 回填、以及可复现的 MatchTrace。

> **v0.4 增量**：异步事件循环、跨节点 exactly-once、Place Adapter、LogicDB 相似度扩展见文末「附录 A」。以下为 v0.3.7 基线文档，保持原样。


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


---

# 附录 A：Neura v0.4 Roadmap 核心实现（事件循环 / exactly-once / Place / 相似度）

> 事件循环 / 轻量跨节点 exactly-once / Place Adapter / LogicDB 相似度扩展
>
> 基线：Neura v0.3.7（Lexer/Parser/Checker、LogicDB、JSON-over-TCP RPC、S2S AES-256-GCM、静态节点表 failover）。
>
> 纪律：**零第三方依赖、不引入 Paxos/Raft、所有跨节点控制面流量必须走既有 S2S 加密链路、编译器强类型推导不破坏**。

---

## 0. 总览

```text
                         ┌────────────────────────────┐
  .neura ──lexer/parser──▶ ast ──checker──▶ 强类型符号表
                         └────────────┬───────────────┘
                                      │
              ┌───────────────────────┼────────────────────────┐
              ▼                       ▼                        ▼
   ┌──────────────────┐    ┌────────────────────┐    ┌────────────────────┐
   │ Event Loop        │    │ Place Adapter      │    │ LogicDB Similarity │
   │ actor + mailbox   │    │ labels/metrics     │    │ Jaccard / SimHash  │
   │ bounded backpressure│  │ where compiler     │    │ Embedding probe    │
   └────────┬─────────┘    └─────────┬──────────┘    └────────────────────┘
            │                        │
            ▼                        ▼
   ┌──────────────────────────────────────────────────────────┐
   │ runtime.Program（LogicDB / match / invoke / emit / learn）│
   └──────────────────────────┬───────────────────────────────┘
                              │ 同进程调用
                              ▼
   ┌──────────────────────────────────────────────────────────┐
   │ state.Coordinator（Reserve → Commit + 幂等窗口 + 租约）    │
   └──────────────────────────┬───────────────────────────────┘
                              │ S2SPeer.Do
                              ▼
   ┌──────────────────────────────────────────────────────────┐
   │ cluster.Node：S2S 握手 → HKDF-SHA256 → AES-256-GCM 帧     │
   │ Op="state.*" → state.Replica.Handle（禁止明文控制面）      │
   └──────────────────────────────────────────────────────────┘
```

新增/改动的包：

| 包 | 文件 | 职责 |
|---|---|---|
| `internal/event` | `event.go` | 异步 Signal 事件循环、actor mailbox、背压、类型闸门、trace |
| `internal/state` | `window.go` `store.go` `coordinator.go` `replica.go` `peer.go` | 乐观 CAS、租约预留、幂等窗口、跨节点两阶段预留提交、S2S Peer |
| `internal/place` | `place.go` `remote.go` | Place Adapter 抽象、where 编译、标签/指标匹配、K8s NodeSource 扩展口 |
| `internal/runtime` | `similarity.go` `events.go` `placement.go` | 与解释器/LogicDB/Parser 的接线，Embedding 探针 |
| `internal/cluster` | `protocol.go` `node.go` | 在既有 S2S 帧上增加控制面 `Op/Payload/Blob`，不新增明文通道 |

---

## 1. Event Loop 与异步 Signal 传播

### 1.1 模型：actor + 有界 mailbox + handler 后置下刷

每个 neuron 是一个 actor：**单 goroutine 串行消费 + 固定容量 mailbox**。
`emit` 在 handler 内**只追加本地 outbox，永不阻塞**；handler 返回后 actor 才把 outbox 扇出到下游 mailbox，背压只发生在这个下刷阶段。

这是为了同时满足两个约束：

1. 下游不能被无限事件压垮 → mailbox 有界 + 可配置背压；
2. 同步阻塞式 emit 在 feedback loop（A→B→A）里会死锁 → handler 执行期间不占用自身 mailbox 槽位，返回后再阻塞，环上至少有一个空位可以推进；再加超时兜底。

```go
type Signal struct {
    TraceID string   // 全程透传
    Seq     uint64   // 全局逻辑序号，仅用于 trace 排序
    Hops    uint16   // 跨神经元跳数，用于斩断反馈环
    From    Endpoint // Neuron.port
    Port    string   // 目标输入端口
    Type    string   // 静态类型标签（Signal<T> 的内层 T）
    Datum   Datum
}

type Handler func(ctx context.Context, s Signal, tx *Tx) error

type Tx struct { // handler 写侧：只追加，不阻塞
    loop    *Loop
    from    string
    hops    uint16
    traceID string
    emits   []Emit
}
```

### 1.2 背压三策略

`Config.Policy`：

| 策略 | 行为 | 适用 |
|---|---|---|
| `Block`（默认） | 下刷时等待 `EmitTimeout`（默认 50ms），超时丢弃并计数 | 强语义信号、AI Agent 调度 |
| `DropNewest` | 满即拒绝新事件 | 遥测、心跳、可丢状态 |
| `DropOldest` | 挤掉队首最旧事件 | sensor 最新值优先 |

`Loop.Stats()` 暴露每个 neuron 的 `Enqueued/Handled/Failed/Dropped/Blocked/Unrouted/Depth/MaxDepth/Capacity`，容量约束可被测试直接断言。

```go
func (l *Loop) push(dst Endpoint, s Signal) error {
    a := l.actor(dst.Neuron)
    switch l.cfg.Policy {
    case DropNewest:
        select {
        case a.in <- s:
            l.pending.Add(1)
            return nil
        default:
            a.dropped.Add(1)
            return ErrBackpressure
        }
    default: // Block
        timer := time.NewTimer(l.cfg.EmitTimeout)
        defer timer.Stop()
        select {
        case a.in <- s:
            l.pending.Add(1)
            return nil
        case <-timer.C:
            a.blocked.Add(1)
            a.dropped.Add(1)
            return ErrBackpressure
        case <-l.done:
            return ErrClosed
        }
    }
}
```

### 1.3 强类型：两道闸

- **编译期**：checker 规则 28/29 已保证 synapse 两端端口存在、方向正确、`Signal<T>` 类型兼容；
- **运行期**：`Loop.Connect` 再次校验端口方向与 `Type` 相等；`Tx.Emit` 与外部 `Emit` 校验 `Datum.TypeName() == Port.Type`。远端注入、手工拼帧、emit 目标未被 checker 覆盖的情况都会在入队前被拒绝，计数在 `Loop.Rejected()`。

runtime 侧用 `signalValue` 保留静态类型，避免 `Int32/Int64` 在运行时被抹平成 `Int`：

```go
type signalValue struct {
    Value Value
    Type  string // 编译期 Signal<T> 的 T
}
func (s signalValue) TypeName() string { return s.Type }
func (s signalValue) Canon() string    { return s.Value.Canon() }
```

### 1.4 emit 语法接线

parser 现有 AST 没有「emit 目标端口」字段，因此运行时支持两种写法，且都在类型校验后入队：

```neura
emit out(value)   // 显式目标端口：parser 解析为 CallExpr，runtime 在求值前截获
emit value        // neuron 只有一个输出端口时，发往该端口
```

`emit o(v)` 不会被误判为未知函数调用；多输出端口且目标不明确时打 warning，不静默丢事件。

### 1.5 从 AST 构建与投递

```go
loop, err := prog.BuildEventLoop(event.Config{
    Capacity:    256,
    Policy:      event.Block,
    EmitTimeout: 20 * time.Millisecond,
    MaxHops:     32,
    TraceLimit:  4096,
})
if err != nil { return err }
loop.Start()
defer loop.Close()

// 外部入口：类型由目标输入端口静态决定
err = prog.Inject(ctx, loop, "RiskControl", "tx", runtime.FloatV(12000), traceID)

// 等待 in-flight（含 outbox 下刷）清空
err = loop.WaitIdle(ctx)
```

### 1.6 保证与边界

**保证**

- 每个 neuron 内 FIFO、串行执行，无 neuron 内数据竞争；
- mailbox 深度 ≤ `Capacity`；`Block` 下生产者等待有上限；
- `TraceID` 透传，`Seq` 可还原事件先后（跨 neuron 的调度交错不保证确定）；
- `MaxHops` 斩断反馈环，不会无限自激。

**不保证**

- 跨 neuron 的全局执行顺序（actor 并发本身不确定）；trace 的 `Seq` 能还原逻辑先后。需要严格回放时，生产端串行注入并在每条事件后 `WaitIdle`；
- `Drop*` 策略下事件不重放（这是背压而非可靠队列；可靠投递由上层 trace/幂等负责）。

---

## 2. Shared-State 与 Cross-node Exactly-once

### 2.1 目标定义（先把话说清楚）

放弃 Paxos/Raft，提供：

> **每键状态效果 exactly-once + 跨节点最终一致**。

- 网络投递：at-least-once（RPC 超时可能重发）；
- 状态应用：同一 `TxID` 只应用一次（节点本地幂等窗口）；
- 多键事务：可检测冲突时 **全有或全无**；提交阶段断链时进入 `Indeterminate`，用同一 `TxID` 重试收敛；
- **不承诺**分区期间的多键线性一致原子性。需要线性一致多键事务时，另接 Raft，不在本层假装。

### 2.2 三个不变量

1. **单写者**：`Router(key) → owner node`。一个键的写只允许落到 owner，避免多主冲突；
2. **稳定幂等键**：`TxID = traceID#seq`。跨重试、跨节点、跨进程重启（窗口保留期内）都不变；
3. **版本 CAS**：`Write.Expect` 是读到的版本；版本不符立即冲突，不覆盖。

### 2.3 两阶段预留提交（不是 2PC 共识，是乐观 2PC）

```text
Coordinator.Commit(tx)
  ├─ Phase 1 Reserve(所有 owner)
  │    Store.Reserve(txID, writes, ttl):
  │      整体校验 Expect / 租约 → 全部通过才写 pending（节点内原子）
  │      失败 → Abort 所有已预留节点
  │
  └─ Phase 2 Commit(所有 owner)
       Store.Commit(txID, keys):
          校验 pending.by == txID → 版本 +1 → 落盘 → 写 txID 幂等窗口
          同 txID 重入 → 直接返回缓存结果（版本不再 +1）
```

- 预留租约 `TTL`：协调器崩溃后，未提交的 pending 自动过期，不会永久锁键；
- `Commit` 在同一把 Store 互斥锁内「先校验全部 key、再统一应用」，节点内不会部分提交；
- 提交阶段远端失败：返回 `ErrIndeterminate` + 未确认节点列表。**用同一 `TxID` 重试**时，已提交节点命中幂等窗口、未提交节点继续 commit，最终收敛；
- 反熵：`Store.Merge` 按 `(Version, TxID)` 做确定性 LWW；`Coordinator.Reconcile` 从 owner 拉取并修复本地。

### 2.4 核心结构

```go
type Write struct {
    Key    string
    Value  []byte
    Expect Version // 0 表示必须不存在
    Delete bool
}

type Record struct {
    Key     string
    Version Version
    Value   []byte
    Deleted bool
    TraceID string
    TxID    string
}

type Store struct {
    mu      sync.Mutex
    kv      map[string]*entry   // entry: rec + pending + by + until
    applied *Window             // txID → *TxnResult，TTL + 容量双限
}

type Coordinator struct {
    Self     string
    Store    *Store
    Replica  *Replica
    Peer     Peer               // 生产实现：S2SPeer
    Route    Router             // key → owner
    TTL      time.Duration
    Attempts int
    applied  *Window            // 协调器侧 singleflight + 结果缓存
}
```

`Window` 是进程内 TTL + 容量双限表，支持 singleflight：同 key 并发 `Do` 只执行一次，失败不缓存（允许同 key 重试），成功结果按 TTL/容量淘汰。

### 2.5 冲突重试（推荐用法）

```go
res, err := coord.CommitFn(ctx, traceID, func(ctx context.Context, attempt int) (*state.Txn, error) {
    // 每次重建都必须重新读版本；不要复用旧 Expect
    rec, _ := store.Get("account:42")
    return &state.Txn{
        ID:      fmt.Sprintf("%s#%d", traceID, attempt+1), // 稳定且唯一
        TraceID: traceID,
        Writes: []state.Write{{
            Key:    "account:42",
            Expect: rec.Version,
            Value:  mustJSON(newBalance),
        }},
    }, nil
})
```

`CommitFn` 只对 `ErrConflict` 重试（指数退避）；`ErrIndeterminate` 直接返回，由上层用**原 TxID** 重试，或交给修复任务。

### 2.6 网络：只允许 S2S

`cluster.Request` 增加控制面字段（向后兼容）：

```go
type Request struct {
    // ... 原有业务字段
    Op      string `json:"op,omitempty"`      // "state.reserve" / "state.commit" ...
    Payload []byte `json:"payload,omitempty"`
}

type Response struct {
    // ...
    Blob []byte `json:"blob,omitempty"`
}
```

- `Node.SetOpsHandler(rep.Handle)` 安装副本；
- `executeOps` 在 **未启用 S2S 时直接拒绝**，控制面不存在明文降级；
- 客户端 `S2SPeer` 无 `RootKey` 返回 `ErrPlaintext`，走 `cluster.CallSecure`：预共享 RootKey 握手 → HKDF-SHA256 会话密钥 → AES-256-GCM 业务帧（复用 v0.3.7 实现，含时间戳/防重放）。

```go
peer := state.NewS2SPeer(selfAddr, rootKey, 2*time.Second)
coord := state.NewCoordinator(selfAddr, localStore, peer, state.MapRouter(selfAddr, map[string]string{
    "account:42": "edge-b:9443",
}))
```

### 2.7 边界与运维

| 场景 | 结果 |
|---|---|
| 同 TxID 重复 RPC / 超时重试 | 幂等，版本只 +1 |
| Reserve 冲突 | 立即返回 `ErrConflict`，已预留节点全部 Abort |
| Commit 阶段断链 | 返回 `ErrIndeterminate`；同 TxID 重试收敛 |
| 协调器崩溃 | 未提交租约 TTL 到期自动回收 |
| 节点永久失联 | 需要上层把 owner 迁走并做反熵（`Reconcile` / `Merge`） |
| 多键跨分区强原子 | **不支持**，这是刻意取舍 |

---

## 3. Place 调度 Adapter

### 3.1 抽象

```go
type Adapter interface {
    Name() string
    Nodes(ctx context.Context) ([]NodeInfo, error)
    Match(ctx context.Context, req Request) ([]string, error) // 返回 failover 顺序
}

type Selector interface { // where 的运行时形态
    Match(n NodeInfo, vars map[string]any, now float64) (bool, error)
}
```

默认实现 `LabelAdapter`：节点 `Labels`（字符串）+ `Metrics`（数值）+ `Ready` + 确定性排序。
`Request` 携带 `At`（显式节点集合，顺序即偏好）、`Vars`（实例字段）、`Balance`、`HashKey`、`Seq`。

### 3.2 where 编译器

`place.Compile(ast.Expr)` 只接受 checker 规则 35 已放行的纯表达式：

- 比较：`== != < <= > >= ~=`（`~=` 为字符串包含）
- 布尔：`&& || !`（短路）
- 算术：`+ - * /`（数字）
- 字面量：int / float / string / bool
- 标识符：实例字段（`Vars`）> 节点指标 > 节点标签 > `now`
- `node.<attr>`、`has_label("x")`、`has_metric("x")`

编译结果带 `Deps()` 与 `Unknown(nodes, vars)`，能在调度前定位拼写错误，而不是静默不匹配。

### 3.3 调度流水线

```text
全部节点
  → Ready
  → Labels 精确匹配
  → At 白名单（非空）
  → where 求值（纯函数）
  → balance:
       ""               名字升序（确定性）
       round_robin      按 Request.Seq 轮转（不用 wall clock，可复现）
       consistent_hash  rendezvous（最高随机权重）排序
  → []string（failover 顺序）
```

`consistent_hash` 用 rendezvous hashing 而不是虚拟环：无环结构、节点增减只重排冲突键、实现更短。测试覆盖了 200 个键的确定性、最小迁移率与同键复现。

### 3.4 与 runtime 接线

```go
adapter := place.NewLabelAdapter(
    place.NodeInfo{Name: "edge-a", Ready: true,
        Labels:  map[string]string{"zone": "edge"},
        Metrics: map[string]float64{"gpu_mem": 8}},
    place.NodeInfo{Name: "edge-b", Ready: true,
        Metrics: map[string]float64{"gpu_mem": 16}},
)

prog := runtime.NewProgramWithPlacement(file, adapter)

// 节点上下线后重跑所有 place 约束
_ = prog.Reschedule(ctx)

for _, p := range prog.SortedPlacements() {
    fmt.Println(runtime.FormatPlacement(p))
}
```

规则：

- `place ... at local` 永远本地，不经过 Adapter；
- `where` 可引用实例字段（通过 `Vars`）；目标为 logic 时没有具体实例，字段约束需改建为节点指标；
- Adapter 无匹配 → **回退静态 At 表**并打 warning，不阻断启动；
- `consistent_hash(field)` 的 `HashKey` 取实例字段规范值；目标为 logic 时返回空，让 Adapter 报错后回退。

### 3.5 K8s 扩展口（不引入 client-go）

```go
type NodeSource interface {
    List(ctx context.Context) ([]NodeInfo, error)
    Watch(ctx context.Context) (<-chan Event, error)
}

type RemoteAdapter struct {
    Source NodeSource
    Base   *LabelAdapter
}
```

未来接 K8s：用 SharedInformer 实现 `NodeSource`（List lister、Watch AddEventHandler），`RemoteAdapter.Run(ctx)` 增量维护节点快照，`Match` 不再每次打 apiserver。place 包与主二进制都不依赖 K8s SDK；需要时把 `NodeSource` 实现放到独立包/独立二进制即可。

---

## 4. LogicDB 相似度扩展

### 4.1 接口

```go
type SimilarityProvider interface {
    Name() string
    Score(a, b map[string]Value) float64 // 纯函数、[0,1]
}
```

`match` 主路径默认走 `JaccardProvider`（保持 v0.3.7 行为），非默认实现会在 `MatchTrace.Candidate.Provider` 打标，便于回放。

### 4.2 兜底实现

- **Jaccard**：相同字段值数 / 字段并集（现有 `Similarity`）；
- **SimHash**：字段 token 的 64 位 SimHash，`score = 1 - hamming/64`，对字段顺序不敏感，适合高基数标签/文本。

```go
prog.SetSimilarityProvider(runtime.SimHashProvider{Bits: 64})
```

### 4.3 外部 Embedding 探针

```go
type EmbeddingProbe interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}

type VectorIndex interface {
    Search(ctx context.Context, vec []float32, k int) ([]VectorHit, error)
    Upsert(ctx context.Context, id string, vec []float32) error
}

type EmbeddingProvider struct {
    Probe    EmbeddingProbe
    Fallback SimilarityProvider // 默认 Jaccard
    Timeout  time.Duration
    Index    VectorIndex        // 外部向量检索探针
}
```

- `Score`：`CanonicalText(fields)` → embedding → cosine；
- 向量服务超时/报错/空向量 → **自动回退 Jaccard**，match 不失败；
- `Search`/`Upsert`：把候选召回下推到外部向量库（HNSW/Milvus/pgvector 皆可），runtime 不绑定 SDK；
- `LogicDB.Nearest(logic, subject, provider, k)`：纯函数相似度检索，score 降序、实例名升序 tie-break，不写 trace。

```go
ep := &runtime.EmbeddingProvider{
    Probe:    myEmbeddingClient,
    Fallback: runtime.JaccardProvider{},
    Timeout:  30 * time.Millisecond,
    Index:    myVectorIndex,
}
prog.SetSimilarityProvider(ep)

vec := prog.Embed("some text")            // 探针可用时返回向量，否则保持旧 stub（nil + warning）
hits, err := ep.Search(ctx, fields, 10)   // 外部向量召回探针
```

确定性边界：外部 embedding 本身可能非确定；`Provider` 标签会记录在 trace 中，候选 tie-break 仍由实例名保证稳定。需要完全可复现时用 Jaccard/SimHash。

---

## 5. 安全与网络纪律

1. **零第三方**：新增代码只 import 标准库与项目内包；
2. **控制面 no-plaintext**：
   - `cluster.Node.executeOps` 在 `S2SEnabled()==false` 时直接拒绝 `Op` 请求；
   - `state.S2SPeer` 无 RootKey 返回 `ErrPlaintext`，不提供明文 fallback；
   - 所有状态 RPC 复用 `CallSecure`（RootKey 握手 + HKDF-SHA256 + AES-256-GCM + 防重放）；
3. **强类型不被绕过**：
   - 编译器仍是第一道类型闸；
   - 事件端口与状态写值都在运行时复核类型/结构；跨节点 payload 是显式 `[]byte`，由业务层编码，不污染 `Signal<T>` 推导；
4. **无重型共识**：状态层只有 CAS + 租约 + 幂等窗口，没有 leader 选举、日志复制、Quorum。

---

## 6. 构建与验证

```bash
cd /opt/neura

gofmt -l .            # 无输出
go vet ./...          # 无告警
go test ./... -count=1 -race
```

当前结果：

```text
ok  neura/internal/checker
ok  neura/internal/cluster
ok  neura/internal/event
ok  neura/internal/lexer
ok  neura/internal/parser
ok  neura/internal/place
ok  neura/internal/runtime
ok  neura/internal/state
```

关键测试覆盖：

| 子系统 | 测试 |
|---|---|
| Event | trace/类型透传、Block 容量不越界、DropNewest、类型拒绝、Hop 限环 |
| State | CAS、重复 Commit 不重复应用、32 并发同 TxID、冲突全回滚、Indeterminate 重试收敛、S2S 端到端、明文拒绝 |
| Place | 标签/指标/实例字段、has_label、类型错误、round_robin 复现、rendezvous 最小迁移、At 顺序、watch 增量 |
| Similarity | provider trace 打标、SimHash、embedding 失败回退、VectorIndex 探针、Nearest、CanonicalText |
| Runtime | BuildEventLoop、synapse 类型不匹配、Place Adapter 接线、静态回退、SharedState 开关 |

---

## 7. API 速查

```go
// Event
loop, err := prog.BuildEventLoop(event.Config{Capacity: 256, Policy: event.Block})
loop.Start(); defer loop.Close()
err = prog.Inject(ctx, loop, "Neuron", "inputPort", value, traceID)
err = loop.WaitIdle(ctx)
stats := loop.Stats()

// State
store := state.NewStore(10_000, 10*time.Minute)
replica := state.NewReplica(store)
replica.Install(node) // node 必须已 EnableS2S
peer := state.NewS2SPeer(self, rootKey, 2*time.Second)
coord := state.NewCoordinator(self, store, peer, router)
res, err := coord.CommitFn(ctx, traceID, buildTxn)

// Place
a := place.NewLabelAdapter(nodes...)
prog := runtime.NewProgramWithPlacement(file, a)
_ = prog.Reschedule(ctx)

// Similarity
prog.SetSimilarityProvider(runtime.SimHashProvider{})
prog.SetSimilarityProvider(&runtime.EmbeddingProvider{Probe: p, Index: idx})
```

---

## 8. 非目标与下一步

**本版不做**

- 多键跨分区强原子（需要共识；当前明确标注 `Indeterminate`）；
- 事件持久化 / 重放日志（当前是内存 actor + 背压，不是 Kafka）；
- 自动 shard 迁移与 owner 选举；
- K8s SDK 本体（只留 `NodeSource` 接口）。

**下一步（按收益排序）**

1. 把 `TraceID#seq` 接到 runtime 的 `nextReqID`，让 capability RPC 与状态事务共用同一幂等域；
2. Event Loop 增加 per-neuron 指标导出与死信队列（可选，不影响核心路径）；
3. `NodeSource` 的 K8s 实现独立成 `cmd/neura-k8s-adapter`，通过 `RemoteAdapter` 回注；
4. 状态窗口可插拔持久化（WAL / 嵌入式 KV），保持接口不变。
