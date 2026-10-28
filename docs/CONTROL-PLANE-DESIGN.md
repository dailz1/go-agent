# 运行中控制面设计（steering / FinishRound / 运行时配置热更新）

> 状态：设计稿 v3（2026-10-28；v2 独立代码级评审 verdict=REQUEST_CHANGES，§10 D6–D10 已全部裁决采纳，§9 D1–D5 已按推荐裁决；§12 列三条裁决后修正案——探针前置、maxIter 边界规则、压缩先行注入。**进入实现的前置动作：§12-1 探针验证**）
> 裁决补充调研：D4 形状的业界证据（PydanticAI 修复层官方文档、pi issue #8756、llm-sdk Resumability、providerkit 崩溃恢复指南、vercel/ai PR#8524）——该形状为业界已知且被 adapter 层归一化处理的常态，详见 §10-D7 裁决记录。
> 前置：pi-mono 内核对比（docs/pi-agent-loop.md §8）、Claude Code / OpenAI Agents SDK / LangChain / PydanticAI 钩子形态调研（2026-10-28 会话核实）。
> 本文只产出设计，不含实现。只读 docs/DESIGN.md 与 agent/、store/ 现有代码，不改任何接口。

## 0. 一条设计原则

**凡是运行中来自外部的意图——插话、改配置、叫停——一律走线程日志，在回合边界的 drain 点生效。**

推论（2026-10-28 裁决修正）：准确的原则是**"接受即持久化"（accept implies persisted）而非"持久化要趁早"**——API 返回成功的时刻，消息必须已在日志里，此后任何崩溃不背叛该承诺。

由此推出三个契约共享同一组不变量：意图先持久化后生效（与"提交点先落库后行动"同构）；生效点唯一且在回合边界（不触碰 TC-first、declare/commit 两阶段、单一执行路径）；Resume 时日志重放即状态重建（无内存态依赖）。

不进本设计的：模型/工具**供应商级**切换（Provider 包装器已覆盖，DESIGN.md §4）；per-tenant 能力面隔离（per-user Agent 实例组合解决，见 §8）；`afterToolCall`/`transformContext`/`convertToLlm` 等通用钩子（与 Tool/Provider 包装器、Compactor 链重复，明确不做）。

## 1. 动机与能力差距

对照 pi 内核（`packages/agent/src/types.ts`）与主流 agent 运行时，go-agent 有两个包装器结构性补不齐、且全行业已收敛的能力缺口：

1. **Steering**：pi 用 `steeringQueue`/`followUpQueue`（内存）在回合边界注入用户消息；Claude Code 用 UserPromptSubmit/排队输入。包装器无法把 user message 注入持久化 history，也无法让 run 在工具轮之间继续。
2. **回合末合作式终止**：pi 的 `finishTurn` 返回 `{action:"continue"|"end"}`，宿主可按内容（预算、循环检测、审查）在干净边界终止"模型还想继续"的 run。go-agent 现有替代只有 ctx cancel（破坏性，终态 cancelled）与 maxIter（只数轮数）。

另有一个已确认的二等公民问题：`llm.Options` 本是调用级（含 `Model`、`Thinking`），但 `a.llmOpts` 在 `New` 时焊死（agent.go:119，透传点 provider_call.go:36/44），运行中改模型/思考强度只能靠 Provider 包装器——不持久、wrapper 拿不到 ThreadID/Round、非线程隔离。

## 2. 记录类型（store 层）

新增三个 kind（均 SchemaV2，`validateRecord` 白名单同步扩充；旧二进制读到：写入侧 `ErrUnknownKind`、重放侧 `ErrIncompatibleLog`，符合"不得借未知 kind 隐藏新终态"的既有规则）：

### 2.1 `input_queued`（schema=2）

EnqueueInput 的意图记录。payload（DTO，显式编解码，与既有信封同规）：

```go
type inputQueuedPayload struct {
    Input string `json:"input"` // 逐字节保留，不规范化（同 run_started 惯例）
}
```

