# 13 · 上下文管理（agent 包）

> 内核文档集第 13 篇。本文讲 `agent` 包上下文管理的三层机制：历史压缩（agent/compact.go + 循环内的 `compactHistoryRound`）、工具结果截断（agent/truncate.go）、逐轮运行环境上下文（agent/round_context.go）。冻结契约是 docs/DESIGN.md P1 节（docs/DESIGN.md:89-99）与「逐轮运行环境上下文（冻结契约）」（docs/DESIGN.md:159-221）。轮顶顺序与事件模型归 10-agent-loop.md，steering / 配置热更新 / FinishRound 归 14-agent-control-plane.md，checkpoint / 重放 / 结算归 12-agent-persistence——本文只在交界处引用，不重写它们的契约。14-agent-control-plane.md §6 已把 overlay 预算计算与截断算法划归本文，§3.5 已把 rs_ 推理条目的跨压缩保序划归本文。行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），与 10/14 两篇同批。

## 1. 三层机制：源头限幅、历史预算、瞬态注入

| 层 | 文件 | 管什么 | 生效点 |
|---|---|---|---|
| 截断 `truncateToolResult` | agent/truncate.go:17 | 单条工具结果的体积上限 | 每个工具结果解析点（入库/入史/入事件之前） |
| 压缩 `compactHistoryRound` | agent/agent.go:757（策略在 agent/compact.go） | 整段历史对预算的达标 | 每轮顶部、provider 请求之前 |
| round context overlay | agent/round_context.go:59 | 当次请求携带的瞬态事实快照 | 每次直接 provider 调用之前（含每个 retry attempt） |

三层的分工对应「该截断的截断、该刹车的刹车」的稳定性目标（docs/DESIGN.md:23）：截断从源头防一条超大结果污染历史；压缩在历史整体超预算时收缩它；round context 则是反向问题——往请求里注入预算内的新鲜事实而不弄脏历史。分层判据是「内核装机制，扩展包装策略」（docs/DESIGN.md:58）：截断边界是机制，压缩算法是策略（`Compactor` 接口 + 可注入实现）。

三层共用同一把尺子：`estimateRunes`（agent/compact.go:206）——把 `[]llm.Message` JSON 序列化后数 rune，失败时退化为逐块长度求和。这是**消息 rune 启发式**，不含 tool schema、provider wire 映射、reserved output 或真实 tokenizer（docs/DESIGN.md:180-182）；预算承诺的是这个启发式，不是 token 数（docs/DESIGN.md:506 明示「rune 估算为启发式非保证」）。steering 的准入与 drain 计量也用它（见 14-agent-control-plane.md §2.2），四处预算因此永不互相矛盾。

## 2. 压缩（compact.go + compactHistoryRound）

### 2.1 触发规则：默认窗口的 80%

```go
targetRunes := (a.contextWindowTokens/100)*a.compactionThresholdPercent +
    (a.contextWindowTokens%100)*a.compactionThresholdPercent/100   // agent/agent.go:765-767
if a.compactor != nil && beforeRunes > targetRunes { ... }          // agent/agent.go:771
```

- 窗口默认 `DefaultContextWindowTokens = 8_192`（agent/agent.go:147，`WithContextWindowTokens` :169 可改，≤0 归一默认）；阈值默认 `DefaultCompactionThresholdPercent = 80`（agent/agent.go:151，`WithCompactionThresholdPercent` :181 可改，[1,100] 外归一默认）。两者都在 `New` 里规范化（agent/agent.go:283-299）。
- 触发条件是**严格大于**：`beforeRunes > targetRunes`。等于阈值不压缩（TestAgentCompaction "below and equal threshold do not compact"，agent/agent_compact_test.go:90；策略级同语义见 TestCompactorThresholdSemantics，agent/compact_test.go:232）。溢出安全写法 `(t/100)*p + (t%100)*p/100` 即 `floor(t*p/100)`，与 `toolResultRunes` 同一惯用法（agent/agent.go:765 注释）。
- 触发点在轮顶固定顺序 `[2]`：`读配置 → 压缩 → drain → 预算检查 → 请求`（agent/agent.go:415，冻结契约 docs/DESIGN.md:232，全景见 10-agent-loop.md §2.1-2.2）。压缩在 provider 请求**之前**执行，本轮请求看到的就是压缩后的历史（TestAgentCompaction "above threshold compacts before provider rounds"，agent/agent_compact_test.go:141）。

