# 12 · 持久化与结算（agent 包）

> 内核文档集第 12 篇。本文只讲 `agent` 包的线程持久化：记录模型（append-only 日志 + checkpoint 加速器）、重放状态机、`WithThread` / `ResumeThread` 的 API 语义、settle 结算机制（`SettlementToken` / `SettleThread` / `SettlementTarget`）与中断恢复的逐场景行为，以及持久化 DTO 的版本演进规则。
> 相邻专题各有专篇：第 10 篇（10-agent-loop.md，自主循环与 maxIter 终局轮）、第 11 篇（11-agent-tools.md，工具执行模型——declareRound 先于审批、单次 commitRound、并行 commit 的 `context.WithoutCancel` 都在那篇展开，本文与之互为参照）、第 13 篇（13-agent-context.md，压缩与预算，规划中）、第 14 篇（14-agent-control-plane.md，控制面记录 `config_changed` / `input_queued` 的写入侧）、第 30 篇（30-store，Store 契约、JSONL/内存后端、乐观并发与幂等，规划中）。持久化的总裁决在 docs/DESIGN.md「P1 运行时安全」（docs/DESIGN.md:91-144），本文与其冲突时以 DESIGN.md 为准。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），全部经 grep 校验。

## 1. 记录模型：append-only 日志是主体，checkpoint 是可重建加速器

### 1.1 线程 = 一条 append-only 记录日志

一个持久线程就是 `store.Record`（store/store.go:83）的 append-only 序列，由 `Seq` 单调编号；`Append(ctx, thread, expected, records...)` 以 expected head 做乐观锁（store/store.go:127）。生命周期记录的语义归 agent 层所有，store 只管字节与一致性（00-overview.md §4.4）。完整 kinds 清单与后端实现见第 30 篇；agent 层写入的全部记录：

| kind | schema | 记录 ID | 载荷要点 | 写入点 |
|------|--------|---------|----------|--------|
| `run_started` | 1 | `<runID>-start` | 逐字节保留的输入 + 线程冻结 system 提示词 | `prepareSessionLocked`（agent/persist.go:73） |
| `round_declared` | 1 | `<runID>-d<n>` | 整轮 assistant 消息（文本/推理 + 全部有序 tool_use）；`kind` 字段泛化：空串=工具轮，`terminal_text`=FinishRound-Continue 的无工具轮 | `declareRound`（agent/persist.go:303）/ `declareTerminalRound`（:326） |
| `round_committed` | 1 | `<runID>-c<n>` | 按声明序的模型可见结果，与声明逐位配对 | `commitRound`（agent/persist.go:355） |
| `checkpoint` | 1/2/3 | `cp-<head>` | 压缩后历史快照 + run 生命周期上下文 + 控制面投影 | `persistence.checkpoint`（agent/persist_util.go:18） |
| `agent_event` | 1 | `<runID>-done` | 版本化事件信封，当前只有 `done` 类型 | `finishRun`（agent/persist.go:391） |
| `run_closed` | 2 | — | FinishRound-End 的合作式终态，不重携带消息 | `closeRun`（agent/persist.go:346） |
| `run_cancelled` | 2 | `<runID>-cancel` | 显式结算终态；有 open declaration 时单条记录原子携带全部 unknown 结果 | `threadView.cancelRecord`（agent/persist_replay.go:62） |
| `config_changed` / `input_queued` / `input_consumed` | 2 | 幂等键 / nonce | 控制面记录（第 14 篇），重放时折叠进 `configState` / steering 队列投影 | `SetThreadConfig`（agent/config.go:59）/ `EnqueueInputWithID`（agent/steering.go:70）/ `drainInputs`（agent/steering.go:155） |
| `error` | 1 | — | 兼容保留的终态标记；本版 agent 从不写入，但重放接受它关闭 run（agent/persist_replay.go:321） | — |

设计取舍是「**逐调用显式持久化**」（docs/DESIGN.md:92）：不配 `WithStore` 就零开销——不生成线程 ID、不触碰任何持久化路径，单一执行路径不变。被否决的替代是框架式自动持久化（所有 Run 默认落库）：那是 OpenAI Agents SDK / LangGraph 的取向，但 go-agent 的定位是可嵌入内核，「是否持久、持久到哪个介质」是每个调用点独立的应用决策，且自动持久化会让最简单的用法背负 I/O 语义。

### 1.2 生命周期状态机与 durable-before-visible

重放器 `replayRecords`（agent/persist_replay.go:146）把日志折叠成 `threadView`（:23：system/systemSet、runID、history、head、runActive、cancelled、lastRound、open、cfg、queue），并强校验记录间的生命周期约束：`run_started` 不得嵌套、`round_declared` 必须链上 `runID/lastRound+1` 且工具调用 ID 非空不重复、`round_committed` 必须逐位配对 open declaration（`validateResultBatch`，:364）、`input_consumed` 只能出现在 drain 窗口内、`run_closed`/`done` 必须无 open declaration……违反即 `ErrIncompatibleLog`（agent/persist_dto.go:18）——**日志是协议，不是可尽力解释的素材**：一条自相矛盾的日志宁可拒绝重放，也不做 best-effort 猜测。

每次运行的全部写入都服从 durable-before-visible 纪律（docs/DESIGN.md:94，00-overview.md §3 交接点 4）：

| 动作 | 先落库 | 后可见 |
|------|--------|--------|
| 工具轮 | `declareRound`（agent/agent.go:662） | 任何 ToolCallEvent 宣告、审批问询、执行 |
| 工具轮收尾 | `commitRound`（第 11 篇 §4.1） | 下一轮 provider 请求 |
| 压缩 | `checkpoint`（agent/agent.go:788） | `CompactionEvent` |
| 自然终态 / maxIter / FinishRound-End | `finishRun`（agent/agent.go:585）/ `truncateRun`（:621）/ `closeRun`（:581） | `DoneEvent` |