- 记录 ID：`"q-" + crypto-random hex(16)`（调用方无法预知 head，幂等重试靠"同前缀+全内容比对"既有 planAppend 语义）。
- 语义：**仅登记意图，不改历史**。replay 对未消费的 `input_queued` 不产生任何消息。

### 2.2 `input_consumed`（schema=2）

runner 在 drain 点写的消费记录。payload：

```go
type inputConsumedPayload struct {
    Inputs []string `json:"inputs"` // 本次 drain 的有序队列内容
}
```

- 记录 ID：`"c-" + crypto-random hex(16)`。
- 语义：canonical history 在**当前 replay 位置**插入 `len(Inputs)` 条 `role=user` 消息（内容 = envelope 包装后的实际 outbound 文本，与内存视图同源），紧接其后的 `round_declared` 的 assistant 对其可见。
- **与宣告的时序关系（对现循环的校正）**：`round_declared` 在工具执行前写入（"durable before any call is announced"），而 drain 在下一轮顶部、`chat` 之前，故 `input_consumed` 永远落在前一轮 commit 之后、本轮宣告之前——独立的单记录 Append，**不与 `round_declared` 合批**（合批在设计上不可能，两时刻之间隔着一次完整 LLM 请求）。
- 崩溃窗口：`input_consumed` 落库后、本轮 assistant 产生前崩溃 → Resume 重放历史含已消费插话，第 N 轮重新生成（at-least-once，模型重新看到插话，语义一致）；不存在"插话已消费但无任何后续宣告"的孤立态——它就是 history 的一部分，与 RunThread 初始输入同性质。

### 2.3 `config_changed`（schema=2）

运行时配置热更新的记录。payload：

```go
type configChangedPayload struct {
    Model    *string `json:"model"`    // nil = 不变
    Thinking *string `json:"thinking"` // nil = 不变；取值域同 llm.WithThinking
}
```

- 记录 ID：`"k-" + crypto-random hex(16)`。
- 语义：自本记录的 replay 位置起，线程生效配置按字段覆盖；后续记录继续叠加（后写赢）。replay 终态即当前生效配置；无任何记录时生效配置 = Agent 构造时的 `a.llmOpts` 对应字段。
- v1 字段域刻意收窄为 Model + Thinking：二者在三个 adapter（openai/glm/openairesponses）都是 per-request wire 参数且无历史耦合；Temperature/MaxTokens 等留待有真实需求再扩（扩字段 = payload 加列，向前兼容）。

## 3. 公开 API（agent 层）

### 3.1 EnqueueInput —— durable steering

```go
func EnqueueInputWithID(ctx context.Context, s store.Store, thread, idempotencyKey, input string) (QueuedInputID, error)
```

- **幂等键由调用方提供**（D10 裁决）：idempotencyKey 派生记录 ID，崩溃后以同 key 重试，store 前缀匹配命中即幂等确认——ack 丢失场景不产生双写。内核不做隐式哈希：幂等键的业务含义（"哪次调用算同一条消息的重试"）只有调用方知道。
- 要求已配置 Store，否则 `ErrNoStore`（与 RunThread 等显式线程入口同规）。
- 追加 `input_queued`。**run 活跃期间允许入队**（D6 裁决采纳方案 (a)）：Enqueue 走**乐观重试**——bounded 次数内重读 head、同幂等键重试；与 runner 写点的乐观吸收协议（§10-D6）配合，全局唯一修订推进。超限返回 `ErrThreadBusy`。ack 丢失的边界语义见 §10-D10。
- **准入 = 硬限制（2026-10-28 评审后修订）**：input 以**序列化消息**（estimateRunes 计量，与压缩/drain 预算同单位，转义无法绕过）对照硬限制——有 RoundContextProvider 时为 canonicalContextCap、否则为 contextWindowTokens——超限即 `ErrInputTooLarge` 拒绝。准入只拒绝"物理装不进"的输入；**装得下但在 drain 时刻历史已贴近硬限制的输入会被延后**（留在队列，下一轮压缩腾出空间后注入）——已接受的输入可能迟到，但绝不会把 run 顶过硬限制。
- 线程无 active run 时同样允许入队（成为下一 run 首轮的 follow-up）；消费时机见 §3.4。
- input 空 = `ErrEmptyInput`（新增哨兵，fail-fast）。

