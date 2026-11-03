# 10 · 自主循环（agent 包）

> 内核文档集第 10 篇。本文只讲 `agent` 包的执行循环本体：单一执行路径、一轮的完整状态机、七种密封事件、maxIter 终局语义、重试边界。
> 相邻专题各有专篇（同目录编号 11–14、20）：压缩与截断策略、持久化与恢复、运行中控制面（steering / 配置热更新 / FinishRound）、provider 适配层、内核总览。本文只写到它们与循环的交界，不重复其内部契约。持久化细节另见 docs/DESIGN.md P1 节与 docs/CONTROL-PLANE-DESIGN.md。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动）；接口契约以 docs/DESIGN.md 为准，两者冲突时以 DESIGN.md 为准。

## 1. 单一执行路径：runStreamInternal

### 1.1 为什么只有一条路径

go-agent 的循环只有一个实现：`runStreamInternal`（agent/agent.go:388）。`Run`（同步拿 `*RunResult`）和 `RunStream`（拿事件流）都不是独立的执行引擎，而是这条路径的两种消费方式：

```
Run(ctx, input, opts...)            agent/agent.go:335
  └─ runSequence(ctx, input, rc)    agent/persist_api.go:63   唯一分发点
       └─ foldRunStream(seq)        agent/agent.go:343        归并事件流 → RunResult
RunStream(ctx, input, opts...)      agent/agent.go:384
  └─ runSequence(ctx, input, rc)    直接把 iter.Seq2 交给调用方
RunWithHistory / RunStreamWithHistory  agent/agent.go:864 / :879
  └─ runStreamInternal(ctx, copied, nil)   绕过 runSequence，永远非持久
```

`runSequence` 是"临时运行 vs 线程运行"的唯一分岔（agent/persist_api.go:58-80）：不带 `WithThread` 时直连 `runStreamInternal`；线程运行经 `executeSession → runStreamInternal`（agent/persist_api.go:153-172），只是在外面包了 session 装配、错误包装（`RunInterruptedError`）和退出回调（`runExitFn`），循环本体一条不变。这个"单执行路径不变式"写在 `runSequence` 的注释里（agent/persist_api.go:60-62）。

这是 P0 结构统一的直接产物。DESIGN.md 记录了决策：**"流是执行原语，同步执行是衍生物。只维护一条模型/工具执行路径，`Run` 与恢复 active run 的 `ResumeThread` 是事件流的归并"**（docs/DESIGN.md:66），路线图 P0 明确"合并 Run/RunStream 为单一执行路径（Run = 事件流归并）"（docs/DESIGN.md:85）。

被否决的替代形态是两条独立路径：一个同步循环 + 一个流式循环。它的代价是每个行为（重试、审批、截断、终局宣告）都要实现并测试两遍，且两条路径的行为漂移只是时间问题——`ResumeThread` 要复用执行逻辑时，只能选择其中一条去近似。合并后的形态里，`Run` 的正确性由"折叠器 + 与 RunStream 同一序列"直接保证，测试 `TestUnifiedRunStreamEquivalence` 与 `TestUnifiedRunStreamTerminalMaxIterEquivalence`（agent/agent_unify_test.go:14 / :167）把"两种入口产生等价结果"钉死为契约。

与 pi 的对照：pi 的 runLoop 同样是单循环，但对外通过回调 EventSink 推送（docs/pi-agent-loop.md:146-151）；go-agent 选择 `iter.Seq2` 是 Go 惯用的拉取模型，且明确"公共 API 不用 channel"（项目约定，docs/DESIGN.md:63 对包装器同样要求保持惰性与早退安全）。

### 1.2 iter.Seq2[AgentEvent, error] 契约

`RunStream` 返回 `iter.Seq2[AgentEvent, error]`，三条规则（doc 注释 agent/agent.go:373-382）：

1. **惰性**。`runStreamInternal` 只构造并返回一个迭代器闭包，循环体在调用方第一次 `range` 时才开始执行（agent/agent.go:400-402 的注释明确"nothing happens until the caller starts ranging"）。线程运行把这条契约延伸到持久层：所有权获取、日志重放、输入提交都发生在首次迭代时，"一个从未被 range 的调用没有可观察副作用"（agent/agent.go:378-381；实现于 `threadSession`，agent/persist_api.go:141-151——`openSession` 在 yield 闭包内部）。
2. **错误走第二个值并终止迭代**。provider 失败、工具硬错误、取消、预算超限都以 `yield(nil, err)` 交付，之后循环 `return`（例如流内错误 agent/agent.go:487-490）。调用方在 `range` 里拿到非 nil err 即是终点。
3. **早退安全**。`yield` 返回 `false` 表示消费者 break；循环里每一个 `yield` 的返回值都被检查（全文一致的 `if !yield(...) { return }` 惯用法），break 后不泄漏执行。`TestRunStream_CallerBreaksEarly`（agent/agent_test.go:1483）钉住最朴素的场景：取两个事件后 break，不 panic。早退的语义边界由事件交付的同步性保证（见 §3.3）：在 `ToolCallEvent` 宣告期间断开，本轮任何工具都不会开始执行。