**为什么是 80% 而不是贴着 100%**：检查点只在轮顶存在，两次检查之间历史要长出一整轮——一条 assistant 回复加最多 N 条工具结果。20% 余量让单轮增长在下一个检查点到来之前仍落在 provider 真实窗口内；贴满窗口触发则每次压缩后立刻再次超限，或者更糟：请求在检查点之间就被 provider 拒绝（上游不会替你静默截断，见 §3.1）。80% 是可调旋钮而非硬编码，宿主按自己的模型窗口与单轮膨胀特征调 `WithCompactionThresholdPercent`。策略组合的形态（丢最老工具组 → 滑窗 → 摘要，token 预算下依序执行）参照 Microsoft Agent Framework（docs/DESIGN.md:491）。

```
轮顶（agent/agent.go:415-478）
│
├─[1] 读配置   refreshConfig → cfg.apply → provider 解析（本轮 prov/opts 整轮钉住）
│
├─[2] 压缩     compactHistoryRound（agent/agent.go:440 调用，实现 :757）
│      targetRunes = floor(window × percent/100)        ← 默认 8192 × 80% = 6553
│      启用 round context 时 target 收窄到 canonicalContextCap（§4.3）
│
│      beforeRunes = estimateRunes(history)
│      ├─ compactor == nil 或 beforeRunes ≤ target → 不压缩，直落 [3]
│      └─ beforeRunes > target：
│           Compactor.Compact(ctx, history, {MaxRunes: targetRunes}, prov, opts)
│           ├─ 策略级失败（result.Errors）→ 逐条 Warn，链内继续
│           ├─ 整体 err / validateCompacted 不过 → Warn，history 原样（非致命）
│           └─ 成功且 result.Changed：
│                ① sess.checkpoint：压缩视图先落库   agent/persist_util.go:18
│                ② Warn "history compacted"           agent/agent.go:793
│                ③ yield(CompactionEvent)             agent/agent.go:799
│                ④ 预算护栏：仍 > target+carry 且无 overlay
│                   → ErrCompactionBudgetExceeded 致命  agent/agent.go:823
│
├─[3] drain   available = steeringAdmissionRunes() − estimateRunes(history)
│             （agent/agent.go:449；FIFO 前缀消费，装不下的留队列）
├─[4] 预算检查  启用 overlay 时：canonical history ≤ canonicalContextCap
│             否则报 ErrCompactionBudgetExceeded（agent/agent.go:457-459）
└─[5] 请求     chatWithRetryAndFallback：每次 attempt 现场做 round context overlay
```

护栏的两条分支要分开读（§2.6 与 14-agent-control-plane.md §2.3 详述 carry）：压缩**失败本身**非致命——Warn 之后 run 带原历史继续；但采纳与否都过一遍护栏，无 overlay 时 `estimateRunes(history) > targetRunes+carry` 就以 `ErrCompactionBudgetExceeded` 结束 run（agent/agent.go:819-824）。也就是说「压缩器坏了且历史确实压不下去」最终仍会刹车，刹车的原因是预算护栏而非压缩错误。启用 overlay 时护栏换成无条件的 canonicalCap 检查（§4.3）。已知残余：保护组（§2.4）自身超预算时同样报 `ErrCompactionBudgetExceeded`（docs/DESIGN.md:506）。

### 2.2 消息组：压缩不可拆的语法单位

`partitionHistory`（agent/compact.go:79）把历史切成组：开头的连续 system 消息一组；每条 user 一组；不带 tool_use 的 assistant 一组；**带 tool_use 的 assistant 与其后的全部 role=tool 结果合成一组**，且 `validateToolResults`（agent/compact.go:162）逐 ID 校验——缺结果、多结果、未知 ID、重复 ID 一律 `ErrInvalidHistory`（agent/compact.go:15）。system 出现在前缀之后、孤儿 tool 结果、未知 role 同样拒绝。