### 3.2 FinishRound —— 回合末决策钩子

```go
type FinishRoundDecision struct{ Action FinishRoundAction }

type FinishRoundAction int

const (
    FinishRoundDefault  FinishRoundAction = iota // 零值 = 维持默认调度
    FinishRoundContinue                          // 保证再发一次 provider 请求
    FinishRoundEnd                               // 正常终结 run
)

type FinishRoundContext struct {
    ThreadID   string
    Round      int           // 零基逻辑轮，与 RoundContextRequest.Round 同源（agent.go 现传 i）
    Assistant  llm.Message   // 本回合完整 assistant 消息（含工具调用），深拷贝
    Results    []llm.Message // 本回合有序模型可见工具结果，深拷贝
}

func WithFinishRound(fn func(context.Context, FinishRoundContext) (FinishRoundDecision, error)) Option
```

- **调用点**：本回合 `round_committed` 落库之后、工具结果追加进内存 history 之后、下一轮 `compactHistoryRound` 之前——即循环里 `totalToolCalls +=` 之后、下一个 for 迭代开始前。此刻无在途请求、上下文静止，与 pi 的请求边界原则一致。
- **nil decision（返回零值）= 默认调度**：有工具调用则继续，无则自然收尾。
- `End`：run 以正常 Done 终结（`Truncated:false`），history 以本回合工具结果收尾；`DoneEvent.History` 照常完整。与 ctx cancel 的 cancelled 终态是两回事——这是**合作式收尾**，不产生取消记录、不需要 SettleThread。挂 maxIter terminal 分支同款"Store 先关闭终态再交付 Done"的纪律。
- `Continue`：保证再发一次 provider 请求（同 pi 的 explicitContinuation 语义）；若 drain 到 steering，请求自然携带插话，无插话则原样 context 再问一轮，**不产生空转**（下一圈自然终止规则照常起作用——continue 只保证"一次"，不保证"继续到自然停"）。
- 回调错误：契约失败关闭——错误经 `FinishRoundError{Round, Err}` 包装后走既有错误路径（session 场景套 `RunInterruptedError`，可 Resume）。**已 committed 的回合不回滚**，重放即恢复。
- 回调在本 run 内串行；同 Agent 并发 run 由宿主保证回调并发安全（与 RoundContextProvider 同条款）。
- 不在 maxIter terminal skip 分支调用（该分支零决策零执行，保持冻结）；不在 Resume 的 cancelled 快照分支调用。

### 3.3 SetThreadConfig —— 配置热更新

```go
type ConfigChange struct {
    Model    *string
    Thinking *string
}

func SetThreadConfig(ctx context.Context, s store.Store, thread string, ch ConfigChange) error
```

- 全字段 nil = `ErrEmptyConfig`；其余错误语义与 EnqueueInput 完全一致（ErrNoStore / 乐观重试 / ErrThreadBusy）。
- **生效点**：`runStreamInternal` 每轮顶部——replay 状态中最后一个 `config_changed` 的字段覆盖本回合 `a.llmOpts` 后传给 `chatWithRetryAndFallback`。每轮固定顺序：**读配置 → 压缩 → drain → 预算检查 → 请求**（2026-10-28 裁决修正：drain 移到压缩之后，见 §3.4）。
- run 开始前的 `config_changed` 同样生效（首个请求即用新配置）；run 结束后写入的，由下一个 run 的 replay 带入。两个 API 都不需要"通知在途 run"的推送通道——runner 每轮从 replay 状态读取，轮询即一致。