含义：消费者看到任何事件时，支撑它的持久事实已经在日志里。落库失败直接中止当轮（`persistence` 的注释原话 "the model never advances past a write that did not land"，agent/persist.go:15-17）——宁可停，不可内存视图越过持久视图。

### 1.3 checkpoint：只加速，不承载 correctness

**定位裁决。** checkpoint 是「可重建加速器，定位在自身 Seq……不是生命周期真相，也不承担文件系统回滚」（docs/DESIGN.md:98）。正确性（一条可重放、TC/TR 配对完整、system 冻结的合法历史）完全由 append-only 日志本身保证；checkpoint 丢失时 `replayThread` 经 `History(ctx, thread, 0)` 全量重放（agent/persist_replay.go:86-131），重建出压缩**前**的完整历史——语义等价，只是更长、更费 token。**损失的最多是压缩成果，永远不会是正确性。**

这条定位是被评审逼出来的。2026-09 Store 设计阶段的简报曾主张「checkpoint 是正确性必需品——没有它，重放无法跨压缩边界恢复」；对抗评审推翻了这个叙事：生命周期记录链（run_started → declared → committed → 终态）本身足以重建全部状态，checkpoint 携带的只是压缩后的视图。最终落进 DESIGN.md 的表述就是现在的「可重建加速器」。

由此推出四条具体规则：

1. **重放不重跑压缩器。** 摘要压缩是非确定性的（依赖当次模型输出），`checkpoint` 落库的注释写明目的就是 "so replay never re-runs a nondeterministic summarizer"（agent/persist_util.go:14-17）。快照损坏时回退全量日志，得到压缩前的原始历史，而不是重新调一次摘要。
2. **快照必须自带生命周期上下文。** checkpoint 会遮住它之前的全部记录，run 是否 active、runID、lastRound、冻结 system 若不随快照携带，尾部重放就看不见它们（`agentCheckpoint` 注释，agent/persist_dto.go:216-219）；控制面（生效配置、待消费 steering 队列）同理随快照投影（`Config` / `Queue` 字段，:220-229）。
3. **写入时机受限。** 只在 active 且无 open declaration 的压缩边界写（docs/DESIGN.md:98）：快照必须是一个尾部日志能直接续上的状态；取消结算后不新增快照——否则一个较新的 checkpoint 可能让旧 reader 跳过 `run_cancelled` 记录，把已结算的 run 又看成 active。
4. **ID 绑定未来位置。** `cp-<head>`（agent/persist_util.go:46）以写入时的 expected head 命名，重试时 expected 不变、字节不变，配合 Store 的全内容幂等（第 30 篇）解决「写没写成功不确定」的重试。

版本纪律（§5 展开）：未知 schema/codec_version 是**类型化兼容错误**，不允许「结构回退顺带吞掉新版本」；已支持版本的解码/结构失败才降级为全量回放。

### 1.4 为什么不用别的记录形态

**被否决：可变状态快照（每次轮写覆盖）。** 崩溃窗口里「改到一半」的状态无法区分新旧，恢复语义无从谈起；append-only 让每个崩溃点都对应日志的一个合法前缀，恢复 = 重放这个前缀。

**被否决：多记录事务式写入。** Store 的 `Append` 支持一批多条记录，但 JSONL 后端不把它当断电事务（docs/DESIGN.md:122）；所以关键修复被压进**单条**记录：`run_cancelled` 有 open declaration 时，声明消息 + 全部 unknown 结果 + run 关闭在一个 payload 里原子落库（§2.4），半行断电最多丢掉整条记录，不会出现「修了一半的配对」。

## 2. settle 结算机制

### 2.1 问题：中断留下未闭合 run，谁来解决

错误、ctx 取消、消费者断开都默认**保留可恢复的未闭合 run**，绝不写假的 Done（docs/DESIGN.md:103）。挂起的 run 挡住新输入：`Run`/`RunStream`（`WithThread`）在 run 未终结时拒绝任何新输入，`ErrRunIncomplete`（agent/persist_id.go:27）。三个出口：

- `ResumeThread`——继续原输入跑完（§4.4）；
- `SettleThread`——用原 token 显式结算为 cancelled，然后接受新输入（本节）；
- 什么都不做——线程保持可恢复，随时回来 resume。

**被否决：错误时自动结算。** 普通错误/ctx cancellation 不自动产生 `run_cancelled`（docs/DESIGN.md:96）。理由有三：执行者可能马上 resume，自动结算会把本可继续的 run 替换成 outcome unknown 的合成交果；「取消」必须是宿主的显式决定（取消的语义是放弃，内核无权替宿主放弃）；结算与接受新输入是两个独立的 durability 边界（agent/settle.go:57、docs/DESIGN.md:103），自动结算会把「放弃旧任务」和「开始新任务」压成一个不可分割的动作，而宿主往往要在两者之间做自己的事（审计、通知、清理子线程）。

**被否决：结算 = 发送取消信号并等待。** `SettleThread` 的注释钉死边界：「Settlement neither signals cancellation nor waits for work」（agent/settle.go:49-57）。取消传播是 ctx 的职责（第 11 篇 §2.6 的排空语义）；结算是一个纯日志操作。把两者合并会让 SettleThread 阻塞在不可控的工具排空上，且并行 worker 尚未 join 时写下的结算会与后续 commit 竞争。宿主的清理顺序因此固定：**先等执行者退出（拿到退出 token），再用新的有界 ctx 结算**。

### 2.2 ownership：每线程全局非阻塞运行所有权

持久化入口的第一步永远是 `acquireThread`（agent/persist_id.go:101）：以 `(store.Store, threadID)` 为键在进程级 `threadOwners` 注册表（:96）上做 `atomic.Bool` CAS。三条语义：