`Run` 的折叠器 `foldRunStream`（agent/agent.go:343-371）是这套契约的第一个消费者：遇错立即返回 `(nil, err)`（:346-348）；`RetryEvent` 累积进 `RunResult.Retries`（:351-352）；`DoneEvent` 映射为 `RunResult`（:353-363）；增量事件（Text/Thinking/ToolCall/ToolResult/Compaction）被忽略——同步消费者只拿终态（:364-365）；出现未知事件类型或流无 Done 结束都是错误（:366-370）。

### 1.3 输入侧的两个补充入口

`RunWithHistory` / `RunStreamWithHistory`（agent/agent.go:864 / :879）接受调用方自带的历史，深拷贝后追加 user 输入，直接进 `runStreamInternal`，`sess=nil`。它们**永远非持久**：即使 Agent 配置了 Store 也不生成线程 ID（DESIGN.md:102；钉死于 `TestMaxIterStoreConfiguredHistoryEntrypointsStayNonPersistent`，agent/max_iter_continuation_test.go:49）。这是刻意的单向门：带历史续跑是一次性操作，持久续跑必须走 `WithThread` / `ResumeThread`，让生命周期记录链保持完整。

## 2. 一轮状态机全图

### 2.1 轮顶固定顺序

每一轮的顶部有固定顺序，控制面的三个机制全部挂在精确的槽位上：

```
读配置 → 压缩 → drain（steering）→ 预算检查 → 请求
```

顺序本身是冻结契约（docs/DESIGN.md:232）：drain 必须在压缩**之后**，插话才能搭上"这一轮"的请求而不破坏压缩预算；配置必须最先读，本轮的 provider/model 与摘要压缩都跟随它（agent/agent.go:415 的注释"Per-round fixed order"）。

### 2.2 状态图

```
                ┌────────────────────────── run 开始 ──────────────────────────┐
                │ 临时运行: history=[system?, user]        (persist_api.go:84)  │
                │ 线程运行: openSession→replay→run_started (persist_api.go:141) │
                │           所有权/重放/输入提交都在首次迭代时发生（惰性）        │
                └──────────────────────────────┬───────────────────────────────┘
                                               ▼
 ╔═════════════════ 迭代 i（0 ≤ i < maxIter, for 循环 agent.go:403）═══════════╗
 ║                                                                            ║
 ║ [0] ctx 取消检查 (agent.go:405) ─────────────────────────► E0 终止: ctx.Err ║
 ║ [1] 动态工具刷新（可选 WithDynamicTools, agent.go:411; DESIGN.md P2-4）     ║
 ║ [2] 读线程配置 refreshConfig + provider 解析 (agent.go:418-439)             ║
 ║      └ 折叠配置指向未注入的 provider ──► E1 终止: ErrUnknownProvider        ║
 ║ [3] 压缩检查 compactHistoryRound (agent.go:440; 实现 :757)                  ║
 ║      └ 超阈值 → Compactor.Compact → checkpoint → CompactionEvent           ║
 ║ [4] steering drain：按剩余预算注入 user 消息 (agent.go:444-456)            ║
 ║ [5] canonical 预算检查 (agent.go:457-460) ──► E2 终止: 预算超限             ║
 ║ [6] chatWithRetryAndFallback (agent.go:466; 实现 provider_call.go:13)      ║
 ║      ├ ChatStream 同步失败 → 分类 → RetryEvent + 退避 → 重试               ║
 ║      │    └ 全部尝试耗尽 ──► E3 终止: "after N attempt(s)"                 ║
 ║      └ ErrStreamingNotSupported → 非流式 Chat → synthesizeChunks 回放      ║
 ║ [7] 流式累积循环 (agent.go:486-518)                                         ║
 ║      ├ ThinkingDeltaEvent / TextDeltaEvent 即时向外 yield                  ║
 ║      ├ ToolCallStart/ArgsChunk、ReasoningItemChunk 只进 accumulator        ║
 ║      └ chunkErr（流内错误）──► E4 终止: 原样上抛，不重试                    ║
 ║ [8] assembleAssistantMessage (agent.go:522; 实现 :836)                     ║
 ║      └ 流损坏（重复/孤儿/缺 id）──► E5 终止: error                          ║
 ║ [9] history += assistantMsg (agent.go:526)                                  ║
 ╚════════════════════════════════════┬═══════════════════════════════════════╝
                                      ▼
                          终止判定 (agent.go:527-656)
          ┌─────────────────────────────┼──────────────────────────────┐
          ▼                             ▼                              ▼
  无工具调用 (:535)            最后一轮且有工具 (:604)          普通工具轮
  ├ FinishRound 钩子            ├ 无审批、无执行（C10）         ├ declareRound 先落库 (:661)
  │  （仅 !isLast, :536）       ├ Store: truncateRun 持久预闭合  ├ TC 宣告批 ×N (:673)
  │  ├ Continue →               │  非 Store: iterationLimitResults├ executeToolRound (:678)
  │  │   terminal_text 宣告后    ├ history += 跳过的软结果        │  （每调用: Get→审批→Execute
  │  │   continue 下一轮         ├ TC 宣告 ×N → skipped TR ×N    │   →结果截断; 并行模式整批
  │  └ End → closeRun + Done    └ Done(Truncated=true) (:645)    │   审批后并行执行）
  └ 默认 → finishRun/closeRun                                    ├ settlement 按声明序物化
      → DoneEvent (:590)                                          │  + TR 事件（serial 每完成
                                                                  │  一个即物化, tool_execution.go:99）
                                                                  └ 全部 settle → commitRound
                                                                    (tool_execution.go:136-145)
                                                                      │
                                              FinishRound 钩子 (:683) ──┤
                                              End → 合作收尾 Done      └ accum.reset → 下一轮 (:723)
```