### 3.4 Drain 点全定义（唯一清单）

run 内有且只有两处 drain：

| 位置 | 时机 | 落库 |
|---|---|---|
| D1 | 轮顶：读配置 → `compactHistoryRound` → **drain** → 预算检查 → provider 调用。drain 只消费序列化成本能塞进"硬限制 − 当前 history"的 FIFO 前缀，装不下的项**延后**（留在队列，随下一轮压缩/排水重试）——已接受的输入可能迟到但绝不把 run 顶过硬限制，预算护栏保持原始形态 | 独立单记录 Append（见 §2.2 时序校正） |
| D2 | run 结束前（自然 Done 或 FinishRound End 的终态确认之后）：队列非空则**保留**，不消费、不落 input_consumed | 无 |

未消费的 `input_queued` 由**下一个 run 首轮 D1** 消费（推荐语义，§9-D3）。这使 EnqueueInput 在无 active run 时天然成为 follow-up：RunThread 的新输入先入 history，D1 再把积压插话补进首轮请求之前。

## 4. 循环内变更点（runStreamInternal 精确标注）

以 agent.go 当前行为为基准，新增逻辑共四处、全部在回合边界：

1. 轮顶（现 `compactHistoryRound` 之前）：读 replay 配置 → 计算 effectiveOpts；
2. D1 drain（压缩后、预算检查前）：range 队列 → 若非空，逐条追加 user 消息进 history，`sess` 场景以独立单记录 Append 落 `input_consumed`（时序校正见 §2.2）；无 store 场景不支持（§9-D1）；
3. `FinishRound` 调用（本回合工具结果 commit 落库并追加进内存 history 之后、下一轮迭代开始之前——注意 `round_declared` 在执行前已写，故决策点在 commit 之后而非 declare 附近）；
4. 终态分支（自然 Done / maxIter / FinishRound End 三路汇合到同一 finishRun 纪律：Store 先关闭终态再交付 Done）。

**不变量自查**（逐条对照 DESIGN.md 现行承诺）：

- sealed 7-event set 不变；steering 注入不产生新事件（consumer 从 `DoneEvent.History` 看到插入的 user 消息，与 ReasoningItemChunk 的处理哲学一致）；
- TC-first 不变：drain 在宣告之前完成，宣告批次内 assistant 与插话的相对顺序由同批 Append 保证；
- declare/commit 两阶段不变：drain 严格落在前一轮 commit 之后、本轮宣告之前，且为独立单记录 Append；replay 顺序即真相，无新中间态；
- `DoneEvent.History` 权威性不变：插话消息按 replay 顺序进入 canonical history，事件流折叠不出它的范围（与 CompactionEvent 同性质）；
- maxIter terminal 分支冻结：不 drain、不调 FinishRound；
- 匿名（无 store）run：FinishRound 可用（纯内存）；EnqueueInput/SetThreadConfig 返回 ErrNoStore（§9-D1 建议维持）。

## 5. Resume/Settle 交互

- Resume 重放时，`input_queued`（未消费）→ 继续排队；`input_consumed` → 插入 user 消息；`config_changed` → 叠加配置。恢复的 run 用恢复时点的配置与队列继续，**无任何"恢复时重放历史事件"语义**（新消费者只看本次运行的事件，插话以 history 形式存在于上下文，不以事件形式重放）。
- EnqueueInput/SetThreadConfig 与 SettleThread 无交互：二者不是 run，不产生 SettlementToken；调用它们不改变原 run 的可结算性。
- `SettlementToken.ExpectedHead` 在原 session 冻结后，外部 API 的追加会使后续显式结算按既有 `ErrRevisionConflict` 失败——**这是特性不是缺陷**：插话落库这一事实应当阻断陈旧 token 的盲结算，与"迟到请求不得改变 revision"的既有裁决同构。

## 6. 错误与哨兵清单