为什么先有组再做压缩：role=tool 消息离开它的 tool_use 就是语法残片，多数 provider API 会直接 400。所有策略都只以组为原子单位动作——滑窗删整组、折叠换整组——**组内与组间的相对顺序从不改变**。这顺带锁死了跨 provider 降级的一个前置条件：openairesponses/codex 的 `rs_` 推理条目与其后的 tool_use 同生同灭在同一个 assistant 消息里，压缩要么整体保留、要么整体随组消失，永不产生孤儿 rs_ 条目（孤儿会被 OpenAI 400）；降级规则表本身冻结在 docs/DESIGN.md:236 与 14-agent-control-plane.md §3.5。

### 2.3 策略族：折叠、滑窗、摘要、链

| 策略 | 构造 | 动作 | 中性策略? |
|---|---|---|---|
| drop_oldest_tool_groups | `NewDropOldestToolGroupsCompactor()` agent/compact.go:269 | 把最老的**未保护**工具组折叠成一条 assistant 文本消息：`[Tool Calls]\n名字: 结果首行` 逐行拼接，整段上限 4096 rune（`maxToolCollapseRunes`，agent/compact.go:23），超出加 `... [truncated]` 后缀；替换物比原组**不小**就跳过该组找下一个 | 是（忽略 provider/opts） |
| sliding_window | `NewSlidingWindowCompactor()` agent/compact.go:401 | 整组删除最老的未保护组，直到达标或无组可删 | 是 |
| summarization | `NewSummarizationCompactor()` agent/compact.go:484 | 把首个 user 组与最新 user 组**之间**的整段历史拉平成转写文本，用默认摘要 prompt（agent/compact.go:22，要求保留目标、关键决策、约束、进度、下一步、工具结果意义、关键数据逐字）调 `provider.Chat`（非流式、不带 tools），产出的摘要以 `[Summary]\n` 前缀的 assistant 消息插回两段 user 之间 | 否（消费 provider/opts） |
| chain | `NewCompactorChain(...)` agent/compact.go:600 | 依序执行每个策略至多一次 | — |
| standard | `NewStandardCompactor()` agent/compact.go:605 | `chain(drop_oldest_tool_groups, sliding_window)`，内核推荐默认 | — |

链的采纳规则（agent/compact.go:612-647）：入口先做整体判断，已在预算内则一个策略都不跑；每个策略至多跑一次；策略报 `ErrCompactionBudgetExceeded` 时**不跳过采纳**（部分进展仍可能小于原历史），其他错误记入 `result.Errors` 后跳过采纳继续下一策略；采纳条件是候选历史不大于当前历史——一个把历史越压越大的"压缩器"永远不会被采纳；达标即 break。这些由 TestCompactorChain（agent/compact_test.go:641）钉死。取消是协作式的：两个中性策略在每个慢操作（分区、估算、每轮候选）前后查 `compactionContextErr`（agent/compact.go:258），取消时返回的部分历史仍保持合法组结构（TestCompactorStrategiesObserveCancellation，agent/agent_compact_test.go:21）。

摘要策略的失败面全部显式：provider 为 nil、返回 nil、返回空文本各自报错（TestCompactorSummarization 的 nil response / response without text / provider error，agent/compact_test.go:590-613）；摘要后仍超预算报 `ErrCompactionBudgetExceeded`；首个与最新 user 之间没有内容（empty span）是无操作、不调 provider（agent/compact_test.go:617）。这些错误在链里被吞成 `result.Errors`、在循环里逐条 Warn——摘要挂了，链后面的滑窗还能救场。

### 2.4 保护组：什么永远不被压掉

`protectedGroups`（agent/compact.go:650）标记四类不可动组：system 前缀组、**首个 user 组**、**最新 user 组**、最后一个组。理由直白：首个 user 是任务原始定义，丢了任务就变了；最新 user 是当前指令；最后一组是正在进行时的上下文；system 前缀是 kernel grammar 与 checkpoint 的冻结契约（docs/DESIGN.md:172-174）。用户/宿主对模型的显式输入高于一切预算考量——超预算宁可报错也不吃掉它们（§2.1 的已知残余正是保护组自身超预算时报错）。采纳侧还有第二道防线：`validateCompacted`（agent/agent.go:88）对压缩前后各做一次分区并要求 system 前缀逐字相同，不认识的压缩器产物直接拒收（Warn "history compaction produced invalid history"，agent/agent.go:780）。