### 2.3 逐点说明

**审批的位置**在工具执行内部，而不是事件分发层：串行模式下每个调用走 `executeTool` 的 `Get → RequiresApproval? → approvalFn → Execute` 序列（agent/agent.go:934-957）；审批拒绝和无回调都是回灌 LLM 的软结果，不是 Go error（agent/agent.go:942-953；fail-closed 契约：`RequiresApproval` 且无 callback 直接拒绝，绝不执行）。并行模式（`WithToolConcurrency(n>1)`）把审批提前为"任何 dispatch 前的整批审批"（docs/DESIGN.md:398），但槽位结算、有序物化的路径与串行共享（agent/tool_execution.go:75-82 的注释：`executeToolRound` 把默认惰性串行与 opt-in planner 分开、共享有序结算）。审批细节属于工具专篇，这里只需要它在状态机里的位置：**宣告（TC）之后、执行（TR）之前**。

**重试的位置**被严格限制在 [6]：只有 `provider.ChatStream` 同步返回的 pre-stream 错误进入重试循环（见 §5）。流一旦开始（进入 [7]），错误就是终态。

**压缩触发点**在 [3]：`beforeRunes > targetRunes` 才调用 `Compactor.Compact`（agent/agent.go:771-773），成功且校验通过（system 前缀不变，agent/agent.go:88-101）才采纳；压缩视图先 checkpoint 落库再发 `CompactionEvent`（agent/agent.go:785-807，注释明确"压缩视图先落库再宣告，replay 永不重跑（非确定的）summarizer"）。压缩失败不终止运行：策略错误与整体错误都记 Warn 继续用原历史（agent/agent.go:774-781），只有压缩后仍超预算才以 `ErrCompactionBudgetExceeded` 终止（agent/agent.go:823-826；steering 注入带来的 carry 容差见 :816-822）。策略链本身（折叠最老工具组 → 滑窗，可选 AI 摘要）见 `NewStandardCompactor`（agent/compact.go:604-610），细节属压缩专篇。

**终态出口共三类 Done**：正常完成（无工具调用，agent/agent.go:590）、FinishRound-End 合作收尾（agent/agent.go:710，Truncated=false，写 `run_closed` 而非 `run_cancelled`）、maxIter 截断（agent/agent.go:645，Truncated=true）。取消快照读取（`ResumeThread` 撞上已结算的 cancelled run）**不产生 DoneEvent**，只返回 `RunResult{Cancelled: true}`（agent/persist_api.go:123-129；DESIGN.md:125-128）。所有错误出口（E0-E5）交付 `(nil, err)` 后立即返回，不补假 Done。

**与 pi 的结构对照**：pi 是双层 while、无上限，把"本来要停了但还有善后"交给外层循环（docs/pi-agent-loop.md:33-56）；go-agent 是单层 `for i := 0; i < maxIter; i++` + 熔断（agent/agent.go:403）。分歧源于运行形态：pi 服务长驻会话（插话驱动、可无限延续），失控防护交给上层 session；go-agent 是嵌入式批处理内核，"任务失控要刹车、花费不能失控"是第一设计目标（docs/DESIGN.md:23），所以 maxIter、重试、持久化都内置（docs/pi-agent-loop.md:188 的结论："maxIter/重试/持久化内置——是嵌入式定位的优势，非缺失"）。

