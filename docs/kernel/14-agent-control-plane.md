# 14 · 运行时控制面

> 内核文档集第 14 篇。本文讲 `agent` 包的运行时控制面：steering 队列（运行中插话）、配置热更新与 provider 热切换、FinishRound 回合末决策钩子，以及 per-round context provider 在控制面里的角色。契约本文是 docs/DESIGN.md「运行中控制面」小节（docs/DESIGN.md:222-259）与 docs/CONTROL-PLANE-DESIGN.md v3；裁决动机与被否决的备选见 docs/control-plane-decision-record.md——本文不重写它们，只补充代码级的机制视图（每个机制落在哪个文件、哪一行、为什么长这样）。行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），与 10-agent-loop.md 同批。

## 1. 一条原则，三个契约

**凡是运行中来自外部的意图——插话、改配置、叫停——一律走线程日志，在回合边界的 drain 点生效；接受即持久化。**（docs/CONTROL-PLANE-DESIGN.md §0）

这条原则不是口号，而是可验证的承诺：`EnqueueInputWithID` / `SetThreadConfig` 返回成功的时刻，记录必须已在日志里，此后任何崩溃不背叛该承诺。由此推出三个契约共享的不变量：

- **意图先持久化后生效**。与循环的"提交点先落库后行动"纪律（declareRound 先于任何工具宣告/执行，见 10-agent-loop.md §2.3）同构：外部写者与 runner 谁都不允许"先干活后记账"。
- **生效点唯一且在回合边界**。每轮顶部固定顺序 `读配置 → 压缩 → drain → 预算检查 → 请求`（agent/agent.go:415-466，冻结契约 docs/DESIGN.md:232，状态机全景见 10-agent-loop.md §2.1-2.2）。控制面永不打断在飞请求、永不新增事件类型——sealed 七种 `AgentEvent` 不增不减（docs/DESIGN.md:224）。
- **Resume 时日志重放即状态重建**。队列、生效配置都是重放投影，无内存态依赖；`DoneEvent.History` 是消费者看到插话的唯一位置（steering 不发事件，与 `ReasoningItemChunk` 的处理哲学一致）。

支撑这三个不变量的是一个写协议：**外部写者乐观追加、runner 写点吸收（D6-a）**。runner 持有的 head 只是期望值；`Append` 冲突时 `absorbTail`（agent/persist.go:214）重读日志尾，把白名单内的控制记录（`config_changed` / `input_queued`，store/store.go:49-50）折进会话投影后按真实 head 重试，白名单外的记录按冲突失败。协议细节与 checkpoint v3 投影归 12-agent-persistence；本文只在各机制引用它。

为什么是这个形态而不是更宽的钩子面：分层判据是"需要循环内部执行时支持的能力进内核，构造期能表达的不进"（docs/control-plane-decision-record.md §2）——包装器无法向持久化 history 注入消息，也无法在回合边界干净收尾，所以 steering 与 FinishRound 必须进内核；per-tenant 隔离、供应商级包装是构造期的事，明确不进。

## 2. steering 队列（agent/steering.go）

### 2.1 API 形态与 ID 语义

```go
func (a *Agent) EnqueueInputWithID(ctx context.Context, thread, idempotencyKey, input string) (QueuedInputID, error)   // agent/steering.go:70
```

它是 `Agent` 方法而非设计稿早期的包级函数——准入边界需要 Agent 的 contextWindow 语义（docs/DESIGN.md:240）。三个入口错误：`ErrNoStore`（匿名运行不支持 steering，durable 是价值主张本身，内存队列是被否决的退化形态——docs/control-plane-decision-record.md D1）、`ErrEmptyInput`（空 input 或空幂等键，agent/steering.go:21）、`ErrInputTooLarge`（:26）。