| 哨兵 | 出处 | 语义 |
|---|---|---|
| `ErrEmptyInput` | EnqueueInput | 空 input 拒绝入队 |
| `ErrEmptyConfig` | SetThreadConfig | 全字段 nil 拒绝 |
| `ErrThreadBusy`（复用） | 两者 | bounded 重试后 head 仍冲突，不等待 |
| `ErrNoStore`（复用） | 两者 | 未配置 Store |
| `FinishRoundError` | WithFinishRound 回调错误包装 | `%w` 保留原因，session 场景外层套 RunInterruptedError |

## 7. 测试计划（stdlib，白盒在包内；复用 agenttest.ScriptedProvider）

1. **steering 生命周期**：入队→D1 消费→插话进入下一轮请求的 outbound（ScriptedProvider 断言收到的 messages）→ `input_consumed` 单记录落库（前一轮 commit 之后、本轮宣告之前）→ DoneEvent.History 含插话；consumer 以 DoneEvent.History 为权威 reconciliation（steering 不产生事件，文档明示该缺口）。
2. **崩溃窗口**：`input_consumed` 落库后、本轮 provider 响应/宣告前崩溃 → Resume 重放得到插话在历史中、本轮重新生成（at-least-once），无孤插话、无孤 assistant。
3. **Enqueue 并发**：runner 持 ownership 期间 Enqueue 的乐观重试成功；人为制造持续冲突返回 ErrThreadBusy。
4. **FinishRound End**：有工具调用的回合后 end → 正常 Done、Truncated=false、history 以工具结果收尾、无取消记录、ResumeThread 返回 ErrNothingToResume。
5. **FinishRound Continue**：无工具调用回合后 continue → 恰好多一轮请求、无空转（下一轮自然终止）。
6. **FinishRound 错误**：回调返回错误 → FinishRoundError 可 errors.As、已 commit 回合保留、Resume 可恢复。
7. **config 热更新**：SetThreadConfig 后下一轮 outbound 带新 Model/Thinking（ScriptedProvider 断言 opts）；重启后（新 Agent + 同 Store）Resume 首轮仍用新配置。
8. **drain 时序**：terminal 轮（maxIter）不 drain；FinishRound End 前的最后一轮照常 drain。
9. **旧 reader 兼容**：含新 kind 的日志在 Append 层被白名单拒（`ErrUnknownKind`）；重放层遇未知 kind 报 `ErrIncompatibleLog` 并可回退 `History(ctx, thread, fromSeq)` 全量——两层分别 fixture；另加 checkpoint 恢复用例（队列/配置投影，待 D9 裁决后细化）。
10. **-race -shuffle**：并发 Enqueue + 多线程 run + FinishRound 回调并发安全。

## 8. 明确不做（negative decisions，防止范围蠕变）

- `tools_changed` / per-thread 工具过滤：per-tenant 隔离用 per-user Agent 实例组合（共享 Store）解决，内核不引入"用户"与"线程能力覆盖"概念。
- `afterToolCall` 结果改写、`transformContext`、`convertToLlm`：与 Tool/Provider 包装器、Compactor 链职责重复，双轨制是净损失。
- steering 的 QueueMode（all / one-at-a-time）：v1 固定 drain-all（§9-D2 可选开放）。
- 中断式插话（abort-and-reprompt）：ctx cancel + settle + resume 已覆盖，选择权在交互层（同 pi 双路并存的立场）。
- follow-up 独立队列：由"run 结束后残留队列由下一 run 消费"（§3.4 D2 推荐）以零新机制覆盖。

## 9. 决策点（2026-10-28 owner 已按推荐全部裁决）

> 裁决备注：D4 增加生效前置条件（业界调研 + 探针验证，见 §12-1）；其余按推荐采纳。