## 3. 事件模型：七种密封事件

### 3.1 清单与发射点

`AgentEvent` 是密封接口：未导出的 `eventType() string` 方法只有本包七种类型能实现（agent/event.go:15-17；编译期断言 agent/event_test.go:15-21），消费者用 type switch 处理（agent/event.go:14）。七种与业界分类对齐（docs/DESIGN.md:54，调研结论 docs/DESIGN.md:489）：

| 事件 | 定义 | 发射点 | 语义 |
|---|---|---|---|
| `TextDeltaEvent` | agent/event.go:21 | 流式循环，每收到 `TextDeltaChunk`（agent/agent.go:497-501） | 增量文本 |
| `ThinkingDeltaEvent` | agent/event.go:30 | 每收到 `ReasoningDeltaChunk`（agent/agent.go:492-496） | 增量推理文本（reasoning summary delta） |
| `ToolCallEvent` | agent/event.go:46 | 整轮流装配完成后、任何执行前，按声明顺序连续发（agent/agent.go:673-677） | 模型**请求**的宣告，不是"已执行" |
| `ToolResultEvent` | agent/event.go:60 | 结果已入 history 后（settlement 物化时，agent/tool_execution.go:119-123） | 覆盖成功、软失败、审批拒绝、未知工具 |
| `RetryEvent` | agent/event.go:89 | 每次重试尝试前（agent/provider_call.go:85） | **仅 pre-stream 连接错误**；流内错误不重试（agent/event.go:88 注释明示） |
| `DoneEvent` | agent/event.go:98 | 三类终态（§2.3） | 字段镜像 `RunResult`；`History` 是权威快照 |
| `CompactionEvent` | agent/event.go:126 | 压缩采纳后、checkpoint 落库后（agent/agent.go:799-807） | 携带压缩后历史的防御性快照 |

值得注意的是**不在**这张表里的东西：流式 chunk 层的 `ToolCallStartChunk` / `ToolCallArgsChunk` / `ReasoningItemChunk` / `DoneChunk` 都不产生事件——它们只进 accumulator（见 §3.3、§3.4）。事件层的粒度是"消费者可行动"，chunk 层的粒度是"协议事实"，两层刻意不对齐。

### 3.2 为什么 TC-first（宣告先于执行）

TC-first 的准确含义：**一轮的全部 `ToolCallEvent` 连续宣布之后，任何一个工具才开始执行；TR 只在结果确定后跟上**（docs/DESIGN.md:408"TC-first 与 materialization：普通轮的全部 TC 连续宣布后才执行"）。注意它不是说"TC 事件先于文本 delta 到达"——流式期间 Text/Thinking delta 实时先到，TC 批是在整流装配完成后才发出的（agent/agent.go:673 在 `assembleAssistantMessage` 之后）。

这个顺序是 2026-09-10 的裁决："先宣告后执行"（docs/DESIGN.md:505）。它买到三样东西：

1. **事件流可重建 history**。连续的 TC 批标记"这些调用属于同一条 assistant 回复"，消费者据此能在事件流里分辨轮次边界（agent/agent.go:667-672 的注释；折叠规则见 agent/agent_fold_test.go:21-29 的 eventFold 契约：连续 announced calls 识别一条 assistant 回复）。
2. **取消/断开时已宣告的回复完整可重建**。循环在执行前就把 assistant 消息追加进 history（agent/agent.go:526 先于 :678），宣告期间取消，消费者仍能从事件重建完整的宣告回复。`TestRunStreamCancelAfterAnnouncedCallsRebuilds` 与 `TestRunStreamCancelMidAnnouncementStillAnnouncesAll`（agent/agent_fold_error_test.go:152 / :216）钉住这两层：后者钉住"宣告批内没有 ctx 检查"这条更窄的约束——中途取消不吞掉剩余宣告，否则半条 assistant 无法重建。
3. **交付是同步的，宣告有否决力**。"消费者在宣告期间 break，本轮不执行任何工具"（agent/event.go:40-45 的 doc 注释把同步交付写成契约）。这给了 UI 层一个天然的"执行前闸门"：收到 TC、还没等到 TR 时断开流，副作用不会发生。

已知的唯一例外是 maxIter 终局批：它的 declaration、全部 skipped results、commit、Done 在首个 TC 之前已经持久预闭合，此时 break 只停止本消费者的交付，线程已 closed，`ResumeThread` 返回 `ErrNothingToResume`（agent/event.go:41-45；docs/DESIGN.md:100、:408）。普通轮的 TC/TR 期间断开则保持 incomplete/resumable，这是两种轮的本质区别。