**ID 语义：幂等键命名“逻辑消息”，派生记录 ID。** `QueuedInputID = "q-" + hex(sha256(idempotencyKey)[:8])`（agent/steering.go:85-86）。同键重试命中已落库记录即幂等确认——ack 丢失（Append 成功但响应没回来）的场景下，调用方拿同一个键重试，`queuedRecordLanded`（:138）按 ID + kind + schema + payload 全内容比对在日志尾找到它，直接返回成功，不产生第二条。键被 checkpoint 遮蔽时（尾扫描看不见）走 `ErrInvalidRecord` → 全量 `History` 回查（:114-118）。不同键同文本是两条消息：**内容哈希不是操作身份，调用意图才是**（D10 裁决，docs/control-plane-decision-record.md §5——评审第一轮 blocker 正是内容 ID）。这组行为由 TestEnqueueIdempotencyAndDistinctKeys（agent/steering_test.go:240）钉死。

与之对照，`SetThreadConfig` 的操作身份是"每次调用"（§3.2）——两种身份都对，因为 steering 的重试语义是"同一条消息"，配置的重试语义是"再来一次"。

### 2.2 准入与 FIFO：只延后、不跳过、不强注

**准入（enqueue 时）只拒绝物理不可行**：input 以序列化消息计量（`estimateRunes(llm.UserMessage(input))`，与压缩/drain 预算同单位，转义文本无法绕过——agent/steering.go:77-84），超过硬限制即 `ErrInputTooLarge`。硬限制 = `steeringAdmissionRunes()`（agent/agent.go:745）：普通运行是 `contextWindowTokens`，启用 round context overlay 时是 `canonicalContextCap()`（overlay 的预留必须活过 steering 注入，见 §6）。

**drain（消费时）只消费装得下的 FIFO 前缀**。`drainInputs`（agent/steering.go:155）逐条累计序列化成本，`used+cost > availableRunes` 时**break**——队头装不下，队头后面的东西一条也不动，绝不跳过队头去消费更短的队尾（TestSteeringDeferralKeepsFIFOOrderNoSkip，agent/steering_test.go:448；部分前缀的精确形态由 TestSteeringDrainConsumesPartialPrefix :526 钉死：`[[A] [B C]]` 而非 `[A, C]`）。装不下的项留在队列，等下一轮压缩腾出空间重试。

为什么是"延后"而不是别的两种：**force 强注**（压缩后仍硬塞）会把 run 顶过硬限制、或触发 `ErrCompactionBudgetExceeded` 杀 run——评审第三轮删除 force 的原因正是它隐含"压缩必达 target"的假设，压缩禁用/失败时必然出错（docs/control-plane-decision-record.md §5 修正 3）。**丢弃或部分截断**违背"接受即持久化"的承诺。代价是诚实的：已接受的输入可能迟到（容量不足的信号），但绝不会被静默丢掉，也绝不杀 run。队列非空且永远装不下的极端情况，输入就持久地留在日志里——可见、可查、可由宿主处理。

### 2.3 drain 时机：为什么在压缩之后

轮顶顺序是 `读配置 → 压缩 → drain → 预算检查 → 请求`（agent/agent.go:415-466）。drain 槽位选在压缩**之后**是 2026-10-28 的裁决修正（docs/CONTROL-PLANE-DESIGN.md §12-3，owner 提出）：

1. **压缩基于稳定历史决策**。压缩对象是上一圈结束时的 history，若 drain 在压缩前，插话刚注入就参与本轮压缩判定，把"用户插话"当成可压掉的旧消息是语义错误。
2. **新注入天然免于被压掉**。压缩完成后才注入的消息不在压缩视野内，插话必然完整进入本轮请求。
3. **消除一条杀 run 路径**。若先注入后压缩，一条贴近硬限制的插话可能触发压缩预算错误；先压缩后注入配合准入上界，插话永不因自身触发 `ErrCompactionBudgetExceeded`。

代价由 `carry` 字段吸收：drain 注入量超过压缩目标的部分记在 `p.carry`（agent/persist.go:29），下一轮的压缩预算护栏放宽 `carry`（agent/agent.go:819-823）——**护栏只能放宽、不能收紧**，否则会破坏 nil-compactor 的既有契约（无压缩时大历史本应放行；同 docs/control-plane-decision-record.md §5 修正 2）。