| # | 问题 | 选项 | 推荐 |
|---|---|---|---|
| D1 | 无 store 的匿名 run 是否支持 steering？ | a) v1 不支持（ErrNoStore 语义延伸）；b) 内存队列支持 | **a**：steering 的价值主张就是 durable，内存版是退化形态且引入两套语义 |
| D2 | QueueMode 是否预留？ | a) v1 固定 drain-all；b) 仿 pi 双模式 | **a**：pi 的 one-at-a-time 服务长对话逐条消化场景，go-agent 无此实证需求；b 可后加（记录格式不变，纯运行时策略） |
| D3 | run 结束时残留队列的语义 | a) 下一 run 首轮消费（follow-up 免费）；b) DoneEvent 报告计数、由宿主决定 | **a** + DoneEvent 不加字段：日志里的事实自明，宿主可查；b 引入事件面变更 |
| D4 | FinishRound End 的 history 终态 | a) 以工具结果收尾（正常 Done）；b) 合成一条 assistant 收尾消息 | **a**：不伪造模型没说过的话（同"不写假 Done"原则）；wire 层各 adapter 对"最后一条是 tool 结果"的容忍度需在实现前对 adapter 各加一个用例确认 |
| D5 | FinishRoundDecision 是否允许携带 config 覆盖（合并 SetThreadConfig 场景）？ | a) 不允许，改配置走 SetThreadConfig；b) decision 带 optional override | **a**：单一职责，决策钩子只决策；配置走日志，两条路径可独立推理 |

## 10. 评审裁决项（2026-10-28 独立代码级评审，verdict=REQUEST_CHANGES；**D6–D10 已全部裁决，见各项末尾裁决记录**）

### D6【已裁决：采纳 (a)】

> **裁决（2026-10-28 owner）**：采纳方案 (a)——runner 写点重读 head、吸收白名单控制记录（input_queued/config_changed）后按真实 head 追加，全局唯一修订推进；run 活跃期间 EnqueueInput/SetThreadConfig 允许并发入队，配合幂等键（D10）。遇白名单外未知记录仍冲突失败。
> 讨论记录：曾考虑 v1 降级方案 (c)（run 期间拒绝入队），owner 否决——"运行中插话"是功能定义本身，不可砍。(b)（runner 代写）因接受与落库分离、崩溃丢承诺被否。
> 已知代价（owner 知情接受）：写点协议形式化与测试面；Store 新增窄尾读接口（公开面积扩大）；写点尾读的常数级 I/O 开销；未来控制面记录类型的白名单长期纪律。

### D6（blocker）外部写入与 runner 所有权协议

**问题**：runner 以 session 缓存的 head 追加（persist.go:161-168）；外部 Enqueue/SetThreadConfig 在 provider/工具执行期间插入记录，会使 runner 下一次 declare/commit/checkpoint/Done 追加 head 冲突。现有文本"绝不影响在途 run"不成立。

**候选**：(a) runner 每个写点前重读 head，吸收插入的控制记录后按新 head 追加（全局唯一修订推进的乐观写协议）；(b) 控制面写入由 runner 在 drain 点代写，外部 API 仅投递意图（退化为内存队列+落库交接，牺牲"意图先落库"）。

**推荐**：(a)。

### D7【已裁决：采纳 (a) 新增 `run_closed` 记录；生效前置 = §12-1 探针验证】FinishRound End 的终态编码

**问题**：重放层拒绝含工具调用的非 truncated Done（persist_replay.go:223-240）；复用现有 Done 编码重放会失败，传空 assistant 又伪造消息。

**候选**：(a) 新增版本化 `run_closed` 记录（无 assistant 消息地关闭 run）；(b) 扩展 Done 编码携带"终态无消息"标志。

**推荐**：(a)；DoneEvent.Message/RunResult.Message 定义为本回合 assistant。

### D8【已裁决：采纳 (a) 钩子前移 + declare 泛化；增补 maxIter 边界规则见 §12-2】无工具回合的 Continue 持久化