1. **非阻塞冲突。** 第二个并发运行在任何模型调用**之前**就得 `ErrThreadBusy`（:26）。被否决的替代是阻塞等待（把并发排队化）：那是串行化不是冲突检测；被否决的另一种是只靠乐观版本号：两个 run 会各自完成模型调用后才发现 Append 冲突——费用已发生、事实已不可收回（:17-20 的注释原话）。所有权必须在花钱之前生效。
2. **按（Store 实例, 线程）全局登记，不按 Agent 实例**（:20-22）：两个 Agent 共享一个 store 也不许把同一线程竞进 provider（`TestSettleOwnership/E46_shared_store`，agent/settle_ownership_test.go:45）。Store 值必须可比较（做 map 键），否则 `ErrStoreNotComparable` 而非 panic（:34，两个内置后端都是指针式）。
3. **表项永不删除**（:86-90）：删除有 ABA 竞争——一个持有已摘下布尔值的等待者可以对新来的 run 赢得 CAS。接受的代价是每个跑过的线程留下一个固定大小条目，随线程基数增长，不随运行次数增长。

`SettleThread` / `SettlementTarget` / cancelled 快照读取与 Run/Resume 使用**同一**所有权（`TestSettleOwnership` 的 E25/E26/E27/inspection 四个子测试：结算进行中，一切并发操作全部 `ErrThreadBusy`，不等待、不抢占）。

### 2.3 SettlementToken：必须在释放前捕获

```go
type SettlementToken struct {          // agent/settle.go:13
    ThreadID     string
    RunID        string
    ExpectedHead int64   // 最后确认的 durable head
}
```

token 绑定「哪个线程上的哪个 run，在哪个修订号」。它**不是授权令牌**（settle.go:10-12 注释），而是防误伤的身份绑定：结算时必须同时匹配重放出的 runID 与当前 head，匹配不上就 `ErrRevisionConflict`，绝不自动刷新。

**为什么必须在释放 ownership 前捕获。** 释放后，线程上可能已经站着**另一个** run（后继 run_started 已落库、甚至已中断挂起）。此时从 `Latest` 取「当前 run」拼 token，等于把「取消 A」的旧请求偷换成「取消 B」——迟到请求不该有改写后继的能力（docs/DESIGN.md:101）。token 的两个交付通道因此都有精确的时序：

1. **错误通道。** `wrapPersistentRunError`（agent/persist_error.go:21）在错误交付给调用方时同步读 `sess.settlementToken()`——此刻 session 仍持有 ownership，`p.head` 是最后确认的 durable head。错误以 `RunInterruptedError{ThreadID, Err, Settlement}` 包装（:7-19），保留 `errors.Is/As` 透因。注释同时警告：一次不确定的写入可能让该 head 落后于真实日志（E10 的场景），所以调用方必须**等 run 退出后**再用它。
2. **退出回调通道。** `WithRunExitFn(fn)`（agent/settle.go:23）在每个成功建立的 session 收尾时交付 token。时序由 defer 链保证：`executeSession`（agent/persist_api.go:153）进入即 `defer runExitFn(sess.settlementToken())`，它比 `threadSession`（:141）的 `defer sess.release()` 后注册、先执行——**回调运行时 ownership 仍在手上**。`TestSettleStreamExit/E51` 钉死回调内重入线程操作得 `ErrThreadBusy`。回调契约：只保存值，不重入、不等待（settle.go:19-22）；early-break（无错误）同样交付。

`openSession` 成功之前的 setup 错误（busy、不兼容日志、生成 ID 冲突耗尽）没有 session、没有 token、不回调（`TestSettleBeforeSession/E29`，agent/settle_boundaries_test.go:50）——这种情况下唯一合法动作是 `SettlementTarget` 重新侦察。

### 2.4 SettleThread：裁决算法与单条原子取消记录

```
acquireSettlement（store≠nil / ID 合法 / ctx 未取消 / acquireThread）
  → token 自检（RunID 非空、ExpectedHead≥1，否则 ErrRevisionConflict）
  → replayThread
  → v.head > ExpectedHead ？
        是 → confirmSettlement：检查 ExpectedHead 处是否已躺着同一 RunID 的
             run_cancelled；是则幂等确认（只读+幂等 Append），否则冲突
  → v.head ≠ ExpectedHead 或 v.runID ≠ RunID → ErrRevisionConflict
  → !v.runActive → ErrNothingToSettle
  → cancelRecord() 以 expected=ExpectedHead 单条 Append
```

（agent/settle.go:58-87。）`cancelRecord`（agent/persist_replay.go:62）是纯函数：不改 view、重复调用字节不变（`TestSettleCodecFinitePrefixAndPureRecord`）。有 open declaration 时，payload 同时携带 `run_id`、`open_round` 与全部 `outcome unknown` 结果（`unknownResults`，:51，对每个声明调用合成一条 role=tool、IsError 的 "outcome unknown" 消息）；无 open declaration 时 `open_round=null`、`results=[]`。重放侧 `applyCancellation`（agent/persist_util.go:103）逐项校验：结果数必须等于声明数、每条必须是该声明调用的 canonical unknown 形态——错 run/round、错误配对、额外内容、重复终态都是 `ErrIncompatibleLog`（docs/DESIGN.md:105-128）。

结算**不做**的事（docs/DESIGN.md:96 的原话清单）：不执行工具、不产生 Done、不伪造 assistant 最终回复、不回滚外部副作用、不改写已提交结果（`TestSettleE32PreservesEarlierCommittedRound`：前一轮的真实 committed 结果原样保留，只有 open 轮补 unknown）、不在结算后新增 checkpoint。**被否决：合成一条假的 assistant 回复把对话收尾。**「模型没说过的话」一旦入库就是永久的伪造历史；unknown 结果是诚实的——它告诉模型「结果不可知」，让模型自己决定重试还是放弃。