drain 自身一次原子落库：一条 `input_consumed` 记录（payload 携带被消费项的 ID+文本，逐项匹配当前队列 FIFO 前缀）先 Append，成功后才从内存队列摘掉已消费前缀（agent/steering.go:179-190）。在 drain 的 Append rebase 期间被吸收进队列的新条目**不被消费**——它们活到下一次 drain（注释在 :187-188，行为由 TestSteeringDrainRebaseKeepsAbsorbedEntry，agent/steering_test.go:88 钉死）。runner 之外谁写 `input_consumed`，重放直接拒绝（TestSteeringConsumedOutsideDrainWindowRejected :166）。

### 2.4 崩溃窗口与重放语义

三个时刻，三种正确性（docs/CONTROL-PLANE-DESIGN.md §2）：

- **入队后、消费前崩溃**：`input_queued` 已在日志里，重放把它放回队列投影（不改 history），下一个 run 首轮 D1 消费（TestSteeringCrashBeforeDrainKeepsQueue，agent/steering_test.go:196）。
- **`input_consumed` 落库后、本轮 assistant 产生前崩溃**：插话已是 history 的一部分，Resume 重放历史含插话、本轮重新生成——at-least-once，模型重新看到插话，语义一致。不存在"已消费但无后续宣告"的孤立态：它就是 history，与 `Run` 的初始输入同性质。
- **正常消费后 Resume**：`input_consumed` 重放恰好一次，不二次注入（TestSteeringConsumedRecordReplaysExactlyOnceOnResume，agent/steering_test.go:345——队列投影已空，不产生第二条消费记录）。

### 2.5 run 边界上的队列语义

run 结束（自然 Done、FinishRound End、maxIter 终局）时队列**不消费、不清点**：残留插话由下一个 run 的首轮 D1 消费（D3 裁决）——"无 active run 时入队"天然成为 follow-up，follow-up 独立队列机制因此被否决（docs/CONTROL-PLANE-DESIGN.md §8）。`DoneEvent` 也不加队列计数字段：日志里的事实自明，宿主可查。钩子里入队的极端时序（FinishRound-End 回调中 Enqueue）同样正确：插话落在 commit 与 closeRun 之间，run_closed 的 Append 冲突后吸收它、在其后关闭，本 run 的请求与 history 都不含它，下一个 run 消费（TestSteeringEnqueuedInEndHookConsumedByNextRun，agent/steering_test.go:386）。

## 3. 配置热更新（agent/config.go）

### 3.1 ConfigChange 与 per-record 原子性

```go
type ConfigChange struct {   // agent/config.go:34
    Model    *string
    Thinking *string
    Provider *string
}
func SetThreadConfig(ctx context.Context, st store.Store, thread string, ch ConfigChange) error   // agent/config.go:59
```

nil 字段 = 不变，非 nil 字段覆盖；多条 `config_changed` 记录按重放序叠加、后写赢（`configState.fold`，agent/persist_dto.go:190）。无任何记录时生效配置 = 构造期 `WithLLMOptions` 对应字段 + 默认 provider（docs/DESIGN.md:228）。

**原子性的准确含义是 per-record**：一条记录内携带的所有字段一次落地、一次生效——尤其是 provider+model 成对（§4.3）。重放永远观察不到半开的配置态：fold 以记录为单位，一条 `provider=glm, model=glm-4.7` 的记录要么整体生效、要么整体不存在。字段域刻意收窄为 Model / Thinking / Provider 三项（温度、max tokens 等构造期表达即可，扩字段 = payload 加列向前兼容）；`Thinking` 是逐适配器如实承载的可选项（openairesponses 忽略它）。

### 3.2 写协议与操作身份：为什么配置不做内容幂等

`SetThreadConfig` 是包级函数（外部写者，不需要 Agent 实例——只做"写侧能做的"语法检查）。每次调用铸造全新随机记录 ID（`newControlRecordID`，agent/config.go:134，前缀 `cfg-`）：**重复一个旧值是新的操作，必须追加新记录**，A→B→A 终态必须是 A（TestSetThreadConfigABAEndsOnA，agent/config_test.go:233）。这是与 steering 幂等键的正面对照：steering 的重试问题是"这条消息发出去了吗"（同键确认），配置的"重试"问题是"我改的主意最终生效了吗"（每次都是新动作）——若用内容哈希做 ID，A→B→A 的第二次 A 会被误判为重复而丢弃，终态错误地停在 B。