**问题**：无工具响应在 agent.go:450-487 立即 finishRun；拟定的钩子位置（executeToolRound 之后）对该分支不可达；且无工具回合若被 Continue 延续，其持久化只能以关 run 的 Done 表示，callback 失败/崩溃会丢该 assistant 轮（重放层还拒绝无工具调用的宣告，persist_replay.go:173-175）。

**候选**：(a) FinishRound 调用点前移到终态判定之前，Continue 使无工具回合也走类型化持久宣告（需放宽重放校验，加子状态）；(b) Continue 仅在工具回合后可达（能力收窄）。

**推荐**：(a)。

### D9【已裁决：采纳 codec 升 3 + 双投影；pending_inputs 存内容不存 ID】checkpoint 与版本策略

**问题**：checkpoint 快照只含既有字段（persist_util.go:17-33）；队列/配置投影不入快照则 checkpoint 之后的控制语义丢失；不升版本则旧 reader 借 checkpoint 绕过新语义。

**候选**：checkpoint codec_version 升 3，快照新增 pending 队列 ID 前缀 + 配置投影；旧版本快照按既有规则回退全量日志重放。

**推荐**：照办（与"版本标记不得让旧 reader 绕过新终态"既有裁决同构）。

### D10【已裁决：采纳 (a) 调用方提供幂等键】不确定写入的幂等语义

**问题**："重读 head、同内容重试"对 ack 丢失不成立——已写入的 ID 在新 head 重试会 ErrInvalidRecord（store.go:171-204）；API 无法承诺返回错误 = 未入队。

**候选**：(a) API 返回确定性 `QueuedInputID`，以 ID 重试命中即幂等确认；(b) 文档明示不确定结果 + 调用方去重责任。

**推荐**：(a)。

### should-fix 清单

已随 v2/v3 修正：drain 顺序定为"配置→压缩→drain→预算→请求"（v3 §12-3）；terminal 免除改为"terminal skip 分支内不 drain"（分支在 provider 响应后才选定）；maxIter 与 Continue 优先级（熔断不可被 Continue 延长）；测试计划第 1/2/9 条重写；`validateRecord`/三 kind/零基 Round/决策零值语义等锚点修正。

待实现期落实：`input_consumed` 引用被消费的 `input_queued` 记录 ID（FIFO 前缀精确界定，非文本匹配）；Thinking 字段 adapter 支持矩阵如实文档化（openairesponses 忽略 Thinking，非全适配器 wire 参数）；模型切换按轮快照 options 而非改 `a.llmOpts`，并与上下文预算交互文档化；session 退出后外部入队会阻断已授权结算（settle.go:70-75），须给宿主恢复决策路径（保留"不自动刷新旧取消 token"禁令）；config_changed 加列的 strict payload 演进（persist_dto 拒绝未知 JSON 字段）需显式版本策略。

## 12. 裁决后修正案（2026-10-28 讨论 §10 裁决时追加，全部已采纳）

### 12-1（生效前置）D4 形状的业界调研 + adapter 探针验证

**修正动机**：初稿把"history 以配对完整的 tool result 收尾"的 wire 层容忍度留作实现期测试。owner 指出应先调研业界；调研结果（PydanticAI 官方文档、pi issue #8756、llm-sdk Resumability、providerkit、vercel/ai PR#8524）表明：该形状是崩溃/取消恢复场景的业界常态，PydanticAI 明确原生支持"history 停在工具结果上"，且业界共识解法是 **adapter 层归一化**而非指望 provider 直接接受。