### 2.5 摘要必须跟随当前线程配置（provider 热换语义）

`Compactor` 接口带着一个刻意设计的调用面（agent/compact.go:49-52）：

```go
Compact(ctx, history, budget CompactionBudget, provider llm.Provider, opts []llm.Option)
```

provider 与 opts 由**调用方**逐次传入：循环内调用方（`compactHistoryRound`）从线程配置投影解析后注入——这正是轮顶顺序 `[1] 读配置` 先于 `[2] 压缩` 的原因之一（agent/agent.go:757 的注释）；手动调用者传自己想用的。这条签名是真实裁决过的语义，不是接口装饰：

具体场景（14-agent-control-plane.md §4.2 的裁决行）：openai 线程配额耗尽，宿主 `SetThreadConfig` 切到 glm，对话继续跑了 30 轮后触发压缩——如果摘要器还绑着构造期的 openai，它会去调一个已经死掉的厂商，压缩静默坏死，或者干脆把整条线程拖垮。所以规则是**摘要跟着对话换厂商**：线程热切换 provider 后，压缩/摘要调用新 provider 与线程生效 model（docs/DESIGN.md:91 冻结契约）。`NewSummarizationCompactor()` 刻意不接受构造期 provider（agent/compact.go:484）——被否决的形态就是构造期注入，它必然绑死在切换前的厂商上。该语义由 TestSummarizationFollowsSwitchedProvider 钉死（agent/provider_hotswap_test.go:327，含第三个 run 经 checkpoint 重建后首轮请求仍解析到切换后 provider 的尾段）。

中性策略（折叠、滑窗）对 provider/opts 视而不见（参数签名 `_ llm.Provider` 并注释 "neutral strategy"，agent/compact.go:277-279）——它们不调 LLM，这两个参数对它们无意义；接口不为它们收窄。

### 2.6 持久化与重放：checkpoint 先于事件

压缩视图的落库遵守「durable-before-visible」：`sess.checkpoint` 在 `yield(CompactionEvent)` **之前**（agent/agent.go:786-799），消费者看到事件时压缩已经扛得住崩溃。checkpoint 的动机写在函数头（agent/persist_util.go:13-15）：**重放绝不重跑非确定性的摘要器**。压缩是「调一次 LLM、产出不可复现」的操作；若只记事件不落快照，崩溃后的 Resume 必须重新调一次摘要 provider——又花钱又可能与已消费的 CompactionEvent 历史不一致。

两条配套设计：

- **checkpoint 是加速器不是真相**。payload 携带 `{CodecVersion, History, System, RunActive, RunID, LastRound, Config, Queue}`（agent/persist_util.go:20-27），自身定位在日志 Seq；重放遇 checkpoint 只推进 head、不并入历史（"accelerator, never history"，agent/persist_replay.go:158），历史真相从快照的 `History` 起点接尾段重放。快照损坏或过旧（pre-v3 无控制面投影）时整体回退全量重放（agent/persist_replay.go:100-124，D9 规则：旧快照不得绕过新语义）。写协议（位置绑定 ID `cp-<head>`、rebase 后从活投影重新序列化）归 12-agent-persistence。
- **CompactionEvent 不重放**。事件是本 run 消费者的现场通知，不是日志记录；恢复语义明确「不向新流重放历史事件」（docs/DESIGN.md:99）。重放出来的历史直接就是压缩后的形态——因为 checkpoint 已把压缩视图定为新起点。`CompactionEvent`（agent/event.go:126）携带 `Strategies / DroppedGroups / BeforeRunes / AfterRunes / History`（防御性深拷贝），语义与钉死测试见 10-agent-loop.md §2.3 与 §3.1；一次性准确发射由 TestAgentCompaction "changed compaction emits one accurate event"（agent/agent_compact_test.go:198）保证。