### 2.5 幂等重试与不确定写

Store 的 Append 是全内容幂等的：同 ID+同字节的重试被识别为已应用（第 30 篇）。结算据此获得三类重试语义（`TestSettleInterruptRetry`，E14/E15/E16）：

| 场景 | 首次调用 | 重试同一 token |
|------|----------|----------------|
| E14 已确认成功 | 成功 | `confirmSettlement` 幂等确认，不追加 |
| E15 ack 丢失（写已落地、错误已返回） | 错误 | `v.head > ExpectedHead` 分支 → 确认原取消记录，字节不变 |
| E16 写前失败 | 错误，日志未动 | 以原 ExpectedHead 重发同一字节内容 |

E16 里首试失败后线程仍 `ErrRunIncomplete`（未结算不解锁新输入）；E39（`TestSettleCancellationContext`）补齐对称场景：结算自己的 ctx 在 Append 返回后取消，调用方看到错误但记录已落——同一 token 重试即确认。**被否决：重试时刷新 token 的 revision。** 每次重试必须用原 ExpectedHead（测试断言 `fault.revisions` 全等于 token 值）；刷新 revision 等于接受「head 已被别人推进」的事实，那正是冲突该报出的时刻。

「不确定写」的另一半在**执行者**侧：E10（checkpoint ack 丢失）里 token 停在最后确认 head，而日志已前进——旧 token 结算得冲突而不是猜测；出路是 `SettlementTarget` 重新侦察。生成线程的 `run_started` 保留同样采用「字节同一重试 + 幂等采纳」：写后报错时，重试被 Store 识别为已应用并采纳同一 run 继续跑（agent/persist.go:146-153）；全部尝试都失败则错误里带回 runID，调用方仍可 `ResumeThread`——「no error path can strand an active run on an undiscoverable thread」。调用方命名线程的对应场景是 E30：`run_started` 落库但报错，run 失败，`SettlementTarget` 照样能在 head=1 侦察到它并结算。

### 2.6 非递归的父子结算

`SettleThread` 只结算指定 Agent/Store 上 token 绑定的那一个 run，绝不递归（docs/DESIGN.md:130-144）：

- 父结算不关闭或删除子线程；子结算不向父 declaration 提交结果。父未提交的 child 工具调用在父结算时仍是 outcome unknown——**不根据子线程的成功/取消伪造父已提交结果**；子悬挂调用由子线程自己配对。
- 嵌套场景的错误链：嵌套 `RunInterruptedError` 的外层 token 属父、内层属子；一次 `errors.As` 收不齐所有 child，宿主必须逐层收。agenttool 的 child 经 `agenttool.Config.RunOptions`（只支持 `agent.WithThread("")`，agenttool/agenttool.go:33-44）每次 Execute 生成一条独立持久线程，这是父子结算链依赖的形态。
- 宿主清理顺序：等所有 child owned work 与父调用/iterator 退出 → 保存各自退出前捕获的 token 与对应 Agent/Store → **从子到父**显式清理 → 父结算确认后才开始新输入。任一 child 清理失败不得报告整体已清理，也不得改用它的最新 run 身份重试（E47/E48 钉的就是「迟到 token 不得改变后继」）。
- R07 不提供跨 Store 事务、durable 委派关系或匿名 child 的跨崩溃发现：进程在 child token 交付前崩溃可能留下无宿主索引的 incomplete 子日志，宿主必须标记清理未确认并自行发现；最小 one-shot child 可以干脆不配 Store。

**被否决：递归结算（父取消连带关闭全部子线程）。** 子线程是独立的持久实体，有自己的 ownership 与 token 身份；父对子的了解只到「某次工具调用」为止，父结算时子可能早已完成、被别的宿主接手、或根本不该取消。递归等于让父凭一次过期的观察替所有子做终态决定——正是 §2.3 防误伤设计要排除的事。

### 2.7 SettlementTarget：只读侦察，不是刷新器

`SettlementTarget(ctx, thread)`（agent/settle.go:34）在 ownership 下重放、检查当前 active run、释放前形成完整 token；无 active run 返回 `ErrNothingToSettle`。它是「新的显式恢复决策」入口——崩溃或 E10 类不确定写之后，宿主用它看清当前挂起的到底是哪个 run。它**不能**用来把一条旧的「取消 A」请求刷新成取消当前 run（settle.go:31-33 注释）：那是换目标，不是恢复决策。

## 3. 中断与恢复语义：逐场景

### 3.1 共同纪律

中断的三种来源——执行错误、ctx 取消、消费者 early-break——共同遵守：

1. **错误一律带凭据。** session 建立后交付的每个非 nil 错误都包装 `RunInterruptedError`（含 token）；early-break 无错误，但退出回调仍交 token。
2. **内存视图与持久视图分歧时，持久视图获胜。** 已交付的事件不撤回，但恢复/结算只看日志（第 11 篇 §4.3：c1 的 TR 已交付、未 commit → 恢复后 c1 也是 outcome unknown）。
3. **结算必须等执行者退出。** 排空期间一切 settle/settlement 尝试得 `ErrThreadBusy`；退出回调在最后一名 worker join 之后才运行（E06）。
4. **结算与快照零执行。** settle、cancelled 快照读取都不调用 provider、工具、审批、compactor、overlay（各测试的 `p.index` / `worker.calls` 断言）。

### 3.2 场景表

E 编号是 settle 测试子测试内嵌的场景 ID（源自 R07 证据清单），可直接 grep。每行给出：中断时点的 durable 状态 → 可观察行为 → 恢复路径。