写协议与 steering 同构：乐观 Append（bounded 5 次重读 head 重试）；连续 revision 冲突后报 `ErrThreadBusy`（明确“可能已落库也可能没落”的不确定语义，下次调用无论同值异值都正确）；非冲突的 store 错误原样包装返回，不误报 busy（TestSetThreadConfigPreservesStoreError，agent/config_test.go:322）。`ErrEmptyConfig` 拒绝全 nil 调用（agent/config.go:63-65）。

### 3.3 生效路径：轮顶读取，永不中途打断

生效点唯一：**每轮顶部**。`refreshConfig`（agent/persist.go:205）→ `absorbTail` 从上次扫描位折入新 `config_changed` → `sess.cfg.apply(a.llmOpts)` 把生效覆盖追加在基座选项之后（agent/persist_dto.go:204——线程模型机械遮蔽 agent 级 `WithModel`，基座跨厂商选项跟随线程、无抑制规则）→ provider 解析（agent/agent.go:417-437）。本轮的 options 是按轮快照，从不回写 `a.llmOpts`。

三个时序保证：

- **在飞请求不受影响**：变更在下一轮顶部生效，永不在请求中途换模型/换厂商（本轮全部 retry attempt 与流式降级复用同一配置与 provider 实例，agent/agent.go:427-433 的解析在轮顶一次性完成）。mid-run 变更下一轮吸收（TestSetThreadConfigMidRunAbsorbedAtNextRound，agent/config_test.go:56）。
- **run 开始前写入的照样生效**：`run_started` 的 Append 自己也是 rebaseable 写点，冲突时吸收先行落库的 config（agent/persist.go:253 `appendRebaseable`；TestRunStartRebasesOverConfig，agent/config_test.go:306）。
- **重启后仍生效**：配置是日志事实。checkpoint v3 携带 `effective_config` 投影；旧 v2 快照遇新 kind 无条件回退全量重放，不得借旧快照绕过新语义（TestConfigProjectedIntoV3Checkpoint / TestConfigBehindV2CheckpointFallsBackToFullLog，agent/config_test.go:172/:143）。

### 3.4 未知 provider 名：fail-closed 与修复路径

写侧只做语法检查（provider 名非空、成对带 model，`ErrInvalidConfigChange`，agent/config.go:70-77）；名字**是否真的注入过**只有运行期 Agent 知道，推迟到轮顶解析处 fail-closed：折出非空 provider 名而 `a.providers` 表中无此键时，该轮以 `ErrUnknownProvider` 中止，错误信息点名线程与 provider 名（agent/agent.go:429-433，sentinel 定义 agent/config.go:26）。

不静默回退默认 provider 是刻意决策：调用方写错了名字，静默回退意味着用错误的模型/计费悄悄跑完全程，比报错更贵。线程本身不受损——中止发生在任何 provider 请求之前，后续一条带正确名字的 `SetThreadConfig`（后写赢）即修复，被中断的 run 照常 `ResumeThread`（TestUnknownProviderAbortsRunAndRepairResumes，agent/provider_hotswap_test.go:285）。解码侧镜像：日志里出现"provider 无 model"的记录说明日志被手改或不合契约，`decodeConfigChanged` 以 `ErrIncompatibleLog` 拒读，不猜（agent/persist_dto.go:133-152）。

### 3.5 降级契约：历史不改写，降级住在适配器出站层

跨 provider 回放的历史**一律不清洗**：内核 loop / store / 重放零参与降级，规则表冻结在 DESIGN.md（docs/DESIGN.md:236）：openai 跳过外来 `ReasoningItemBlock`、透传 `ReasoningBlock`（reasoning_content）；glm 剥离两种推理块；openairesponses / codex 原样回放自有 `ReasoningItemBlock`（加密内容 + ID、保序），丢弃外来 `ReasoningBlock`；任何适配器不因外来推理块硬错误；文本与图像永远携带；codex 对 tool_call ID/Name 缺空、非法 JSON arguments 的既有校验照旧 fail-loud。模型设置是唯一的例外遮蔽项：线程 model 记录机械遮蔽 agent 级 `WithModel`。每个适配器的逐字段行为由各自的 `cross_provider_degrade_test.go` 与 agent/provider_hotswap_test.go 钉死；rs_ 条目保序约束（孤儿 rs_ 项会被 OpenAI 400）由压缩的消息组语义保证，归 13-agent-context。