崩溃窗口三态由 TestSettleInterruptCompaction（agent/settle_interrupt_compaction_test.go:16）钉死，细节归 12-agent-persistence：E08 压缩进行中取消（无 checkpoint，SettleThread 得到未压缩历史）；E09 checkpoint 已落库（崩溃后重放以压缩视图为起点）；E10 checkpoint 的 Append 结果不明（ack 丢失）——原 token 失效报 `ErrRevisionConflict`，显式 `SettlementTarget` 重新检查后可结算。结算路径同样零重入：settled snapshot 不重跑 compactor、不发 overlay、不碰 provider（同测试末段断言）。

## 3. 截断（truncate.go）

### 3.1 规则：约 30% 窗口，掐头留尾

上限由 `toolResultRunes`（agent/agent.go:908）给出：`floor(window × 3/10)`，下限 `minToolResultRunes = 128`（agent/truncate.go:10）。默认窗口 8192 → 2457 rune（TestAgentToolResultLimit "default limit is enabled"，agent/agent_truncate_test.go:24 断言 2457）。约 30% 的占比规则意味着一个 run 内至少能容纳任务、历史与三条彼此独立封顶的结果（docs/DESIGN.md:90、:499）。

`truncateToolResult`（agent/truncate.go:17）的形态：

- **掐头留尾，50/50**：保留 rune 均分为头部与尾部（kept 为奇数时尾部多一个 rune），中间换成标记 `\n...[tool result truncated: omitted N of M runes]...\n`，最终长度**恰好等于**上限（不等则 panic，agent/truncate.go:53-54）。
- 标记长度依赖 N/M 的十进制位数，位数循环迭代到收敛（位数只增故必收敛，反向收缩是 caller bug，panic，agent/truncate.go:42）；上限必须 ≥128 才保证标记装得下、至少留 1 个内容 rune，违反即 panic（agent/truncate.go:33-35）。三个 panic 全是 caller bug 防线——`toolResultRunes` 的构造保证了它们在内核内不可达。
- 截断返回**浅拷贝**（`cpy := *result` 换 Content，agent/truncate.go:57-59）：`Status` / `IsError` 等软错误语义原样穿透（TestAgentToolResultLimit "error status survives truncation"，agent/agent_truncate_test.go:193）；不截断时原样返回指针、零分配。

为什么掐头又留尾、且必须发生在本层：只掐头不留尾是被真实 bug 教训过的形态（Hermes 案例，docs/DESIGN.md:490）——工具输出的尾部往往是结论（退出码、最后一行错误、`tail` 的本义）；上游 API 也不会替你静默截断（Claude 超限直接报错），限幅必须发生在进入历史之前。截断在**每个结果解析点**执行：并行/串行执行路径（agent/tool_execution.go:96）与规划器立即解析路径（agent/tool_execution_planner.go:100、:145——跳过、错误等合成结果同样过一遍，管道统一）。

### 3.2 轨迹保存语义：一份拷贝，四处同源

截断后的内容**就是**轨迹本身，不存在"截断版给模型、全文另存"的两份真相：

- ToolResultEvent 的 `Result.Content`、下一轮 provider 请求里的 tool result、`DoneEvent.History`、Store 的 `round_committed` 记录——四处是同一份截断后内容（"event request and history have identical content"，agent/agent_truncate_test.go:60；落库侧契约 docs/DESIGN.md:95「结果先截断后落库（与内存视图同源）」）。
- 消费者拿到事件后改 `Result.Content` 影响不到历史（深拷贝防线，agent/agent_truncate_test.go:127）。
- 多条结果彼此独立封顶，一条巨结果不吃掉别条结果的额度（agent/agent_truncate_test.go:162）。

**超长原文不保留，这是稳定目标的代价**（docs/DESIGN.md:27 原文表述：「工具结果按截断策略后的轨迹保存，超长原文不保留（稳定目标的代价，截断时记 Warn 日志）」）。理由是预算承诺的对象就是"将要进入模型视野与日志的轨迹"：若原文保留在任何一处（历史、日志或侧通道），立即出现模型看到的与存储保存的两份真相，replay 即重建的历史就不再是请求看到的那个历史，压缩与预算护栏的目标也随之漂移。审计诉求由 Warn 日志承接：`tool result truncated` 带 `tool_name / tool_call_id / omitted_runes / total_runes / cap_runes`（agent/agent.go:918-930）——丢了什么、丢了多少、按什么上限，全部可查。宿主确实需要全文时，正确位置在工具实现侧：工具在返回前持有自己的输出，自行保存全文、返回引用（路径、ID）即可；这不是内核职责。