| 场景 | 中断时 durable 状态 | 行为 | 恢复 | 权威测试 |
|------|--------------------|------|------|----------|
| E01 provider 回复前取消 | `run_started` | `RunInterruptedError(ctx.Canceled)` + token；无任何模型内容落库 | settle → 快照仅含 user 输入；随后新输入即新 run（重定向） | settle_interrupt_provider_test.go:14 |
| E02 provider 流中途取消（text/reasoning/半截 tool call 已 yield） | `run_started` | 同 E01；已 yield 的部分内容**不**落库（测试断言快照仅 user 消息） | 同 E01 | 同上 |
| E13 同一 ctx 二次 cancel | 同 E01 | 幂等，无额外效果 | 同 E01 | 同上 |
| E03 工具执行中取消 | `run_started` + `round_declared`（open） | 工具按 ctx 退出，run 终止于 `ctx.Err()` + token；0 commit | settle 将 open 声明配对全部 unknown；或直接 `ResumeThread` 续跑 | settle_interrupt_tools_test.go:16 |
| E04 并行部分结果已交付（fast 完成、slow 在飞） | 同 E03 | fast 的 live 结果只在事件流里，未 commit | settle/Resume 后**全部**声明调用（含 fast）= outcome unknown（纪律 2） | 同上 |
| E11 审批等待中取消 | 同 E03 | 审批回调随 ctx 返回；0 执行、0 commit | 同 E03；结算不重复问审批 | 同上 |
| E06 并行多 worker 在飞 | 同 E03 | 停止派发、排空在飞 worker；最后一名 join 前不交 token、不释放（探针 settle → `ErrThreadBusy`） | settle → c1/c2 均 unknown | settle_interrupt_parallel_test.go:14 |
| E07 并行槽位全 resolved、commit 已落库后取消 | `run_started`+`declared`+`committed` | 终态 `ctx.Canceled` + token；真实结果已持久（commit 用 `context.WithoutCancel`，第 11 篇 §4.2） | settle 追加**空**取消（无 open）；快照保留真实结果（"real success"/"real rejection"），committed 前缀逐字节不改写 | settle_interrupt_commit_test.go:29 |
| E05 串行 commit 后、下一 provider 调用前取消 | 同 E07 | 同 E07 | 同 E07 | 同上 |
| E08 压缩进行中取消 | `run_started`（无 checkpoint） | 错误是 `Canceled` 或 `ErrCompactionBudgetExceeded`（压缩失败非致命降级，第 13 篇），仍带 token；压缩器恰好调了一次 | settle → cancelled；快照 = 压缩**前**历史 | settle_interrupt_compaction_test.go:16 |
| E09 checkpoint 已落库、随后流失败 | `run_started` + `checkpoint` | 错误 + token（head 含 checkpoint） | settle 在 checkpoint 之后追加 `run_cancelled`；快照 = 压缩后视图 | 同上 |
| E10 checkpoint 写入不确定（ack 丢失） | 日志实际已到 head=2，session 确认 head=1 | token.ExpectedHead 停在 1；旧 token 结算 → `ErrRevisionConflict` 且日志不动 | `SettlementTarget` 侦察（head=2）→ 新 token 结算 | 同上 |
| E12 round-context 回调失败（第 2 轮 overlay） | 第 1 个工具轮已 `committed` | 错误（含取消变体）+ token；overlay 是瞬态请求层，从未入库（快照与日志都搜不到） | settle/Resume；committed 结果保留 | settle_interrupt_callback_test.go:16 |
| E37 maxIter 预闭合中断（写入在 declaration/committed/done 三点失败） | 截断于 `truncateRun` 序列中间 | run 以写入错误终止 + token | done 已落 → `ErrNothingToSettle`（终态不可重开）；committed 后 → skipped 结果保留（**确定未执行**）；仅 declaration 后 → unknown（不确定） | settle_terminal_test.go:12 |
| E31/E32/E33 消费者在首个 ToolCall/ToolResult/Text 事件处 break | text → `run_started`；call → `declared`（open）；前轮已 commit 的保留 | 无错误（break 不是错误）；`WithRunExitFn` 在 unwind 后、释放前交 token | settle → open 轮配 unknown（E32：前一轮真实结果原样保留）；或 Resume 续跑 | settle_stream_test.go:16、settle_stream_committed_test.go:12 |
| E34 匿名生成线程上 break | 同 E31 | 生成线程同样是持久 session，退出回调可拿原 token（匿名 ≠ 非持久） | 同 E31 | settle_stream_test.go:16 |
| E35/E36 Done / maxIter Done 交付后 break | 终态已完整落库 | 回调交 token，但 `SettleThread` → `ErrNothingToSettle` | `ResumeThread` → `ErrNothingToResume`（终局轮预闭合，第 10 篇） | 同上 |

maxIter 终局轮的例外在第 10/11 篇已述：skip 批次在首个 terminal 事件前已完整 durable 预闭合（declaration + skipped results + commit + Done），consumer break 只停止自己的交付，不可能重开已关闭的 run。

### 3.3 两条恢复路径

**ResumeThread（继续）。** 重放发现 open declaration 时，`closeUnknown`（agent/persist_replay.go:43）把声明消息并入历史、对每个声明调用合成 unknown 结果，并以**原 run 的记录 ID**（`<runID>-c<round>`）补一条 recovery `round_committed`（agent/persist.go:78-92）——日志保持 canonical，不需要第二次合成。绝不盲目重执行工具（:38-40 注释）：unknown 是给模型的信号，模型自己决定重试哪个调用。恢复后 `roundBase = lastRound+1`（agent/persist.go:92），轮号延续 durable 序号；不追加新 user 消息——继续的是原输入。

**SettleThread + cancelled 快照（收档）。** 结算后 `ResumeThread` 返回只读快照 `RunResult{Cancelled:true, ThreadID, History}`：不执行模型、工具、审批、overlay、compactor、回调，不写日志，可重复读取（E19）；Message/ToolCalls/Usage 均为零值——它不代表旧任务成功，也不代表历史费用为零（docs/DESIGN.md:99）。快照的历史永远配对完整（`partitionHistory` 校验）。