### 3.3 交付的同步性

事件交付是同步的：`yield` 返回 false 即停止推进，所以"事件已送达"先于"循环继续"。这与 pi 的 `await emit(...)` 语义一致（docs/pi-agent-loop.md:150-151），但实现形态是拉取式 `iter.Seq2`：消费者不 range，循环不走。于是"消费者处理慢 = 背压"，不需要任何队列或缓冲。

### 3.4 为什么 ReasoningItemChunk 不发事件，DoneEvent.History 是权威快照

OpenAI Responses 协议的加密推理项（`ReasoningItemBlock{ID, EncryptedContent, Summary}`）以 `ReasoningItemChunk` 整项流式交付。循环对它的处理是：**只喂给 accumulator，不产生任何 AgentEvent**（agent/agent.go:508-512，注释"consumed by the accumulator for ordered assembly; not a consumer event"）。

原因在 DESIGN.md 的折叠契约里说得很直白：**"因为 Responses `ReasoningItemChunk` 当前不映射为 AgentEvent，consumer 不得仅靠增量 events 重建含 reasoning items 的 assistant，必须在 Done 时以该 snapshot reconciliation"**（docs/DESIGN.md:87）。拆开看：

- 加密推理项对消费者是不可解释的不透明载荷，没有可展示的"增量"；它的价值在 history 里的保序回放（推理项要和它前导的 function_call 保持声明顺序，accumulator 按 `OutputIndex` 跨类排序装配，agent/accumulator.go:75-79 与 :229-264；不变式钉于 `TestStreamingReasoningItemOrderPreserved`，agent/reasoning_order_test.go:17）。
- 若为它加事件，就要动密封七事件集。控制面设计明确冻结了这一点："sealed AgentEvent 仍为七种，不增加取消事件"（docs/DESIGN.md:125），同样的逻辑适用于 reasoning item——事件面是"canonical agent 行为的观测入口"（docs/DESIGN.md:51），不为单一协议的私有载荷开口子。协议差异被吸收在 chunk 层（未知 chunk 类型静默忽略，向前兼容，agent/agent.go:515-517），事件层保持稳定。
- 于是完整性由 `DoneEvent.History` 兜底：它是"完整、无损的权威 history snapshot"（docs/DESIGN.md:87），深拷贝交付（agent/agent.go:592 `copyMessages(history)`；CompactionEvent.History 用 `deepCopyMessages`，agent/agent.go:804）。折叠契约的表述：Done/Compaction 快照**替换**预览，而不是与增量调和（agent/agent_fold_test.go:27-29、:119）；测试文件头部的注释给出理由——"persistence, fallback conversion, and future event additions can carry information not reconstructible from the event stream"（agent/agent_fold_test.go:17-19）。
- 因此存在两种合法的消费者姿态：**普通 Chat-only history 可以做 event-only 折叠当诊断用**（agent/agent_fold_test.go 的 eventFold 就是参考实现，`TestAgentEventFoldRebuildsHistory` agent/agent_fold_test.go:168），但它**不是通用完整-history 契约**；通用契约是 Done 快照调和（docs/DESIGN.md:87）。

相邻的决定：reasoning 的**文本摘要增量**（`ReasoningDeltaChunk`）是有事件的（`ThinkingDeltaEvent`），因为那是可读文本；不发事件的只有加密条目本身。别把两者混为一谈。

### 3.5 为什么密封

密封接口 + type switch 的组合，把"新增事件类型"从兼容性问题变成编译问题：外部包无法实现 `eventType()`，所以事件集合的演化只发生在本包内、由 DESIGN.md 先行（docs-first 约定，docs/DESIGN.md:4）。消费者的 `default` 分支要么显式报错（`foldRunStream` 对未知事件返回错误，agent/agent.go:366-368），要么像 eventFold 一样 `t.Fatalf`（agent/agent_fold_test.go:69-70）——不静默吞。这与 chunk 层的宽容（未知 chunk 忽略）形成有意对比：chunk 层面向协议演化（新服务器字段），事件层面向消费者契约（新事件=新行为语义，必须显式处理）。

## 4. maxIter 语义

### 4.1 定义与默认

`maxIter` 是**LLM 往返次数上限**（`WithMaxIter` doc："the maximum number of LLM round-trips"，agent/agent.go:161；循环结构 doc agent/agent.go:107-108）。默认 10（`New` 归一，agent/agent.go:282），小于 1 归一回 10（agent/agent.go:292-294）。每一轮 = 一次 provider 调用（含其重试组）+ 其工具执行，reasoning/文本/工具调用都只是这一次响应的组成部分（对照 pi 的回合定义，docs/pi-agent-loop.md:22-27）。