## 4. provider 热切换（WithProviders + ConfigChange.Provider）

### 4.1 注入表与解析

`New` 的位置参数 provider 是默认，以自身 `Name()` 注册；`WithProviders(providers ...llm.Provider)`（agent/agent.go:227）追加备用项，键 = `Provider.Name()`，重名后注册者胜（不 fail-fast，同名实例用自定义 Name 区分——agent/agent.go:308-317）。切换通道复用 §3 的 `SetThreadConfig`：没有独立的 switch API，provider 切换就是一条携带 provider+model 的 `config_changed` 记录。

轮顶解析（agent/agent.go:429-436）：折出名为空 = 从未切换（默认 provider）；非空则查表，整轮钉住解析出的实例——所有 retry attempt 与 `ErrStreamingNotSupported` 的 Chat 降级复用它（切换粒度是轮，不是 attempt，TestThreadProviderSwitchPinsRetryAttempts，agent/provider_hotswap_test.go:254）。匿名（无 store）运行永远用默认 provider：没有线程就没有日志，没有日志就没有切换语义。

### 4.2 切换规则表：什么重置、什么保留

规则表的统一判据：**线程日志拥有的东西（历史、队列、配置）跨切换保留；Agent 实例拥有的东西（工具表、审批策略、重试预算）与切换正交；厂商相关的东西（wire 编码、推理块形态）在适配器出站层现场降级。**

| 维度 | 切换时的行为 | 为什么这样设计 |
|---|---|---|
| 生效时机 | 下一轮顶部；在飞请求在原 provider 上跑完 | 唯一生效点 = 回合边界；请求中途换实例会撕裂一轮（retry/stream 必须复用同一实例） |
| 对话历史 | **完整保留，一字不改**；外来推理块按 §3.5 冻结表在适配器出站转换时降级 | history 是日志真相（replay 即重建）；改写历史需要"迁移"语义且不可恢复；降级是纯投影，落在唯一知道厂商差异的层 |
| 工具 | **不变**：Registry 是 Agent 构造期资产，线程配置碰不到它 | 工具表 per-tenant（判据二：状态归属）；`tools_changed`/per-thread 工具过滤明确否决（docs/CONTROL-PLANE-DESIGN.md §8） |
| 压缩摘要 | 历史里已有的摘要原样保留；**切换后的压缩/摘要调用新 provider + 线程生效 model** | 摘要是当轮 provider 的函数（`compactHistoryRound` 收当轮解析的 provider 与 opts，agent/agent.go:440/757 起）；否则 openai 配额死掉 30 轮后压缩仍调用死掉的 openai，静默坏死（TestSummarizationFollowsSwitchedProvider，agent/provider_hotswap_test.go:327） |
| steering 队列 | **不受影响**：队列是 provider 无关的文本，下一 drain 点照常注入 | 队列状态与 provider 正交；插话的到达语义不随厂商变化 |
| options | 线程 model 机械遮蔽 agent 级 `WithModel`；`Thinking` 覆盖；Stop/JSONMode/MaxTokens/Temperature 跟随线程，由目标适配器按既有契约接受/拒绝/忽略 | 成对语义使 model 唯一权威，无需抑制规则；跨厂商调参放 agent 级本身是误用（做热切换的 harness 不该这么配，docs/DESIGN.md:228） |
| 重试与流式降级 | 同一轮全部 attempt + Chat fallback 复用解析出的同一实例 | 轮粒度切换；attempt 粒度切换会让一次重试横跨两个厂商，计费与语义都说不清 |
| 解析失败 | `ErrUnknownProvider` fail-closed 中止该轮；不回退默认 | 静默回退 = 用错误配置跑完全程；线程可修复（§3.4） |
| checkpoint | 生效配置（含 provider）投影进 v3 快照；重建无需新记录 | 配置是日志事实，快照只是加速器；第三个 run 首轮请求直接解析到切换后 provider（TestSummarizationFollowsSwitchedProvider 尾段） |
| 未切换的线程 | 日志字节不变（payload `provider` 字段 omitempty） | 旧二进制只在线程真正切换过时才 fail-loud，兼容面最小化（TestConfigChangedPayloadProviderFieldByteCompat，agent/provider_hotswap_test.go:139） |