E44/E22（settle_lifecycle_test.go:113）钉住恢复目标的唯一性：ResumeThread 永远针对**最后一个**未终结 run——原 run 未结算则续原输入（E44）；已结算且新 run 也中断挂起，则续新输入（E22）。

### 3.4 重定向（redirect）：结算之后才接受新输入

E20/E45：不结算时，`ErrRunIncomplete` 对**任何**新输入成立——包括与原输入逐字节相同的输入；「提交一样的输入」不能被当成恢复。E18/E21：结算与「新输入已被持久接受」之间存在独立窗口——`run_cancelled` 落库到下一条 `run_started` 落库之间，日志里没有也不需要有新输入的痕迹。结算完成后，带新输入的 `WithThread` 调用开新 run：模型请求的第一条历史就是新输入（E01 尾部的重定向断言：provider 第二次调用收到的 messages = 旧输入 + 新输入的完整历史）。

## 4. WithThread / ResumeThread：API 语义

### 4.1 每调用 RunOption 的设计

持久化挂在 `RunOption`（agent/persist_api.go:14）上——与构造期 `Option` 独立的**逐调用**选项类型：同一个 Agent，这一次调用走线程、下一次临时跑。「线程」是调用点属性（今天这个任务要不要可恢复），不是 agent 实例属性。`runConfig`（:42）每次调用新建，不修改 Agent、不与并发调用竞争；`newRunConfig`（:50）按序应用选项，重复 `WithThread` 后者胜（与构造选项同一覆盖顺序）。被否决的替代：构造期一次性绑定线程——一个 Agent 实例就被焊死在一个线程上，多线程路由（agenttool 的每次 Execute 新线程）没有自然的表达。

`runSequence`（:63）是唯一分岔点：无 `WithThread` → 临时运行直连 `runStreamInternal`（store 配置了也不碰）；有 → 经 `threadSession` → `executeSession` → `runStreamInternal`，单执行路径不变量保持（第 10 篇）。`ErrNoStore` 在迭代器存在之前同步返回（:68）；`validateThreadID` 同样前置（ID 非空、`url.PathEscape` 后 + ".jsonl" ≤ 255 字节，agent/persist_id.go:60-68——ID 永不规范化）。

### 4.2 WithThread 三态

| 形态 | 语义 |
|------|------|
| 缺省 | 临时运行：不触碰 store，`ThreadID` 恒为 "" |
| `WithThread("")` | 内核生成 crypto-random 128 位线程 ID（`t-<32hex>`，agent/persist_id.go:74-84），经 `DoneEvent`/`RunResult` 暴露 |
| `WithThread("t-1")` | 续接或创建：head=0 即新建（无「线程不存在」错误）；后续调用重放冻结 system（`systemSet` 连空提示词也算已冻结，`WithSystemPrompt` 改配置不能篡改历史上下文，agent/persist_replay.go:169-171、agent/persist.go:98-100） |

run 未终结时新输入 → `ErrRunIncomplete`（agent/persist.go:96）：错误文本指明两个出口——Resume 或用原 token 显式结算。`RunWithHistory` / `RunStreamWithHistory`（agent/agent.go:864/:879）永远非持久：一次性导入语义，不入线程、不交付退出 token、不能借结算隐式导入（`TestSettleHistoryEntrypointsStayNonpersistent`）。

### 4.3 生成线程的 rev=0 预留

`reserveGeneratedThread`（agent/persist.go:123-171）：先 `Latest` 确认 head==0，再以 expected=0 追加 `run_started`。冲突即「这个随机 ID 已命名一个线程」→ `reservationConflictError`（agent/persist_id.go:41），调用方**换新 ID 重试**（`runOnNewThreadStream`，agent/persist_api.go:178-205，至多 3 次），绝不污染既有线程；保留成功之后的冲突意味着真实并发写入，run 停止而非重启。重试采纳有两条护栏：一是只接受**无** `RunInterruptedError` 包装的 `ErrThreadBusy`/保留冲突（session 已建立后的失败不能当重建信号，`errors.As` 排除，agent/persist_api.go:188-197）；二是重试发生时无事件交付、无模型调用，不可能重复或丢失任何东西。**被否决：生成后先读再写、不设预留。** 随机 ID 碰撞概率极低但不为零；rev=0 Append 是唯一的原子「不存在性检查 + 创建」，读-写两步在跨进程下有 TOCTOU 窗口（v1 契约单进程，但写路径仍按可扩展形态收敛）。

### 4.4 ResumeThread 的完整分支

`ResumeThread`（agent/persist_api.go:105-140）按序：store/ID/ctx 检查 → acquire ownership → replay → 分支：

- `v.cancelled` → **只读 cancelled 快照**（§3.3），不进入执行。
- `v.runActive` 且有 open → `closeUnknown` + recovery commit（§3.3）后进入执行：`roundBase = lastRound+1`，历史 = 冻结 system + 重放历史，**不追加新输入**。
- 无 active run → `ErrNothingToResume`（agent/persist_id.go:28）：无记录、正常 Done、maxIter Done、兼容 `KindError`、合作式 `run_closed`——都是真终态，无事可续。

### 4.5 ThreadID 的暴露面

`RunResult.ThreadID`（agent/agent.go:272-274）与 `DoneEvent.ThreadID`（agent/event.go:116-118）同名同义：持久线程的 ID，临时运行为 ""。`foldRunStream` 从 DoneEvent 透传（agent/agent.go:362）；三个 DoneEvent 发射点（自然终态 :597、maxIter :653、FinishRound-End :717）与 cancelled 快照都携带；执行中的 `RoundContextRequest`（:471-474）与 `FinishRoundContext`（:544-548、:684-688）同样拿到 ThreadID。宿主因此总能在运行结束后拿到线程 ID 持有引用；但 ThreadID 本身**不是**结算凭据（§2.3）——没有原 token 时只能走 `SettlementTarget`。

