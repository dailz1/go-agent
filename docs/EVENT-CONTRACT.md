# 事件交付契约（EVENT-CONTRACT）

**状态**：冻结契约 v1（2026-11-05）
**来源**：issue #4「事件语义未正式化」；DESIGN.md 是 contract of record，本页是事件流交付语义的完整表，冲突时以 DESIGN.md 指向的版本为准。
**非目标**：不引入钩子/扩展系统、不改变七种 sealed 事件、不给事件加确认/回压机制。

---

## 1. 交付模型总纲

- **单消费者、同步投递**。事件流是 `iter.Seq2[AgentEvent, error]`：内核逐个 `yield` 给唯一消费者，`yield` 返回 `false`（消费者 break）即停止投递且 run 不再产生任何事件。事件是单播的；多消费方（可观测性 + 对账等）各自 tee 是调用方的职责，内核不做 fan-out。
- **惰性启动**。`RunStream`（含 `WithThread`）返回的迭代器体在首次 range 才执行：ownership、回放、输入提交都发生在第一次迭代。从不 range 的调用无可观察副作用。
- **错误是终态**。`yield(nil, err)` 至多出现一次，其后不再有事件。错误来自 provider（流前失败重试耗尽、流内 SSE 错误）、持久化写入、ctx 取消等系统级失败；工具级错误是软 `ToolResult`，不走错误通道。
- **durable-before-visible**。一条事件只在其对应状态已持久化（配置 Store 时）之后才投递：轮次先 `round_declared` 再宣告 TC；结果先 `round_committed` 再投 TR；压缩先 checkpoint 再投 CompactionEvent；终态先落库再投 Done。消费者看到的每个事件都意味着它描述的状态已能从日志回放。
- **`Run` 是归并**。`Run` 完整消费流并把 DoneEvent 折叠为 `RunResult`，事件不外露；`Run` 消费者不存在"错过 Done"问题。未知事件类型中止 fold（类型化错误）；流结束而无 Done 是错误（`agent stream ended without a done event`）。

## 2. 轮内全序

一个 round 内消费者可见事件的顺序是全序：

```
RetryEvent*                    （仅当本轮模型调用发生流前重试；每次尝试前一条）
CompactionEvent?               （仅当本轮顶部压缩实际生效；至多一条）
ThinkingDeltaEvent* / TextDeltaEvent*   （按 provider 流的原始交错顺序）
ToolCallEvent{N}               （连续批次，declaration order，无 ctx 检查）
ToolResultEvent{N}             （与 TC 一一配对，declaration order）
（下一轮，或）
DoneEvent                      （恒为最后一个事件）
```

跨轮可分辨性：连续的 TC 批次标记同一 assistant 回复，跨轮批之间必然隔着一轮完整的 TC/TR 或 Done。`ReasoningItemChunk` 不产生事件（read reasoning from DoneEvent.History）；未知 chunk 类型静默忽略。

## 3. 逐事件契约

### TextDeltaEvent / ThinkingDeltaEvent
- **何时发**：流式响应期间按 provider delta 原样转发；顺序 = provider 发出顺序，无重排。
- **到达保证**：尽力而为的镜像，不是持久化事实。这两类事件不对应任何落库记录；流中断后增量不补发。
- **break 行为**：在增量期 break，assistant 尚未 durable（无 `round_declared`）。持久线程停留在 `run_started`，线程可恢复（`ResumeThread` 会重新生成该轮，原增量不重放）；非持久运行就是停止。

### ToolCallEvent
- **何时发**：模型回复含工具调用时。普通可执行轮：整轮宣告持久化之后、任何调用执行之前，逐条投递；**宣告批次内没有 ctx 检查**（拆批会留下半宣告、不可重建的 assistant）。
- **到达保证**：同轮全部调用必然连续到达（ TC-first）；批次连续性 = 同一 assistant 的标记。语义是"模型请求的宣告"，不是"已执行"。
- **break 行为**：普通轮在宣告期 break → 任何工具未执行，轮已 durable，线程保持 incomplete/resumable（恢复时对未提交调用合成"结果未知"结果）。
- **maxIter 终端例外**（唯一 precommit 例外）：`i == maxIter-1` 时，全部 skip 结果与终态在**第一条终端 TC 之前**已写完（`truncateRun`）。TC/TR/Done 纯粹是事后投递；消费者在任何终端 TC 或 TR 上 break 只影响自己的投递，线程已 closed，`ResumeThread` 返回 `ErrNothingToResume`。C10 旧例外（宣告前返回）已消除：终端轮同样宣告全部 TC。