**没有任何东西被"重置"**：这张表里找不到一行"切换时清空 X"。这是设计结论而非疏漏——切换的全部状态变化就是 provider 解析结果变了，其余世界照旧。

### 4.3 provider+model 为什么必须成对

规则：携带非空 `provider` 的记录必须同时携带非空 `model`；写面拒绝缺失形态（`ErrInvalidConfigChange`，agent/config.go:70-77），解码面视其为不合契约记录（`ErrIncompatibleLog`，agent/persist_dto.go:133-152），两侧镜像由 TestSetThreadConfigProviderValidation / TestDecodeConfigChangedProviderPairRule（agent/provider_hotswap_test.go:102/:160）钉死。

三个理由：

1. **model 名是 provider 局部命名空间**。`glm-4.7` 离开 glm 无意义；不成对的切换会把 A 家模型名发给 B 家端点，错误在厂商服务端才暴露，且可能表现为"跑通了但模型不对"的静默错配。
2. **重放无撕裂态**。成对落在一条记录里，fold 以记录为原子单位，任何重放位置看到的配置都是自洽的 (provider, model) 组合——拆成两条记录则存在"provider 已换、model 未换"的中间日志位置。
3. **机械遮蔽优于抑制规则**。线程 model 一经写入即覆盖 agent 级 `WithModel`（options 后写胜），无需"哪些选项该随切换失效"的语义判断；`Thinking` 仍可独立覆盖（provider 置位时 thinking 是可选携带）。

被否决的备选：Provider 包装器做供应商切换（不持久、wrapper 拿不到 ThreadID/Round、非线程隔离——docs/CONTROL-PLANE-DESIGN.md §1）；不成对切换 + 事后补 model 记录（撕裂态见上）。

## 5. FinishRound 钩子（agent/finishround.go）

### 5.1 用途：内容判定层

```go
func WithFinishRound(fn func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error)) Option   // agent/finishround.go:65
```

它回答一个 maxIter 回答不了的问题：**这一轮的内容（assistant 消息 + 工具结果）值不值得继续跑**。maxIter 是数值熔断（"还能不能跑"），FinishRound 是内容判定（"应不应该停"）；两层并存，熔断优先级更高（docs/control-plane-decision-record.md 原则 D，同构先例 OpenAI Agents SDK 的 max_turns + hooks）。典型用途：预算判定、循环检测、内容审查——在干净边界上终止"模型还想继续"的 run。

`FinishRoundContext`（agent/finishround.go:36）携带 `{ThreadID, Round（零基，跨 resume 连续的逻辑轮号）, Assistant, Results}`，后两者深拷贝——钩子拿到的是只读视图，改它影响不到循环。Round 跨 resume 的连续性由 `sessionRoundBase`（agent/agent.go:901）保证（TestFinishRoundErrorIsTypedAndResumable 断言 [0 1]，agent/finishround_test.go:172）。

### 5.2 调用时机：两个调用点，两处豁免

钩子在**终态判定之前**被咨询，覆盖两种回合形态（D8-a 裁决：钩子前移，否则"模型想停"的分支不可达，而那正是 Continue 的主用途）：

1. **工具回合**：`round_committed` 落库、结果已追加进内存 history 之后、下一轮迭代开始之前（agent/agent.go:684）——此刻无在途请求、上下文静止。
2. **无工具回合**：先以 `terminal_text` 子状态持久宣告（`declareTerminalRound`，agent/persist.go:326；round_declared 的 kind 字段泛化，重放层接受无工具调用的 terminal_text 宣告并并入 history），然后才咨询钩子（agent/agent.go:537-544）。宣告先于钩子是崩溃窗口的修复：Continue 后崩溃，重放不丢这个回合（TestFinishRoundToolFreeContinueCrashWindow，agent/finishround_test.go:260 验证"宣告已 durable、重放恢复后出站恰好一次携带该回合"）。