### 4.6 吸收协议：与控制面写者的共存

`SetThreadConfig` / `EnqueueInputWithID` 是外部写者，会在 runner 持有 ownership 期间向日志追加记录。每个 runner 写点（declare/commit/checkpoint/终态）Append 冲突时执行 D6(a) 吸收（agent/persist.go:178-203 的 `append` 重试环）：重读尾部长窗，把白名单内的 `config_changed` / `input_queued` 折叠进 session 投影，再在真实 head 重试；白名单外的记录不可吸收、响亮失败（`absorbTail`，:214-254）——runner 绝不越过一条自己没写的记录。轮顶的 `refreshConfig`（:205）做同构扫描。写入侧与控制面语义见第 14 篇；本篇只确立边界：**runner 的乐观 head 与外部控制面写者通过吸收协议收敛，其余一切并发写入都是冲突**。

## 5. DTO 与版本演进

### 5.1 为什么必须显式 DTO

密封接口（`AgentEvent` 七变体）没有导出判别符、部分变体内嵌 error，直接 JSON 序列化不可逆。持久化因此走显式版本化 DTO：`eventEnvelope{Type, Version, Payload}`（agent/persist_dto.go:28，`eventEnvelopeV1 = 1` :22）当前只承载 `done`；生命周期记录各有专属 payload 结构（§1.1 表）。这层「麻烦」换来的是落库格式的显式契约与向前演进的空间（00-overview.md §5.1）。

### 5.2 严格解码纪律

`strictDecode`（agent/persist_dto.go:285）对 payload 开 `DisallowUnknownFields` 并拒绝尾随数据。对照面很清楚：`llm.Message` 的解码器**故意**宽松（忽略未知字段，厂商兼容），若持久化 payload 也这样解，新版本写入的字段会被旧 reader 静默丢弃——数据损坏披着兼容外衣。所以不透明的载荷要解成具体 wire 结构再转（`decodeRunCancelled`，:91-126，注释原话 "Decode concrete wire blocks here"：若直接解 `llm.Message`，宽松解码会把 strict 检查架空）。`decodeConfigChanged` 在解码侧镜像写侧规则：provider 非空必须带 model，畸形即 `ErrIncompatibleLog`——「一个合规写者永远不会写出这种形态，读到就说明日志被手改过，fail-closed 不猜」（:133-152）。

向前兼容的伴生技巧：`configChangedPayload.Provider` 用 `omitempty`（:121-126）——不切换 provider 的记录与旧版本逐字节相同，旧二进制照常读；一旦出现 provider 字段，旧二进制 strict-decode 直接报错而不是误读。

### 5.3 版本矩阵与演进史

| 版本 | 引入内容 | 演进规则 |
|------|----------|----------|
| SchemaV1 | 生命周期主体：run_started / round_declared / round_committed / agent_event / error / checkpoint(codec 1) | 旧日志永不迁移；unset schema 归一为 1 |
| SchemaV2（R07） | `run_cancelled`（与取消结算一揽子）、`config_changed`、`input_queued`、`input_consumed`、`run_closed`、checkpoint codec 2 | store 侧强制：这五种 kind 的 schema≠2 直接 `ErrUnsupportedSchema`（store/store.go:228） |
| SchemaV2 内扩展（provider 热换） | `config_changed.provider` 字段 | `omitempty` 字节同一性 + 旧 reader strict-decode fail-loud（§5.2）；checkpoint codec 不动，`configState` 加 Provider 字段对旧快照零影响 |
| SchemaV3（控制面） | checkpoint codec 3：新增 `config` 投影与 `queue` 投影 | pre-v3 快照**不被信任**：重放只回放其尾部会静默丢掉被快照遮住的 `config_changed`，所以整体回退全量日志（D9 规则，agent/persist_replay.go:100-104） |

未来版本的待遇分两级：**未知 schema、未知 kind、未知信封版本、未知 codec_version 是类型化兼容错误**（`ErrUnsupportedSchema` / `ErrIncompatibleLog`），绝不静默忽略——旧二进制必须大声报错，不许借 `KindError` 或 checkpoint 隐藏新终态（docs/DESIGN.md:121-124）；**已支持版本的解码失败 / 结构损坏**才降级为「丢弃快照、全量回放」（`decodeCheckpoint` 返回 `nil, nil`，agent/persist_dto.go:246-269）。两者不可混：不能让「未来版本」借结构回退的通道溜过去（`checkpointVersion` 在快照不被使用时也校验版本，:233-244；`TestSettleCodecRejectsFutureVersions`）。

反向兼容同样受测：新 checkpoint（codec 3）必须被**旧版严格解码器拒绝**而不是被当成 v1 快照误读（`TestSettleCodecCheckpointCannotMasqueradeAsV1`）——新字段 + codec_version 标记保证旧 reader strict-decode 失败、回退全量日志。

### 5.4 replay 容错：三级降级

1. **快照可用** → 以快照为基座只重放尾部（`Latest` 的 Tail）。
2. **快照不可用（损坏 / 不被信任 / pre-v3）** → `History(ctx, thread, 0)` 全量重放；回退只重放 `Latest` 快照覆盖的前缀——快照之后的记录属于吸收协议的领地，在这里折叠会把队列投影对上陈旧 head（H1 规则，agent/persist_replay.go:113-118）。
3. **日志本身矛盾** → `ErrIncompatibleLog`，没有第三级。