### 4.2 终局轮：确定未执行，零审批零执行

最后一轮（`i == maxIter-1`）如果模型仍请求工具，走终局分支（agent/agent.go:604-656）：

1. **不审批、不执行**任何调用。这是 C10 修订的裁决（docs/DESIGN.md:512："maxIter 终局轮宣告 ToolCallEvent + 确定性 skip ToolResultEvent，零执行零审批，已落地为 agent/max_iter.go 与折叠契约的 Done reconciliation"；钉死于 `TestMaxIterTerminalSkipsApprovalAndParallelExecution`，agent/max_iter_terminal_test.go:175）。
2. 每个请求的调用配一条**确定性软错误结果**：`"not executed: the run reached its iteration limit"`（`iterationLimitResults`，agent/max_iter.go:8-17）。措辞是契约的一部分：maxIter 截断 = **确定未执行**，区别于中断恢复合成的"结果未知"（docs/DESIGN.md:99："maxIter 截断 = 确定未执行 → 合成'因达到迭代上限未执行'软结果（非'未知'）"）。
3. 事件顺序固定：`TC × N → skipped TR × N → Done(Truncated=true)`（agent/agent.go:630-654；钉死于 `TestMaxIterTerminalOrderAndCounts`，agent/max_iter_terminal_test.go:36）。这让 Store 与非 Store 路径的 Done History 完全一致，且**成对可续跑**：assistant 的 tool_use 与软结果 tool 消息满足 TC/TR 配对，下一段历史拿去续跑时 provider 转换不会报错（`TestMaxIterNonPersistentContinuationUsesPairedHistory` 证明续跑输入正是含 skipped 结果的完整历史，agent/max_iter_continuation_test.go:33-47）。
4. Store 路径下，整个终局批在首个 TC 之前**持久预闭合**（`sess.truncateRun`，agent/agent.go:619-625）：declaration、全部 skipped results、commit、Done 一次写完，消费者此后在任何点断开都不能重开这个 run（§3.2 的唯一例外）。

### 4.3 与正常完成的差异

| 维度 | 正常完成（无工具调用） | maxIter 截断 |
|---|---|---|
| 触发 | `len(toolBlocks) == 0`（agent/agent.go:535） | 最后一轮且 `len(toolBlocks) > 0`（agent/agent.go:604） |
| `Truncated` | false | true（agent/event.go:107-109 的字段语义："hit maxIter without reaching a final text response"） |
| `Message` | 最终文本回复 | 最后一条 assistant 消息——可能含文本，但**带着未执行的工具调用**（agent/agent.go:646） |
| History 尾部 | assistant 文本消息 | assistant（含 tool_use）+ N 条软错误 tool 结果 |
| FinishRound | 无工具轮会咨询（非最后一轮时） | **不咨询**——终局分支冻结（`WithFinishRound` doc："The hook is not consulted on the final allowed iteration"，agent/finishround.go:60-61；条件 `!isLast` agent/agent.go:536） |
| 后续 | run 自然关闭 | 调用方可拿 `History` 经 `RunWithHistory` 续跑（非持久），或线程续跑 |

判定的对称性值得注意：`isLast` 时即使模型给出纯文本回复，也按正常完成处理（`Truncated=false`）——截断只发生在"还想调工具但预算用尽"这一种形态（agent/agent.go:527 与 :535-604 的分支结构）。

### 4.4 为什么是内核职责

pi 无 maxIter，理由是长驻会话把防失控交给 session 层（docs/pi-agent-loop.md:59-60）。go-agent 反向选择的依据是设计目标第一条"稳定"："任务失控——库必须兜住……该刹车的刹车。用户程序不能挂，花费不能失控"（docs/DESIGN.md:23）。嵌入式使用者往往没有自己的 session 层可托付，熔断必须在循环里。同时熔断的产物是**可续跑的成对历史**而非半截废料（§4.2 第 3 点），所以"刹车"不等于"丢工作"。

## 5. 重试边界

### 5.1 一句话边界

**只有 pre-stream 失败可以重试；流内（mid-stream）错误一律终止，禁止重试。** 这条边界写在事件注释（"RetryEvent … ONLY emitted for pre-stream connection errors. Mid-stream SSE errors are NOT retried"，agent/event.go:88）、项目反模式清单（"Retrying mid-stream SSE errors — only pre-stream failures retry"，AGENTS.md ANTI-PATTERNS）和 codex 适配器契约（"惰性迭代内错误不会触发现有 agent 的外层 pre-stream RetryEvent；不能承诺自动恢复"，docs/DESIGN.md:357-358）三处。

### 5.2 chatWithRetryAndFallback：一次轮内调用的完整生命周期