两处豁免：maxIter 熔断圈（最后一轮）**不咨询**钩子——熔断优先级最高，Continue 不可延长预算护栏，且该分支零决策零执行、保持冻结（agent/agent.go:536 的 `!isLast` 条件；TestFinishRoundNotConsultedAtMaxIterBoundary，agent/finishround_test.go:143）；Resume 的 cancelled 快照分支同样不咨询。

### 5.3 三值语义

| Action | 语义 | 持久化后果 |
|---|---|---|
| `FinishRoundDefault`（零值） | 维持默认调度：有工具调用继续，无则自然收尾 | 工具回合：既有 declare/commit 照常；无工具回合：terminal_text 宣告已在，run 经 `run_closed` 关闭（不再重携带消息，agent/agent.go:577-586） |
| `FinishRoundContinue` | 保证恰好再发一次 provider 请求；steering 排队输入随 drain 搭车；无输入则原样 context 再问一轮 | terminal_text 宣告已 durable，循环继续；"不空转"由下一轮的自然终止规则保证——continue 只保证一次，不保证继续到自然停；受 maxIter 约束（TestFinishRoundContinueKeepsNoToolRunAlive，agent/finishround_test.go:72） |
| `FinishRoundEnd` | 合作式收尾：正常 Done，`Truncated=false`，history 以工具结果收尾，`DoneEvent.Message` = 本回合 assistant | 新增 `run_closed` 记录（agent/finishround.go:108；store/store.go:52）——声明与结果已在日志中，关闭 run 不重携带、不伪造收尾消息（D7/D4 裁决：合成 assistant 违反"不写假消息"）；与 run_cancelled 对称（宿主策略收尾 vs 调用方放弃） |

End 的关键边界行为由 TestFinishRoundEndAfterToolRound（agent/finishround_test.go:15）钉死：不写 `run_cancelled`、不涉及 `SettleThread`（它不是取消，没有待结算的中断）、`ResumeThread` 返回 `ErrNothingToResume`（run 已干净关闭，没有可恢复的中断态）、残留 steering 保持在队列里等下一个 run。`run_closed` 自身的三个写边界（写入前取消/写入后确认/ack 丢失）由 TestRunClosedWriteBoundaries（agent/finishround_test.go:386）钉死：终态以日志实际状态为准，不以调用方自以为的状态为准。

### 5.4 边界：它不负责什么

- **不是取消通道**。取消语义（run_cancelled / SettlementToken / ResumeThread）属于 ctx cancel + settle 那条线（12-agent-persistence）；End 是合作式，不产生任何取消态。
- **不携带配置变更**（D5 裁决：决策钩子只决策，改配置走 `SetThreadConfig`）——两条路径可独立推理、独立测试。
- **不改写流量**。钩子拿到深拷贝视图，无改写消息/选项的权限（设计原则"不给改写流量的权限"；FinishRound 是决策不是改写，docs/CONTROL-PLANE-DESIGN.md §11）。
- **不负责 steering**：End 时队列里的输入留在队列；钩子里 Enqueue 的时序正确性由 §2.5 的吸收协议保证，钩子不需要知情。
- **不逾越熔断**：maxIter 终局轮不咨询（§5.2）。
- **错误即失败关闭**：钩子返回错误或非法 Action 值时，错误包为 `FinishRoundError{Round, Err}`（`errors.As` 可达，原因 `Unwrap` 保留，agent/finishround.go:43-54；非法 Action 值在 callFinishRound :71 起同路径拒绝），已 commit 的回合不回滚，run 可 Resume；并发上，钩子在本 run 内串行，同 Agent 并发 run 的并发安全由宿主保证（与 RoundContextProvider 同条款，agent/finishround.go:62-64）。

## 6. per-round context provider 的控制面角色

`RoundContextProvider`（agent/round_context.go:19）在**每次直连 provider 调用前**被调用，返回一段"当前事实"快照，包装为一条瞬态 user 消息注入出站，**从不进入 history**：

```go
type RoundContextProvider func(context.Context, RoundContextRequest) (string, error)   // agent/round_context.go:19
// RoundContextRequest{Round, Attempt, ThreadID} —— 每个 retry attempt 也各调一次（provider_call.go:33，request.Attempt 递增）
```