结构校验不止于解码：`checkpointSystemMatches`（:346）验证快照的 system 前缀与冻结提示词完全一致（空提示词 = 无前缀消息；否则恰好一条单 text block 的 system 消息）——多一条 system、多一个 block、文本不同，都算快照损坏，回退全量。

物理损坏的容错在单条记录粒度：JSONL 半行（断电撕裂）由后端修复，重放停在断点——`run_cancelled` 写了半行时，线程呈现为「open declaration 待解决」而不是「取消了一半」，settle/Resume 仍可用（`TestSettleCodecTornCancellationLine` 对 0/1/半行/缺尾各切点断言）。这正是 §1.4「修复压进单条记录」的回报。

### 5.5 字节同一性与幂等

一切重试语义的地基是「不可变批次与 ID 只生成一次，重试逐字节同一」（docs/DESIGN.md:101）：`recordID(runID, suffix)` 派生稳定 ID（agent/persist_dto.go:271，注释：模糊失败的重试必须复现逐字节相同的调用方内容，这是 Store 幂等重试的前提）；`cp-<head>` 绑定未来位置；`cancelRecord` 纯函数、重复调用不变；checkpoint 冲突重试时**从活投影重新序列化**（控制面记录可能刚被吸收，快照必须带上吸收后的配置而不是冲突前拍下的旧配置，agent/persist_util.go:27-30）。Store 侧 `planAppend` 的全内容幂等匹配（ID/Kind/Schema/Payload 逐项相等才接受已应用前缀）见第 30 篇。

## 6. 测试即契约

| 测试组 | 钉住的契约 |
|--------|------------|
| settle_interrupt_provider / tools / commit / parallel（E01-E07、E11、E13） | §3.2 中断场景表：分阶段取消的 durable 状态与恢复路径 |
| settle_interrupt_compaction / callback / retry（E08-E10、E12、E14-E16） | 压缩与回调时点的中断；不确定写与幂等重试 |
| settle_stream / stream_committed / terminal（E31-E37、E51） | early-break 的 token 交付；committed 前缀保留；maxIter 预闭合中断的确定性分级 |
| settle_lifecycle（E18-E23、E44/E45） | 结算后的生命周期：快照只读可重复、重定向窗口、恢复目标唯一 |
| settle_identity（E47-E50、E52、E24） | token 身份：迟到请求不改后继、同 run 前进后旧 token 冲突 |
| settle_ownership（E25-E27、E46） | 全局非阻塞所有权：并发操作全部 busy |
| settle_boundaries（E28-E30、E39 + 合作式终态 + RunWithHistory 非持久） | 公共边界哨兵；setup 失败无凭据；run_started 不确定写 |
| settle_codec_*（6 个文件） | 取消记录编解码、checkpoint 版本矩阵、损坏回退、半行容错、纯函数记录构造 |

## 7. 锚点速查

| 符号 | 位置 | 职责 |
|------|------|------|
| `persistence` | agent/persist.go:18 | 一次 run 的持久化 session（head/roundBase/open/cfg/queue） |
| `openSession` / `prepareSessionLocked` | agent/persist.go:40 / :73 | 预留-重放-开 run 的统一入口；resume 关闭 open 轮 |
| `reserveGeneratedThread` | agent/persist.go:123 | 生成线程 rev=0 预留 |
| `persistence.append` / `absorbTail` / `appendRebaseable` | agent/persist.go:178 / :214 / :256 | D6(a) 吸收协议 |
| `declareRound` / `declareTerminalRound` / `commitRound` | agent/persist.go:303 / :326 / :355 | 工具轮三接触点（第 11 篇 §4） |
| `closeRun` / `truncateRun` / `finishRun` | agent/persist.go:346 / :373 / :391 | 三种终态落库 |
| `checkpoint` | agent/persist_util.go:18 | 压缩快照（`cp-<head>`，重试重序列化） |
| `threadView` / `closeUnknown` / `cancelRecord` | agent/persist_replay.go:23 / :43 / :62 | 重放状态；unknown 合成；取消记录构造 |
| `replayThread` / `replayRecords` | agent/persist_replay.go:86 / :146 | 三级降级重放与生命周期校验 |
| `RunOption` / `WithThread` / `runSequence` | agent/persist_api.go:14 / :32 / :63 | 逐调用选项与唯一分岔点 |
| `ResumeThread` / `threadSession` / `executeSession` | agent/persist_api.go:105 / :141 / :153 | 恢复入口；ownership/token 的 defer 链 |
| `runOnNewThreadStream` | agent/persist_api.go:178 | 生成 ID 的保留冲突重试 |
| `SettlementToken` / `WithRunExitFn` | agent/settle.go:13 / :23 | 结算凭据与退出回调 |
| `SettlementTarget` / `SettleThread` / `confirmSettlement` | agent/settle.go:34 / :58 / :102 | 只读侦察；结算算法；幂等确认 |
| `RunInterruptedError` | agent/persist_error.go:7 | 中断错误包装（携带 token） |
| 哨兵 | agent/persist_id.go:24-34 | ErrNoStore / ErrInvalidThreadID / ErrThreadBusy / ErrRunIncomplete / ErrNothingToResume / ErrNothingToSettle / ErrStoreNotComparable |
| `threadOwners` / `acquireThread` | agent/persist_id.go:96 / :101 | 全局非阻塞所有权 |
| `eventEnvelope` / `strictDecode` / `agentCheckpoint` | agent/persist_dto.go:28 / :285 / :220 | 版本化 DTO；严格解码；快照载荷 |
| `decodeConfigChanged` / `configState` | agent/persist_dto.go:133 / :184 | provider+model 成对校验；生效配置折叠 |
| 循环内持久化调用点 | agent/agent.go:538 / :581 / :585 / :621 / :662 / :788 | declareTerminal / closeRun / finishRun / truncateRun / declareRound / checkpoint |