与软错误契约的关系：截断只改 Content，不改语义。`ResultError` 截断后仍是 `ResultError`（§3.1 的浅拷贝保证），失败反馈回 LLM 的机制不受影响——详见 11-agent-tools.md 的软错误契约。

### 3.3 命名辨析：truncateToolResult ≠ truncateRun

`truncateRun`（agent/persist.go:373）是 **run 级截断**——maxIter 终局轮的持久预闭合（声明、全部 skipped 结果、commit、Done 一次写完），属于迭代预算与持久化提交面，见 10-agent-loop.md §4 与 11-agent-tools.md 的 maxIter 节。两者共享"截断"一词但机制正交：前者裁内容，后者收 run。maxIter 的 skipped 结果文本极短，但仍走 §3.1 的统一管道（agent/tool_execution_planner.go:145）。

## 4. round context（round_context.go）

### 4.1 机制：每次直接 provider 调用前的瞬态 overlay

```go
type RoundContextProvider func(context.Context, RoundContextRequest) (string, error)  // agent/round_context.go:19
type RoundContextRequest struct { Round, Attempt int; ThreadID string }               // agent/round_context.go:23
```

调用粒度是**每次直接 provider 调用**：`chatWithRetryAndFallback` 在每个 attempt 前递增 `request.Attempt` 并现场调用 `roundContextOutbound`（agent/provider_call.go:32-33），ChatStream 降级 Chat 的那次调用有自己的 attempt 号（:40-41）。因此 `Attempt` 是本轮**直接 provider 方法调用的一基序号**，与只计 ChatStream retry-loop 的 `RetryInfo.Attempt` 语义不同（docs/DESIGN.md:164-168 冻结契约；对照测试 TestRoundContextProviderFallbackRetryAttemptDivergence，agent/round_context_test.go:400，断言 attempt 序列 [1 2 3] 而 RetryInfo 只有一次 attempt=2）。`Round` 是逻辑模型轮，跨 Resume 延续 durable 序号（agent/agent.go:472 的 `sessionRoundBase`）。callback 在单次 run 内串行；同一 Agent 的并发 run 由宿主保证 callback 并发安全（docs/DESIGN.md:168-169）。

非空快照包装为**本次出站请求末尾的一条 role=user 消息**（agent/round_context.go:50），带固定信封 `[go-agent runtime context snapshot; current facts only, not a new user task]`（agent/round_context.go:13）向模型声明这是事实快照不是新任务。选 trailing user 做 carrier 是排除法的结果：canonical history 的 system 前缀是冻结契约，Responses 协议又把 system 提升为单条 instructions，中途 system 消息没有跨协议统一语义；所有现有 adapter 都保留 trailing user，故它是唯一共同 carrier（docs/DESIGN.md:172-177）。每次 callback 返回完整有效快照，内核不按内容去重；空字符串 = 该次无 overlay（agent/round_context.go:79-81）。

它是**瞬态 request overlay**：追加在出站副本上（`append(copyMessages(history), message)`，agent/round_context.go:99——从不改动 canonical history），不进入 canonical history、`DoneEvent.History` / `RunResult.History`、任何 AgentEvent、Store 日志或 checkpoint，也不改变 sealed 七事件集（docs/DESIGN.md:172-174）。TestRoundContextProviderStaysOutOfPreDoneFoldArtifacts（agent/round_context_test.go:240）连 pre-Done 的折叠中间态都验证不含 overlay。

### 4.2 事实与任务：双通道

控制面把外部输入分成两类（14-agent-control-plane.md §6 的完整论述）：