### ToolResultEvent
- **何时发**：一个调用的结果（成功、软失败、审批拒绝、未知工具、恢复的 panic、iteration-limit skip）记入 history 后投递，恰好一条。
- **到达保证**：durable-before-visible——事件到达即结果已可回放。轮内投递顺序 = 宣告顺序；**执行完成顺序绝不泄漏到事件顺序**（并行执行时后完成的调用不插队）。
- **break 行为**：同 TC——普通轮 break 保持 resumable；终端 skip 批 break 只停投递。

### RetryEvent
- **何时发**：仅流前失败（连接/鉴权/非 2xx）触发的每次重试尝试前一条。流内 SSE 错误永不重试、永不发 RetryEvent。
- **到达保证**：尽力通知；重试最终结果（每次尝试的完整明细）以 `DoneEvent.Retries` 为准，消费者可以完全不处理 RetryEvent。
- **break 行为**：在 RetryEvent 上 break 会中止整个 run（包括挂起中的重试）。

### CompactionEvent
- **何时发**：压缩链实际改变了 history 且成功后投递，每次生效至多一条；整个运行可能零次。
- **到达保证**：durable-before-visible（checkpoint 先于事件，回放绝不重跑摘要器）。压缩失败是非致命降级：Warn 日志 + 原史继续，此时**事件缺席但 run 正常**——消费者不得把"没有 CompactionEvent"当作"历史从未被压缩"。
- **break 行为**：在 CompactionEvent 上 break，压缩已 durable、run 在本轮顶部停止，线程 resumable。

### DoneEvent
- **何时发**：终态（最终文本回复、FinishRound 合作关闭、maxIter 截断）落库之后、作为流的最后一个事件，恰好一条。
- **到达保证**：**只保证给完整消费流、从未 break 的消费者**。break 之后 Done 及一切后续事件不再投递；这不是丢失，是 `iter.Seq2` 的定义。需要终态但不做全量消费的调用方应使用 `Run`（折叠版，终态经返回值交付）或持久化恢复，而不是"假设 Done 会来"。
- **History 权威性**：`DoneEvent.History` 是完整、无损的权威快照。增量事件不足以重建含 reasoning items 的 history（ReasoningItemChunk 不产生事件）；对账/审计必须以 Done 快照 reconcile，不得只靠事件折叠。Usage / TotalUsage 在 Done 中是权威值。
- **持久线程的取消**：cancelled 的 ResumeThread 快照不是执行结果，永不产生 DoneEvent；`RunResult{Cancelled:true}` 经返回值交付。

## 4. 消费者行为定义

- **消费者没有错误通道**。`yield` 不回传错误；消费者处理失败的表达方式就是 break。内核不感知也不需要感知"消费者出错"与"消费者主动停止"的区别，二者语义相同：停止投递、run 按 break 路径收尾。
- **break 不自动结算**。普通轮 early-break 不写假终态、不产生 `RunInterruptedError`；持久线程保持可恢复。需要显式放弃时走 `WithRunExitFn` 拿 SettlementToken + `SettleThread`（见 DESIGN.md P1 取消结算节）。
- **消费者收尾义务**。消费完成后迭代器自然返回；提前 break 时持久线程在释放 ownership 前完成 unwind（配置了退出回调则在 unwind 中同步交付本 session 的 SettlementToken）。从不 range 的惰性流不触碰任何资源。

## 5. 对账方（平台消费方）的最小依赖声明

依赖 DoneChunk 必达的对账逻辑，其成立条件是：**消费到 Done 才对账，否则保持估算保守不回补**——这与 issue 描述的现状一致，且与本契约相容：Done 只投递给完整消费者，"没见到 Done"合法地涵盖了"提前 break"与"运行失败"两种情况，两者都不应回补。若需在对账侧获得不依赖全量消费的终态，应改用持久化回放（`History`/回放日志）而非事件流。

## 6. 与其他文档的关系

- DESIGN.md：contract of record；本页为其事件契约的完整表。
- `agent/event.go`：逐事件的 Go doc comments 是面向类型使用者的摘要，与本页冲突时以本页 + DESIGN.md 为准并修 comments。
- 语义验证锚点：`agent_fold_test.go`、`reasoning_order_test.go`、TC-first 与 maxIter 终端批次测试、persistence stream tests（durable-before-visible 与 break/resumable 行为）。