`chatWithRetryAndFallback`（agent/provider_call.go:13-102）每轮被调用一次（agent/agent.go:466），内部结构：

```
attempt := 1 .. maxAttempts (= MaxRetries+1; MaxRetries=-1 时 maxAttempts=1, :24-27)
│
├─ roundContextOutbound：按需构造瞬态 runtime-context overlay (:33; round_context.go:59)
├─ provider.ChatStream(ctx, outbound, tools, opts...)
│    ├─ 成功 → 拿到 stream, break (:59-61)
│    ├─ ErrStreamingNotSupported → 降级链（§5.4）
│    └─ 其他错误 e：
│         ├─ ctx 已取消 → 终止 (:62-65)
│         ├─ !IsRetryableError(e) && !IsNetworkError(e) → 终止（不可重试类）(:66-69)
│         └─ 可重试：
│              ├─ 记录 RetryInfo → yield(RetryEvent) (:84-87)
│              ├─ OnRetry 回调 (:88-90)
│              └─ WaitForRetry 退避等待（可被 ctx 打断）(:91-94)
└─ 循环结束仍失败 → 终止: "iteration N: ... failed after M attempt(s)" (:97-100)
```

退避算法：全抖动指数退避、上限截断（`Backoff`，llm/retry.go:33-43）；若错误携带 `APIError.RetryAfter > 0` 则优先按服务器指示等待（`WaitForRetry`，llm/retry.go:54-59）；等待受 ctx deadline 约束（llm/retry.go:60-69）。

**可重试分类**只有两类（agent/provider_call.go:66）：`IsRetryableError`——`*llm.APIError` 且 `Retryable()`（429 或 5xx，llm/retry.go:72-80）；`IsNetworkError`——net.Error 的 Timeout/Temporary、url.Error 解包后的网络错误、ECONNREFUSED/RESET/EPIPE（llm/retry.go:86-114）。明确**排除**：context.Canceled / DeadlineExceeded（llm/retry.go:90-91）和 ErrStreamingNotSupported（llm/retry.go:93-95，那是降级信号不是重试信号）。取消绝不重试由 `TestAgentRun_NoRetryOnContextCanceled` / `TestAgentRunStream_NoRetryOnContextCanceled`（agent/agent_test.go:1758 / :2070）钉死。

**配置与记账**：`AgentRetryConfig`（agent/agent.go:132-142）——MaxRetries 未显式设置或为 0 都取默认 3，**-1 才是关闭**；BaseDelay 默认 500ms；MaxDelay 默认 `llm.DefaultMaxDelay`=120s（`resolveRetryConfig`，agent/agent.go:885-899）。`RetryInfo.Attempt` 是**一基总尝试数**：Attempt=1 是首次，Attempt=2 是第一次重试（agent/event.go:75）；同一份数据流向三处：RetryEvent（流式消费者）、`OnRetry` 回调（同步观察）、`DoneEvent.Retries` / `RunResult.Retries`（事后汇总，agent/event.go:71-73）。耗尽时的终止错误保留 `*llm.APIError` 可 `errors.As`，并带尝试次数（"after 4 attempt(s)"，钉死于 `TestTerminalRetryExhaustion`：恰好 1 个终态错误、OnRetry 恰好 3 次、provider 恰好被调 4 次，agent/retry_terminal_test.go:39-77）。

注意两个"Attempt"不是同一个数：`RetryInfo.Attempt` 只计 ChatStream 重试循环；`RoundContextRequest.Attempt`（agent/round_context.go:21-27）计每次**直接 provider 方法调用**（含非流式降级那次，agent/provider_call.go:32 与 :40 各自 `request.Attempt++`），DESIGN.md:163-166 明确区分二者。

### 5.3 为什么 mid-stream 错误禁止重试

流开始后，Text/Thinking delta 事件已经实时交付给消费者（agent/agent.go:492-501 在累积循环里逐 chunk yield）。此时若在流内错误处重试整个请求：

1. **消费者会看到重复输出**。已交付的 delta 无法收回，重试成功后同一段文本再来一遍；对终端 UI 这是可见的字节级损坏。
2. **计费翻倍且不可判定**。半条流的 token 消耗已发生；内核无法知道服务器端实际消耗了多少。
3. **状态可能已损坏**。中途断流可能留下重复 DoneChunk、半个 tool call 等装配异常（P0 曾修复 OpenAI 流式重复 `DoneChunk`，docs/DESIGN.md:86）；accumulator 对重复 index、孤儿 args、缺 id/name 有显式损坏检测（agent/accumulator.go:95-128），但这些属于"终止并报错"的范畴，不是"擦掉重来"。
4. **消费者可能已 break**。拉取模型下，内核甚至不保证还有人在读；重试产生的输出无处投递。