| | steering 输入 | round context 快照 |
|---|---|---|
| 性质 | 任务：用户的指令，改变对话走向 | 事实：当前环境状态，供模型参考 |
| 通道 | durable 队列 → drain 注入 canonical history | 每轮现场 callback → 瞬态 overlay |
| 持久化 | `input_queued` / `input_consumed` 记录，崩溃不丢 | 从不落日志，Resume 后重新采集 |
| 注入位置 | history 尾部（在 overlay 之前） | 出站请求最后一条消息 |

判据是需求本身：任务必须可恢复（崩溃后模型还要看到它说过什么）、必须进历史（它是对话的一部分）；事实必须新鲜（每轮重新采集才有意义）、进历史反而污染（旧事实永远留在对话里是错误）。需要扛过崩溃的去日志，需要新鲜的走回调，两套通道互不越界——这也是为什么 steering 的内存队列形态被否决而 round context 的持久化形态被否决（14-agent-control-plane.md §2.1 的 D1 裁决与 §6）。

### 4.3 预算耦合：20% 预留与 canonicalContextCap

启用 overlay 后窗口被切成两半（agent/round_context.go:42-48）：

```go
overlayCap()          = contextWindowTokens / 5          // 20% 预留给 overlay
canonicalContextCap() = contextWindowTokens − overlayCap  // canonical 历史的硬限制
```

这个 20% 预留穿透三层预算，保证 overlay 的空间**活过**其他机制的消耗：

- **压缩目标收窄**：`roundTargetRunes` 在 target 超过 canonicalCap 时收窄到 canonicalCap（agent/agent.go:731-738）；`compactHistoryRound` 同步（agent/agent.go:768-770）。压缩后还有一条**无条件**的 canonicalCap 检查——nil compactor、压缩失败、校验拒收、采纳成功四条路径全部覆盖（agent/agent.go:829-832；docs/DESIGN.md:182-185「compaction 后无条件检查 canonicalCap（包括 nil/error/invalid compactor 路径）」）。这就是 §2.1 里「无 overlay 才有 target+carry 护栏」的原因：overlay 下护栏换成这条更严的检查。
- **steering 准入与 drain 收窄**：`steeringAdmissionRunes()` 在 overlay 启用时返回 canonicalCap（agent/agent.go:745-750），drain 的可用额度 = 它减去当前历史（agent/agent.go:449）。插话再大也不能把 canonical 历史顶过 cap、挤掉 overlay 的预留。
- **drain 之后的兜底**：注入后 `estimateRunes(history) > canonicalContextCap` 直接报 `ErrCompactionBudgetExceeded`（agent/agent.go:457-459）。

overlay 自身两级预算（agent/round_context.go:59-100）：

1. **信封预检在 callback 之前**（agent/round_context.go:67-70）：连空信封都装不进 overlayCap 的 tiny window 直接报错，不调 callback、绝不发送破碎 overlay（docs/DESIGN.md:186-188）。
2. **内容适配**：完整快照满足双约束——增量 ≤ overlayCap **且** 完整出站候选 ≤ contextWindowTokens（`fits`，agent/round_context.go:103-108）——就原样注入；否则二分搜索最大保留量，头尾保留、中间插入 `[... runtime context truncated ...]` 标记（`boundedRoundContextMessage`，agent/round_context.go:102-141；头部取 `ceil(keep/2)`，头半多一个 rune）。连最小截断形态都装不下才报错。截断时 Warn `round context truncated`，带 round / attempt / total_runes / cap_runes / omitted_runes（agent/round_context.go:91）。

超预算统一报 `ErrCompactionBudgetExceeded`；callback 自身的错误包装为 `RoundContextError{Round, Attempt, Err}`（agent/round_context.go:30-40，`errors.As` 可达、Unwrap 保留原因）；callback 前的取消不伪造该内层错误（TestRoundContextProviderCancellationBeforeFallbackSkipsCallback，agent/round_context_test.go:276——取消穿透为 `context.Canceled` 而非 RoundContextError）。

两条截断算法的取向差异值得一提：工具结果截断（§3.1）尾部多留一个 rune（结论常在尾部），round context 截断头部多留一个 rune（快照开头通常是时间/身份等最具定位价值的事实）。

### 4.4 与排水顺序、持久化的交互