**裁决内容**：
1. 该形状**不配对损坏**（每个 tool_use 都有配对 result），比 PydanticAI 修复层处理的病态更良性；设计维持"以工具结果收尾"。
2. **adapter 层新增明文契约**（并入 DESIGN.md 修订）："outbound 序列以配对完整的 tool result 收尾是合法形态；adapter 负责转换为各协议的合法 wire 表示（Anthropic：tool_result 并入紧跟 tool_use 的 user turn、真实用户输入作后续 turn；Responses：维持 item 排序约束）"。vercel/ai PR#8524 为同款修复的先例。
3. **实现前置探针**（写代码前执行）：对 openai / glm / openairesponses 三 adapter 各发一组"…assistant(tool_use) → tool result → user"请求，tool result 内容覆盖三种来源（真实结果 / settle 的"结果未知" / maxIter 的"未执行"）。任何一家拒绝 → 停止实现，回裁决桌。
4. **顺带补课**：该形状在现有系统已由三条路径产生（maxIter truncateRun、settle 合成未知结果、early-break 修复），探针同时验证存量行为——若存量即被拒，先修存量 bug。

**探针执行记录（2026-10-28，adapter 层 wire 映射——已通过）**：以本地捕获服务器拦截三 adapter 的非流式请求载荷，注入目标序列（system, user, assistant(tool_use call_probe_1), tool result, user 新输入）：

- **openai（/chat/completions）**：`system → user → assistant(tool_calls) → tool → user`，tool_call_id 配对完整，5 项全 PASS；
- **glm（/chat/completions）**：同上，5 项全 PASS；
- **openairesponses（/responses）**：`message → function_call → function_call_output → message`，system 映射为顶层 `instructions`（协议规定，非缺陷），call_id 配对完整，6 项全 PASS。

**结论**：三个 adapter 的 wire 映射对该形状的输出均满足各协议规则，§12-1 第 3 条（adapter 层）**通过**。**剩余项**：provider 服务端真实接受度需携带真实凭据复验（openai/glm 需 API key；ChatGPT 订阅可经 llm/codex + codexauth 走 Responses 协议，其形状规则由 openairesponses 探针覆盖）——纳入实现期 e2e，不阻塞进实现。

### 12-2 maxIter × Continue 边界规则

FinishRound 钩子前移后（D8），在 `i == maxIter-1` 的最后一圈也会被调用。**裁决规则：熔断优先级最高——该圈的 Continue 视为默认调度（进入 terminal skip），不可延长 run**。文档如实写明 Continue 的"保证再发一次请求"受 maxIter 约束。与 OpenAI Agents SDK 的 max_turns + hooks 两层并存设计对齐；maxIter 本身保留，不被回调判定取代（护栏必须不依赖外部代码正确性）。

### 12-3 drain 顺序修正：先压缩、后注入

D1 顺序从"读配置 → drain → 压缩 → 预算 → 请求"改为"读配置 → **压缩 → drain** → 预算检查 → 请求"（见 §3.4）。理由：压缩基于上一圈结束时的稳定 history 决策；新消息注入在压缩后天然免于被压掉；配合 §3.1 长度上限，消除"用户插话触发 ErrCompactionBudgetExceeded 杀 run"的路径。owner 提出，采纳。

## 11. DESIGN.md 修订草案（裁决后并入）

> 归入 §5 路线图新增小节"运行中控制面"，§4 原则追加一句。要点：

- §4 追加：**"运行中外部意图走线程日志，回合边界 drain 点生效；接受即持久化。"**（与"观察用事件，改行为用包装"并列；同时把"不给 hook 随意改写流量的权限"限定为"不给改写**流量**的权限"——FinishRound 是决策不是改写。）
- llm 层契约追加（§12-1）：**"outbound 序列以配对完整的 tool result 收尾是合法形态；adapter 负责转换为各协议的合法 wire 表示。"**
- agent 层追加（§12-2）：**"maxIter 熔断优先级高于一切回合决策回调；terminal 圈的 Continue 视为默认调度。"**
- 新小节骨架：三个公开 API 签名；两个新 kind 及 payload；drain 点清单（§3.4 表）；FinishRound 调用点与三值语义；配置生效顺序（配置→压缩→drain→请求）；错误哨兵表；Resume 交互三条；不变量自查清单（§4 八条）；negative decisions（§8 全文）。
- 事件契约不变声明："sealed AgentEvent 仍为七种"原文照旧，追加"控制面能力不新增事件类型"。