所以流内错误走最短路径：`chunkErr → yield(nil, chunkErr) → return`（agent/agent.go:487-490），错误分类、恢复决策全部上交调用方（线程运行则可经 `ResumeThread` 续跑，那是持久层的恢复语义，不在重试层）。代价是诚实的：长流在网络抖动下更脆；换来的是"消费者看到的每个字节恰好对应一次服务器输出"这条硬不变式。

pi 的对照：重试放在 pi-ai 的 StreamFn 里（docs/pi-agent-loop.md:169），即同样收在"请求建立"这一层，不在循环层对流内错误做恢复。

### 5.4 fallback 链：ErrStreamingNotSupported → Chat → synthesizeChunks

`provider.ChatStream` 同步返回 `llm.ErrStreamingNotSupported`（llm/provider.go:14）时，**同一次 attempt 内**降级到非流式 `provider.Chat`（agent/provider_call.go:39-46），成功后用 `synthesizeChunks` 把完整回复**回放成一条合规的 chunk 流**（agent/provider_call.go:49-56、:104-129）：文本块→TextDeltaChunk、推理块→ReasoningDeltaChunk、工具调用→Start+Args 一对、结尾补一个合成 `DoneChunk`，finish_reason 按"有无 tool_use"合成 stop/tool_calls（:124-128）。

设计要点：

- **降级不是重试**。它不消耗 attempt、不发 RetryEvent、不退避——是同一次尝试的协议变体。因此"流式不可用"的 provider 对循环完全透明：`TestFallbackSynthesizesEvents`（agent/agent_unify_test.go:100）证明降级路径合成的事件序列与原生流式不可区分（等价性的另一半由 `TestUnifiedRunStreamEquivalence` 覆盖）。这也是 §3.4 里"Done 快照权威"的理由之一——fallback conversion 携带的信息无法从事件流重建（agent/agent_fold_test.go:17-19）。
- 降级后的 `Chat` 若失败，错误进入**同一个重试分类**（agent/provider_call.go:46-47 赋值 lastRetryErr 后落到 :59-69 的公共路径）：429 一样退避重试，下一轮 attempt 仍先试 ChatStream。
- overlay 构造在降级前重做一次（`request.Attempt++` 后再次 `roundContextOutbound`，agent/provider_call.go:40-45），保证 RoundContextProvider 看到的快照对应**实际发出的那次请求**。

### 5.5 为什么重试住在循环而不是适配器

每个适配器自带重试的方案被否决（内核只保留适配器各自的 401 刷新等协议内机制，如 codex 的"最多重发一次"，docs/DESIGN.md:356-357）。理由：重试分类依赖统一错误模型（`*llm.APIError` 是 llm 包的公共叶子类型，Retryable() 驱动分类，AGENTS.md CONVENTIONS）；重试预算属于"花费不能失控"（docs/DESIGN.md:23）的内核职责，多个适配器各自计数会让总预算不可控；RetryEvent/OnRetry/Retries 汇总的观测面只有循环层能提供（一次轮内所有尝试的完整账目）。于是适配器契约被收紧为"如实返回可分类的错误、不吞哨兵"（包装器守则：不得吞 `ErrStreamingNotSupported`，否则非流回退与重试分类失效，docs/DESIGN.md:62）。

## 附录：本文依赖的契约测试索引

| 契约 | 测试 |
|---|---|
| Run/RunStream 单路径等价 | agent/agent_unify_test.go:14、:167 |
| 非流式降级合成事件 | agent/agent_unify_test.go:100 |
| 消费者早退安全 | agent/agent_test.go:1483 |
| 事件折叠重建 + Done 快照替换 | agent/agent_fold_test.go:119、:168（eventFold 契约 :17-29） |
| 终态错误下的折叠（保留已宣告回复/丢弃装配前增量） | agent/agent_fold_error_test.go:38、:75、:106 |
| TC-first 取消可重建、宣告批无取消检查 | agent/agent_fold_error_test.go:152、:216 |
| ReasoningItem 保序（流式/终态/降级/深拷贝） | agent/reasoning_order_test.go:17、:76、:114、:169 |
| maxIter 终局顺序/纯文本归一/零审批零执行 | agent/max_iter_terminal_test.go:36、:127、:175 |
| maxIter 成对历史可续跑、WithHistory 入口非持久 | agent/max_iter_continuation_test.go:33、:49 |
| 重试耗尽终态（1 错误/3 回调/4 调用） | agent/retry_terminal_test.go:39 |
| 取消不重试 | agent/agent_test.go:1758、:2070 |
| 密封接口与事件类型名 | agent/event_test.go:23（编译期断言 :15-21） |
| 临时运行 ThreadID 为空 | agent/event_test.go:45 |