**排水顺序**：round context 在轮顶顺序 `[5] 请求` 的内部、按 attempt 现场执行——位于 drain（`[3]`）与预算检查（`[4]`）之后。因此 drain 注入的插话先落进 canonical history、再被计入 overlay 的 fits 判定；overlay 永远是出站请求的最后一条消息，永远在所有任务类输入之后。顺序对调（先 overlay 后 drain）会让 fits 的候选与实际请求不一致，预算检查失去意义——这是轮顶顺序冻结的一部分（docs/DESIGN.md:244；drain 在压缩之后的理由归 14-agent-control-plane.md §2.3）。

**持久化交互**（TestRoundContextProviderPersistence，agent/round_context_persist_test.go:14）：

- 快照从不进日志：全量扫描线程记录 payload 不含任何快照文本（:62-66）。overlay 只存在于出站请求这一瞬间。
- Resume 先重放 canonical history，再执行 live callback——不重放、不累积、不双计旧快照（docs/DESIGN.md:206-208；测试断言第二个 run 的请求只含新快照 snapshot-B 不含 snapshot-A）。
- 崩溃窗口的边界（TestRunInterruptedErrorPersistentBoundaries，agent/round_context_persist_test.go:91）：run_started 确认之后，callback 错误、预算失败等一切交付错误包为 `RunInterruptedError{ThreadID, Err}`——可 `errors.Is/As` 原因、可直接 `ResumeThread`（预算失败同时暴露 ThreadID 与 `ErrCompactionBudgetExceeded` sentinel，:151）；session 建立前的错误保持原样；消费者主动 early-break 无错误、不触发包装。压缩与 overlay 的交错崩溃面由 TestSettleInterruptCompaction 覆盖（§2.6）：压缩/checkpoint 只碰 canonical history，callback 失败前可能已有合法 CompactionEvent 与 checkpoint，但绝无 overlay、provider 响应或 Done 事件（docs/DESIGN.md:188-190）。

`RoundContextProvider` 是策略注入点而非接缝——内核对快照内容一无所知，只管包装、预算与瞬态性（00-overview.md §4.5）。控制面视角的角色论述（事实/任务分野、steering 耦合）见 14-agent-control-plane.md §6，本文不重复。

## 5. 契约测试索引

| 文件 | 覆盖 | 代表用例 |
|---|---|---|
| agent/compact_test.go | 分区语法、rune 估算、阈值语义、三策略与链、调用方不可变性、保护组 | :232 阈值边界、:288 折叠次序与保护组、:512 摘要成功/失败面/空 span、:641 链序与降级、:790 标准链唯一保护组 |
| agent/agent_compact_test.go | 循环内触发点、事件准确性、取消、Warn 日志 | :21 取消、:90 等于/低于阈值不压缩、:141 超阈值先压缩后请求、:195 事件字段准确 |
| agent/truncate_test.go | 截断算法本体：位数收敛、恰好等于上限、panic 条件、浅拷贝 | 算法级白盒 |
| agent/agent_truncate_test.go | 上限派生、四处同源、消费者改不动历史、独立封顶、软错误穿透 | :24 默认 2457、:60 四处同源、:127 变更防线、:162 独立封顶、:193 Status 穿透 |
| agent/round_context_test.go | overlay 核心、预算边界、每轮每 attempt、折叠隔离、截断、attempt 语义 | :139 恰好压线/超一 rune、:204 每轮一次且不进 History、:240 pre-Done 隔离、:301 双截断共存、:351/:400 attempt 序列 |
| agent/round_context_persist_test.go | 快照不落日志、Resume 重采集、RunInterruptedError 边界 | :14 持久化隔离、:91 中断包装边界 |
| agent/round_context_integration_test.go | 压缩 × 取消 × overlay 交错 | :14 集成面 |
| agent/settle_interrupt_compaction_test.go | 压缩崩溃三态（进行中/已提交/结果不明） | :16 E08/E09/E10 |
| agent/provider_hotswap_test.go | 摘要跟随切换后 provider | :327 含 checkpoint 重建尾段 |

并发面随仓内常规命令覆盖：`go test -race -shuffle=on -count=1 ./agent`。