它在控制面里的角色是把“事实类”输入与“任务类”输入分开：steering 输入是任务（必须进历史、必须可恢复）；round context 是事实（必须新鲜、永不污染历史）。信封文本 `[go-agent runtime context snapshot; current facts only, not a new user task]`（agent/round_context.go:13）向模型声明这不是新任务。落库对比：steering/config/End 都走线程日志，round context 走每轮现场回调——需要扛过崩溃的去日志，需要新鲜的走回调，两套通道互不越界。

与 steering 的预算交互是控制面内部的耦合点：启用 overlay 时硬限制收窄为 `canonicalContextCap() = contextWindowTokens − window/5`（agent/round_context.go:42-46），steering 的准入上界与 drain 预算同步用它（agent/agent.go:745）——overlay 的预留必须活过 steering 注入，且注入后的 canonical 历史超 cap 直接报错（agent/agent.go:459-463）。快照自身有界：信封超预算报错；内容超预算时二分搜索保留头尾并插入截断标记（`boundedRoundContextMessage`，agent/round_context.go:102），空快照 = 本次不注入。失败以 `RoundContextError{Round, Attempt, Err}` 类型化包装。它是策略注入点而非接缝（00-overview.md §4.5），详情（overlay 计算、截断算法）归 13-agent-context。

## 7. 与持久化、结算的边界

两个外部写者不产生 run、不产生 `SettlementToken`，与 `SettleThread` 没有直接交互——但有一条刻意的间接效应：外部写入推进了线程 head，原 run 的陈旧 `SettlementToken.ExpectedHead` 因此不再匹配，后续显式结算按既有规则报 `ErrRevisionConflict`（agent/settle.go:70-75）。**这是特性不是缺陷**：插话已落库这一事实应当阻断陈旧 token 的盲结算——取消不能把已经接受的用户输入一并抹掉（docs/CONTROL-PLANE-DESIGN.md §5）。

## 8. 明确不做（防止范围蠕变）

以下方向在裁决中被明确否决（docs/CONTROL-PLANE-DESIGN.md §8，理由详见对应裁决记录）：`tools_changed` / per-thread 工具过滤（per-user Agent 实例组合解决）；`afterToolCall` 结果改写、`transformContext`、`convertToLlm` 等通用钩子（与 Tool/Provider 包装器、Compactor 链职责重复）；steering 的 QueueMode 双模式（v1 固定 drain-all，one-at-a-time 留作运行时策略后加，记录格式不变）；中断式插话（abort-and-reprompt，ctx cancel + settle + resume 已覆盖，选择权在交互层）；follow-up 独立队列（残留队列由下一 run 消费，零新机制）。

## 9. 契约测试索引

控制面的行为契约由四个白盒测试文件钉死（stdlib `testing`，在包内）：

| 文件 | 覆盖 | 代表用例 |
|---|---|---|
| agent/steering_test.go | 入队幂等/准入/serialized 计量、drain FIFO 前缀/部分前缀/无跳过、rebase 保留吸收项、崩溃窗口、恰好一次重放、run 边界 | :448 FIFO 无跳过、:526 部分前缀、:592 序列化计量、:386 End 钩子内入队 |
| agent/config_test.go | 生效时机（run 前/中/重启后）、ABA、v2/v3 checkpoint、错误分类 | :56 mid-run 吸收、:233 ABA、:143/:172 checkpoint 回退/投影 |
| agent/finishround_test.go | End/Continue/Default 三值、maxIter 豁免、terminal_text 崩溃窗口、类型化错误、run_closed 写边界 | :72 Continue、:260 崩溃窗口、:386 写边界三态 |
| agent/provider_hotswap_test.go | 成对校验（写+解码）、字节兼容、fold/apply 遮蔽、轮粒度钉住、fail-closed 修复、摘要跟随、rs_ 保序 | :221 轮粒度切换、:254 重试钉住、:285 修复路径、:327 摘要跟随 |

并发面（-race -shuffle：并发 Enqueue + 多线程 run + 钩子并发安全）随仓内常规测试命令覆盖：`go test -race -shuffle=on -count=1 ./agent`。

