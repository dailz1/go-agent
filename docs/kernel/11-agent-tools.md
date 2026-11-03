# 11 · 工具执行模型（agent 包）

> 内核文档集第 11 篇。本文只讲 `agent` 包如何调度、隔离与结算一次工具轮：串行/并行两条执行路径的选择规则、planner–dispatch–settle 三阶段、panic 隔离、取消传播、审批 fail-closed、软失败契约，以及工具执行与持久化、maxIter 的交界。
> 相邻专题各有专篇：第 10 篇（10-agent-loop.md，自主循环与 maxIter 终局轮）、第 12 篇（持久化与线程恢复）、第 31 篇（tool 包：`Tool` 契约、`ToolResult` 与 `Registry` 本体）。本文只写到与它们的交界，不重复其内部契约。工具并行的接口级冻结契约原文在 docs/DESIGN.md「工具并行执行（P2-2 冻结契约）」一节（docs/DESIGN.md:392-407），本文与其冲突时以 DESIGN.md 为准。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动）。

## 1. 两条执行路径与选择规则

### 1.1 入口与分派

主循环在宣布完本轮全部 ToolCallEvent 之后（TC-first，见第 10 篇）调用唯一入口 `executeToolRound`（agent/agent.go:678）。分派规则只有一次比较（agent/tool_execution.go:77-82）：

```go
if a.toolConcurrency == 1 {
    return a.executeToolRoundSerial(...)
}
return a.executeToolRoundParallel(...)
```

`toolConcurrency` 来自构造选项 `WithToolConcurrency(n)`（agent/agent.go:195），构造默认 1（agent/agent.go:287），`n <= 0` 在 `agent.New` 归一为 1（agent/agent.go:301-302）。并发度的作用域是"同一个模型回复内的工具调用"：模型轮之间永远是串行的请求-响应，所以这里没有全局调度器，只有 per-round 的二选一。

| 条件 | 路径 | 语义 |
|------|------|------|
| 默认或 `WithToolConcurrency(1)` | `executeToolRoundSerial`（agent/tool_execution.go:84） | 逐调用 live `Get → approval → Execute`（agent/agent.go:934） |
| `WithToolConcurrency(n>1)` | `executeToolRoundParallel`（agent/tool_execution_planner.go:12） | planner → dispatch → settle 三阶段，同轮副作用可重叠 |

### 1.2 为什么默认串行

**被否决的替代：默认并行。** 裁决记录在路线图里（docs/DESIGN.md:390）："默认串行保证确定性；历史按调用顺序追加，完成可乱序靠 ID 配对"。默认值改动的代价不在性能而在行为：并行让同轮工具的副作用在时间上重叠，让事件时序、日志交错、审批问询顺序都依赖调度时序。go-agent 的定位是嵌入库，默认必须是最小惊讶路径；写下 `WithToolConcurrency(n>1)` 这个动作本身，就是对"副作用可重叠"的一次知情确认。

### 1.3 n=1 兼容契约

P2-2 冻结条款（docs/DESIGN.md:394）：`n=1` 保持既有可观察行为——逐调用、live `Get -> approval -> Execute`，且既有 panic 仍按原路径逃逸（`Info()`/审批回调 panic 不被 recover，直接冒出迭代器；tool_serial_compat_test.go 的 `TestSerialPlannerInfoPanicEscapes`、`TestSerialApprovalPanicEscapes` 钉死）。`tool_serial_*` 测试组把"不传 option"与"显式 `WithToolConcurrency(1)`"钉成逐事件一致：成功/硬错误轨迹（`TestDefaultAndExplicitSerialMatchSuccessAndHardError`）、取消轨迹（tool_serial_cancel_test.go 的 `TestDefaultAndExplicitSerialMatchCancellation`）、审批交错轨迹（`TestDefaultAndExplicitSerialMatchApprovalInterleaving`）。

**为什么保留这个不对称**（串行 panic 逃逸 vs 并行 panic recover，见 §2.5）：P2-2 是行为增强，不是行为变更。已有嵌入方可能依赖串行路径的 fail-fast panic 守卫自己的审批 UI；内核无权单方面改写已被观察的行为。所以新增的规划边界 recover 只属于 `n>1`，`n=1` 走旧路径。

### 1.4 串行路径的形状

`executeToolRoundSerial`（agent/tool_execution.go:84-104）每个槽位：

1. 执行前检查 `ctx.Err()`（:87）——已取消则该槽位定案为 missing，立即结算收尾（:88-90）；
2. `executeTool`（agent/agent.go:934-960）：live `registry.Get` → 未知名软结果（:936-938）→ 审批（:940-950）→ `executeToolHandle`；
3. 每个槽位完成后立刻 `settle(index+1)`（:99）。

第 3 步是刻意的：n=1 也走共享的 settle，每槽位结算一次，为的是保留历史时序——c1 的 ToolResultEvent 在 c2 派发之前就交付，与 P2-2 之前的旧循环逐事件一致（docs/DESIGN.md:406："n=1 每完成一个槽位即走共享 settlement，以保留 c1 在 c2 派发前 materialize 的旧时序"）。settle 的机制见 §2.4。

## 2. 并行执行设计（n > 1）

### 2.1 三阶段流水线

```
模型回复 assistant(N 个 tool_use)
  │  （前置已完成：declareRound 落库 → TC1..TCN 连续宣布，见第 10/12 篇）
  ▼
① planParallelToolRound    agent/tool_execution_planner.go:32   调用方 goroutine，零并发
   │   live Get → Info() 边界 → 审批边界，全部按声明顺序
   │   未知名 / 无审批回调 / 用户拒绝 → 该槽位 resolved 软结果
   │   Info() 或审批回调 panic → hardError，中止规划
   │   ctx 已取消 → 中止规划
   ▼  通过规划的槽位保持 pending（handle 已冻结）= 待执行集合
② dispatchParallelToolRound  agent/tool_execution_planner.go:87  min(n, N) 个 worker
   │   jobs 无缓冲 channel；每槽派发前检查 ctx
   │   worker: executeToolHandle（Execute panic → 软结果）
   │   取消 → 停止派发，close(jobs)，排空在飞 worker
   ▼  markPendingMissing：未派发的 pending 槽位定案为 missing
③ settle(N)                agent/tool_execution.go:108  与串行共享的唯一结算路径
     全 settled → 按声明序 materialize 全部结果 → 单次 commitRound
     有 hardError（优先）→ materialize 前缀 → 交错误 → 不 commit
     有 missing → materialize 前缀 → 交 ctx.Err() → 不 commit
```

三个阶段的职责刻意分离：①回答"这些调用分别允不允许执行、由哪个 handle 执行"；②只做并发执行；③独占 history 追加、事件交付与落库。共享结算（③）是两条路径唯一的行为汇合点，settle 的注释原话是 "the sole history/event/commit path"（agent/tool_execution.go:106-107）——配对、前缀、commit 规则只写一处，串行与并行不可能在结算语义上漂移。

### 2.2 planner 阶段：冻结与整批审批

`planParallelToolRound`（agent/tool_execution_planner.go:32-66）按声明顺序逐槽做三件事。

**冻结执行 handle 与 `Info`。** 通过规划的槽位记下 `slots[index].tool = t`（:47），`Info()` 只在规划期经 `toolInfoAtPlannerBoundary`（:69-76）调用一次。规划期就是 n>1 的解析点：规划结束后，本轮之内 Register/Unregister/Replace 一律不影响成员与 handle——`TestParallelPlanFreezesRegistryAndRunToolsSnapshot`（agent/tool_parallel_approval_test.go）与 `TestParallelSamePlanRegistrationRemainsFrozen`（agent/tool_parallel_epoch4_test.go）钉住"同轮注册的新工具不可执行、返回 not found 软结果"。对比串行的逐调用 live `Get`（tool_serial_registry_test.go 的 `TestSerialLiveGetFindsToolRegisteredEarlierInRound`：串行同轮注册即可用）。

**被否决的替代：并行途中逐槽 live Get。** 那会让"本轮由哪些工具组成"取决于 worker 调度时序——审批面看到的注册表和执行面看到的可以不是同一版本，P2-4 热变更语义（变更在下一解析点生效，docs/DESIGN.md:396）也就无从谈起。

**整批声明序审批。** 所有 RequiresApproval 工具在任何 Execute 开始之前、按声明顺序逐个问询审批回调（`ApprovalFunc`，agent/agent.go:103-105；Warn 日志 :55，问询 :57）。`TestParallelMixedApprovalPlanning`（agent/tool_parallel_approval_test.go）钉住两点：审批问询顺序等于声明顺序；被拒绝/无回调/未知名的槽位拿软结果，已批准的工具照常执行。

**被否决的替代：审批与执行交错（边问边跑）。** 审批是人对 agent 的门禁：问询顺序必须与模型声明顺序一致才可预期；"先全批、后执行"保证用户拒绝第 2 个调用时，第 3 个还没来得及产生副作用。交错执行会把"已产生的副作用"和"未做出的决定"压进同一个时间窗，拒绝的语义就残缺了。

**三类软结果即时定案。** 未知名（"tool %q not found"，:38-40）、无审批回调 fail-closed（:51-53）、用户拒绝（:61-64）都在规划期写死为对应槽位的 resolved 软结果，并即时过 `limitedToolError` 限长（:144-146）——软错误内容同样受结果截断约束（见 §3.4）。

### 2.3 dispatch 阶段：有界 worker 池

`dispatchParallelToolRound`（agent/tool_execution_planner.go:87-121）：

- worker 数量 = `min(a.toolConcurrency, len(calls))`（:19）。`TestParallelWorkerPoolCapsConcurrentSuccess`（agent/tool_parallel_epoch4_test.go）用活跃计数钉住峰值恰好等于配置值。
- 主 goroutine 逐槽扫描 pending 槽位（:104-106），每槽派发前检查 `ctx.Err()`（:107-108），`select` 派发 / ctx.Done（:112-116）——即使阻塞在无缓冲 jobs channel（:88）上，取消也能立即退出派发循环。
- 取消的两个出口都是 `close(jobs)` + `workersDone.Wait()`（:114-115 与 :117-119）：不再派发新槽位，但**在飞 worker 排空等到底**。排空没有人为上界（docs/DESIGN.md:406）："工具必须 honor ctx，内核不以泄漏 goroutine 伪造终止"。

**被否决的替代：给排空加超时然后放弃 goroutine。** 超时后内核继续推进等于伪造终止，放弃 goroutine 等于泄漏；两条都违背稳定承诺（docs/DESIGN.md:23"用户程序不能挂"）。取消责任只能通过 ctx 移交给工具实现，不能由内核单方面宣布消失。

**被否决的替代：每调用一个 goroutine、不设池。** 并发上界就不再是用户配置的 n，资源占用随模型一轮请求的调用数失控，也无法用现有 channel 语义做有序排空。

### 2.4 settle：槽位状态机与有序结算

每个声明一个槽位，四种状态（agent/tool_execution.go:11-25）：

```
pending ──规划期软结果（未知名/无回调/拒绝）──▶ resolved
pending ──worker 完成（先过 applyToolResultLimit）─▶ resolved
pending ──Execute 返回 Go error ──────────────▶ hardError
pending ──取消导致未派发/未执行 ────────────────▶ missing
```

`observe`（:47-58）在状态写入时维护最低 hardError/missing 声明索引；`markPendingMissing`（:60-66）把取消留下的 pending 批量定案为 missing；`observeAll`（:69-73）在并行收尾时全量扫描一遍。

`settle(frontier)`（:108-152）的裁决顺序：

1. **boundary 选取**（:109-114）：hardError 优先于 missing，同类别取最低声明索引；
2. **materialize 连续 resolved 前缀**（:115-124）：从上次进度起，把边界内连续 resolved 的槽位写入 history（`ToolResultMessage`）并交付 ToolResultEvent，遇到非 resolved 槽位即停；
3. **hardError 终局**（:125-128）：交付带 `iteration %d: tool %q:` 上下文的错误，`return false`——不 commit；
4. **missing 终局**（:129-132）：交付 `ctx.Err()`，`return false`——不 commit；
5. **全 settled**（frontier == len(calls)，:134-150）：一次性把全部结果按声明序 `commitRound`（commit 上下文的取舍见 §4.2）。

两条被测试钉死的关键性质：

**完成顺序绝不泄漏。** worker 完成顺序、派发顺序只存在于槽位内部；事件、history、Store 记录全部按声明顺序。`TestParallelC2FirstKeepsEventHistoryAndCommitOrder`（agent/tool_parallel_epoch4_order_test.go）让 c2 先完成，断言事件序列、DoneEvent.History、round_committed 三处都仍是 c1,c2。为什么：TC/TR 按 ID 配对是协议层规则，但消费方 fold（`TestParallelEventsFoldToDoneHistory`，agent/tool_parallel_fold_test.go）、人类可读性、Store 的逐位对应校验都要求单一顺序；若按完成序 materialize，同一轮会在不同观测面呈现不同顺序，"事件折叠等于 DoneEvent.History"的契约直接破裂。

**被否决的替代：按完成序交付事件。** P2-2 冻结条款明确"完成顺序绝不泄漏到 history、事件或 Store"（docs/DESIGN.md:400）。

**硬错误只保留有序前缀。** hardError 槽位之后的完成结果——哪怕成功——全部丢弃：`TestParallelHardErrorsKeepOnlyTheOrderedPrefix`（agent/tool_parallel_test.go）中 index 1 硬错误时只有 1 个前缀结果交付。为什么：如果跳过硬槽位继续 materialize c3，历史里会出现"c1 有结果、c2 无结果、c3 有结果"的空洞，TC/TR 配对断裂，这条历史既过不了 OpenAI/GLM 出站转换，也过不了 Store 的逐位校验，且该轮永远无法 commit。丢弃整个尾部、整轮不 commit，让恢复层用 outcome unknown 统一补齐（§4.3），是唯一在每个持久状态下都保持配对完整的形态。

**被否决的替代：把硬错误降级成该槽位的错误占位软结果、跑完本轮。** 这直接违反软/硬边界的双向禁令（docs/DESIGN.md:62："Go error 也不得降级为软结果"）——系统级故障被伪装成工具输出后，"环境已不可信"这个事实就对模型和调用方双双失明。settle 的注释把语义钉死（agent/tool_execution.go:106-107）："Terminal errors always stop the iterator even when the consumer accepts their delivery"——硬错误无条件终止迭代，与消费者是否收下错误无关。

**hardError 优先于 missing。** 取消与硬错误竞态时，终态错误是硬错误而非 `context.Canceled`：`TestParallelCancelPlusHardError`（agent/tool_parallel_cancel_hard_test.go）在 c1 执行中取消、c2 已 boom，断言终态是 boom。为什么：`context.Canceled` 是通用噪声，硬错误才是可诊断的根因；优先级固定之后，调用方看到的终态与调度时序无关。

### 2.5 panic 隔离：三个边界，两种语义

| 边界 | 位置 | panic 的归宿 | 适用 |
|------|------|--------------|------|
| `Execute` | `executeToolHandle` 的 defer/recover（agent/tool_execution_planner.go:126-132） | 软结果 `tool %q panicked: %v`，error 通道置 nil | 串行 + 并行 |
| `Info()` / 审批回调（规划边界） | `toolInfoAtPlannerBoundary` :69-76、`approveAtPlannerBoundary` :78-85 | hardError，中止规划与全部执行 | 仅并行 |
| `Info()` / 审批回调（串行路径） | 无 recover（agent/agent.go:940-950） | 原样逃逸 | 仅串行 |

**为什么 `Execute` panic 是软结果。** panic 发生在一个已批准、已派发的槽位内，把它隔离为该槽位的失败不影响其余槽位裁决的有效性；对模型而言，崩溃的工具就是一次失败调用（`TestParallelExecutePanicIsSoftResult`，agent/tool_parallel_execute_panic_test.go）。这也是"默认安全"条款的组成部分（docs/DESIGN.md:67："工具 panic 不外泄"）——内核承诺嵌入方的进程不被工具实现挂掉。

**为什么规划边界的 panic 是硬错误。** `Info()` 或审批回调在规划期崩溃，意味着内核无法回答"这个槽位允不允许执行"这个前置问题；在未裁决的安全状态下继续规划或执行任何槽位都是赌博。所以 fail-closed：中止后续规划、一个都不执行——`TestParallelPlannerInfoPanicZeroExecute`（agent/tool_parallel_cancel_test.go）与 `TestParallelApprovalCallbackPanicZeroExecute`（agent/tool_parallel_approval_panic_test.go）的断言都是 0 执行。持久化协同：规划期 panic → 硬错误 → 不 commit → open declaration 保留，ResumeThread 可恢复（`TestParallelInfoPanicLeavesRecoverableDeclaration`，agent/tool_parallel_epoch4_test.go；`TestParallelApprovalPanicLeavesRecoverableDeclaration`，agent/tool_parallel_panic_recovery_test.go）。

**为什么串行保持逃逸。** 见 §1.3 的兼容契约。

### 2.6 取消传播

原 ctx 的三个检查点：规划循环每步（agent/tool_execution_planner.go:34-35）、dispatch 每槽派发前与 select（:107-116）、`Execute` 直接收原 ctx（:133）。三个取消时点对应的终态：

1. **宣布期取消**（消费者看到 ToolCallEvent 后 cancel）：宣布批次本身无 ctx 检查（TC-first 契约，agent/event.go:36-45 的注释；第 10 篇），取消在进入 executeToolRound 后的第一个槽位检查点生效——0 执行、终态 `context.Canceled`（`TestParallelCancelAfterAnnouncements`，agent/tool_parallel_consumer_test.go）。
2. **规划期取消**（典型：审批回调内 cancel，`TestParallelCancelDuringPlanning`，agent/tool_parallel_planning_cancel_test.go）：规划中止，剩余 pending 槽位经 markPendingMissing 全部定案 missing，0 执行。
3. **派发/执行期取消**：停止派发、排空在飞；已完成的槽位若凑齐全 settled 就照常 commit（§4.2），否则 missing 终态。

为什么审批回调里的取消也算规划期取消：审批是执行的准入判定，准入序列没走完就不该有执行发生——这与"整批审批先于执行"（§2.2）是同一条原则在取消维度上的投影。

### 2.7 审批 fail-closed

`RequiresApproval == true` 且 `approvalFn == nil` 时的行为：**拒绝执行，回软错误**。不是静默放行，也不是整轮报错。

- 代码：并行 agent/tool_execution_planner.go:51-53；串行 agent/agent.go:941-943。文案同一："tool %q requires approval but no approval callback is configured"。
- 执行计数为 0：`TestParallelNoCallbackFailClosed`（agent/tool_parallel_no_callback_test.go）断言工具一次都没跑。

两个被否决的替代：

**静默放行。** "没配审批 = 全部免审"会让危险工具在配置缺失时无声执行，直接击穿"默认安全"（docs/DESIGN.md:67："危险工具须审批"）。fail-closed 是安全边界的标准形态：能力缺失时选择不给能力，而不是假设调用方另有安排。

**整轮硬错误。** 无回调是 per-tool 可定位的配置错误；升格为整轮错误会让一个配置疏漏杀死整个 run，且模型得不到任何反馈。软错误让模型看到"这个调用被拒"，可以改道其他工具继续任务，循环连续性不受损。

用户拒绝（回调返回 false）同样是软结果（"tool %q execution rejected by user"，planner :61-64；串行 agent/agent.go:948-950），并伴随 Warn 日志（"tool requires approval" / "tool execution rejected"）。"用户说不"由此成为对话内容的一部分：模型可读、history 可审计、Store 可回放。

## 3. 软失败契约

### 3.1 决策：ToolResult{Status: ResultError}，而不是 Go error

`Tool.Execute` 的签名是双通道（tool/tool.go:14-29）：

```go
Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
```

接口注释即契约原文（tool/tool.go:25-28）：业务失败（坏参数、资源不存在……）返回 `ToolResult{IsError: true}` 回传给 LLM；Go error 只保留给应当中断循环的系统级故障。软结果的标准构造器是 `NewErrorResult`（tool/tool.go:101-106）；`Status` 的全部取值是 `ResultSuccess("")` 与 `ResultError("error")`（tool/tool.go:86-91）。

为什么这么设计——四层理由：

1. **失败的归属不同。** 参数错、找不到文件是"模型与工具之间的对话问题"，正确响应是作为对话内容回传，让模型下一轮自我修正；网络瘫痪是"程序与环境之间的故障"，正确响应是中断循环、让人看见。两者塞进一个通道，结局只有两个：要么把对话问题升级成程序事故，要么把程序事故降级成对话噪声。agent 包对这个边界的执行是严格的：worker 里 `Execute` 返回非 nil error → 槽位 hardError（agent/tool_execution_planner.go:133-136）；返回 nil result → 软错误 "returned nil result"（:137-139）。
2. **协议约束：TC/TR 配对必须永远完整。** 每条 assistant tool_use 必须有配对的 role=tool 结果消息，这是 OpenAI/GLM 出站转换的硬性要求（docs/DESIGN.md:99：中断轮恢复时合成的结果"按宣告顺序，满足 TC/TR 配对与 OpenAI/GLM 转换要求"；合成实现在 agent/persist_replay.go:43-58）。Go error 冒泡会留下未配对的 tool_use，历史从此不合法——无法 commit、无法 resume、无法续跑。软错误保证配对在任何结局下完整。
3. **循环连续性。** agent 的核心价值是自主恢复：软失败后循环继续，模型可以换参数重试、换工具、或向用户说明失败原因。一次坏参数打断整个 run，等于把恢复责任错误地推给嵌入方。
4. **可观测性与数据完整性。** 软失败是一等公民：ToolResultEvent 的注释写明覆盖 "successful execution, soft failures, approval rejections, and unknown tools"（agent/event.go:57-59）；软失败进 history（DoneEvent.History 是权威快照，agent/event.go:98-102）、进 Store（docs/DESIGN.md:95：round_committed"含软失败：审批拒绝、未知工具、nil 结果、恢复的 panic"）。失败轨迹不消失——事后审计、评测、数据导出拿到的是完整序列。

### 3.2 被否决的替代

**"工具错误也返回 Go error，循环层统一转软。"** 转换点必须回答"哪些 error 该转、哪些不该"，而这个分类恰恰是工具作者掌握的领域知识；接口层强制作者表态（选哪个返回通道），内核不做猜测。DESIGN.md 把边界钉成双向禁令（docs/DESIGN.md:62）："软错误 ToolResult 不得提升为 Go error，Go error 也不得降级为软结果"。

**"硬错误也物化成占位结果，跑完本轮再报告。"** 违反上述禁令的后半句，并且掩盖"环境已不可信"的事实；settle 对 hardError 无条件终止迭代（§2.4）。

### 3.3 对可观测性的影响

- **消费者视角。** 软失败有独立的 ToolResultEvent；硬失败只有一个终态 error yield，但 settle 先物化有序前缀再交错误，所以"崩溃前完成了什么"仍然可见——`TestParallelHardErrorFoldsOrderedPrefix`（agent/tool_parallel_fold_test.go）断言 fold 出 c1 的 TR 之后才是终态错误。
- **日志视角。** 审批 Warn（agent/tool_execution_planner.go:55、:62）、截断 Warn（agent/agent.go:918-932）、执行 Info/Debug（agent/tool_execution_planner.go:124-125、:140）——软失败全链路结构化日志。
- **Store 视角。** 软失败随 round_committed 落库（含恢复的 panic），重放与审计看到的就是模型当年看到的（docs/DESIGN.md:95）。硬失败不产生 commit——它不产生"结果"，只留下可恢复的 open declaration（第 12 篇）。

### 3.4 对循环连续性的影响

- 软失败不断轮：结果照常物化、照常 commit，下一模型轮照常发起。
- **软错误同样受结果限长约束。** 规划期软结果与 worker 结果都在槽位发布前过 `applyToolResultLimit`（agent/agent.go:918-932；并行入口 `limitedToolError`，agent/tool_execution_planner.go:144-146）。`TestParallelLimitsCompletedSlotsBeforeSlowWorkerDrains`（agent/tool_parallel_test.go）证明限长发生在槽位完成时而非结算时——慢工具未返回时，快工具的截断日志已经发出。为什么：报错文本也占上下文预算，一个循环报错的工具不设限就能撑爆上下文窗口；上限约为窗口的 30%（`toolResultRunes`，agent/agent.go:908-916）。
- 硬失败终止 run 但保留可恢复状态：见 §4.3。
- 结果截断的完整策略（掐头留尾、omission marker）不在本篇：见 agent/truncate.go 与 docs/DESIGN.md §7。

## 4. 与持久化的交互点

> thread 落库的 record 语义、ResumeThread/SettleThread 的完整契约在第 12 篇；本节只列工具执行路径上的接触点。

### 4.1 三个接触点

工具轮在持久化维度上有三个接触点，全部服从 durable-before-visible 纪律（docs/DESIGN.md:94-95）：

**轮前声明。** `declareRound`（agent/agent.go:660-665 → agent/persist.go:303-317）在任何宣布、审批、执行之前落库整轮 assistant 消息。`TestParallelDeclarationIsDurableBeforeApproval`（agent/tool_parallel_lifecycle_test.go）在审批回调内断言声明已在 Store 中。为什么：审批回调可能 panic、可能取消 run——先落库保证"模型请求过的调用"永远可恢复，审批环节的任何失败都不丢失事实。

**轮后提交。** 全 settled 后单次 `commitRound`（settle 尾部，agent/tool_execution.go:134-150 → agent/persist.go:355-367），结果按声明序逐位对应；硬错误/missing 结局不 commit，日志保留 open declaration。

### 4.2 commit 上下文的不对称

并行路径 `newToolRound(ctx, context.WithoutCancel(ctx), ...)`（agent/tool_execution_planner.go:13）；串行路径 `newToolRound(ctx, ctx, ...)`（agent/tool_execution.go:85）。

为什么并行要 detach：并行结算发生在排空之后，此刻调用方取消大概率已经发生；而结果已全部定案、事件即将交付——若因 ctx 取消跳过 commit，日志会把"内核已知的结果"谎报为 outcome unknown，内存视图与持久视图永久分歧，durable-before-visible 被击穿。`TestParallelSettledResultsCommitDespiteCancellation`（agent/tool_parallel_persist_test.go）钉死：交付 ToolResultEvent 后取消，终态是 `context.Canceled`，但 round_committed 必须已经在日志里。

为什么串行不需要：串行结算内联在每个槽位完成之后，"全部定案"与"commit"之间没有取消可以插入的异步窗口，保持原 ctx 让取消尽早生效。

### 4.3 中断与恢复的分工

硬错误/missing/消费者 break 都停在"已声明、未提交"状态；恢复时 `closeUnknown` 对每个声明调用合成一条 "outcome unknown" 软结果，绝不盲目重执行（agent/persist_replay.go:43-58）。两个测试钉住反直觉的细节：

- `TestParallelInterruptedResultBatchResumesWithOrderedUnknowns`（agent/tool_parallel_persist_test.go）：消费者在收到 c1 的 ToolResultEvent 后 break，恢复后 **c1/c2 都是 outcome unknown**——已交付的内存视图与未提交的持久视图分歧时，以持久视图为准，因为内核无法证明已交付结果的安全落库点存在。
- `TestParallelCommitFailureLeavesOpenRound`（同文件）：Store 写入失败时，已交付的事件保持已交付（不撤回），日志保持 open declaration（可重试提交或恢复）。

## 5. 与 maxIter 的交互

maxIter 终局轮在进入工具执行之前就被拦截（agent/agent.go:604-640）：全部请求照常宣布，但**零 planner、零审批、零执行**——每个调用拿到 deterministic skipped 软结果 "not executed: the run reached its iteration limit"（agent/max_iter.go:8-17 的 `iterationLimitResults`）。`TestMaxIterPersistent` / `TestMaxIterNonPersistent`（agent/tool_parallel_maxiter_test.go）钉住：Truncated=true、执行计数 0、Store 侧 committed 的是 skipped 结果。

让 maxIter 轮特殊的是持久化顺序：Store 路径先 `truncateRun`（agent/persist.go:369-389：declare + commit skipped + closeRun 一次完成）再发事件——整批 durable 先于第一个 ToolCallEvent，消费者 break 只停止自己的 delivery，不可能重开已关闭的 run（ResumeThread 返回 `ErrNothingToResume`；agent/event.go:36-45 的 ToolCallEvent 注释明说这是"唯一例外"）。为什么允许这个例外：skip 轮的结局在执行发生之前就完全确定，不存在"结果未知"，预关闭是安全的；普通工具轮的结局依赖执行，做不到。终局轮的完整状态机在第 10 篇。

## 6. 与 Registry 的交互边界

模型可见工具（provider 请求携带的 []ToolInfo）与执行解析是两个独立层次：

| 层次 | 默认 | `WithDynamicTools()` | 依据 |
|------|------|----------------------|------|
| 模型可见 | run 开始时 `registry.List()` 快照，整轮不变 | 每轮迭代顶部刷新 | docs/DESIGN.md:396 |
| 执行解析 | n=1 live Get；n>1 planner 冻结 | 解析点不变 | docs/DESIGN.md:396 |

Registry 的 Unregister/Replace/Apply 与在飞执行并发安全：在飞 Execute 自然跑完、结果如实入史，变更在下一个解析点生效；对已消失名字的调用是软错误（`TestParallelSamePlanRegistrationRemainsFrozen`）。Registry 本体的冻结 Info、读缓存、注册期校验是 tool 包的内部机制：第 31 篇。

## 7. 测试即契约

工具执行的语义以测试文件名分组钉死（全部在 agent/ 下，行号从略）：

| 测试组 | 钉住的契约 |
|--------|-----------|
| tool_parallel_test.go | n 归一化、声明序结果、有序前缀、限长时机 |
| tool_serial_compat_test.go / tool_serial_cancel_test.go / tool_serial_registry_test.go | n=1 与默认值逐事件一致；串行 panic 逃逸；live Get |
| tool_parallel_approval_test.go / tool_parallel_no_callback_test.go / tool_parallel_approval_panic_test.go | 整批声明序审批；planner 冻结快照；fail-closed；回调 panic |
| tool_parallel_cancel_test.go / tool_parallel_cancel_hard_test.go / tool_parallel_planning_cancel_test.go / tool_parallel_consumer_test.go | 三个取消时点；硬错误优先于取消；consumer break |
| tool_parallel_persist_test.go / tool_parallel_lifecycle_test.go / tool_parallel_panic_recovery_test.go | 声明先于审批；声明序 commit；中断恢复；commit 失败 |
| tool_parallel_epoch4_test.go / tool_parallel_epoch4_order_test.go | 规划期 panic 可恢复；同轮注册冻结；worker 池上限；完成序不泄漏 |
| tool_parallel_maxiter_test.go / tool_parallel_fold_test.go / tool_parallel_compaction_test.go | 终局 skip；事件折叠等于 DoneEvent.History；压缩预算 fail-closed |

## 8. 锚点速查

| 符号 | 位置 | 职责 |
|------|------|------|
| `executeToolRound` | agent/tool_execution.go:77 | 串行/并行分派唯一入口 |
| `executeToolRoundSerial` | agent/tool_execution.go:84 | 逐槽 live Get→审批→执行，每槽即时 settle |
| `executeToolRoundParallel` | agent/tool_execution_planner.go:12 | plan→dispatch→settle；commit 上下文 detach |
| `planParallelToolRound` | agent/tool_execution_planner.go:32 | 冻结 handle/Info + 整批声明序审批 |
| `dispatchParallelToolRound` | agent/tool_execution_planner.go:87 | min(n,N) worker 池；取消即停派发、排空在飞 |
| `executeToolHandle` | agent/tool_execution_planner.go:123 | Execute 边界：panic→软结果，nil→软结果 |
| `toolRound.settle` | agent/tool_execution.go:108 | 唯一 history/event/commit 路径 |
| 槽位状态机 | agent/tool_execution.go:11-25 | pending / resolved / hardError / missing |
| `executeTool` | agent/agent.go:934 | 串行解析：live Get + 审批 fail-closed |
| `applyToolResultLimit` | agent/agent.go:918 | 槽位发布前的结果限长 |
| `WithToolConcurrency` | agent/agent.go:195 | 并发度选项；n<=0 归一 1（agent/agent.go:301-302） |
| `declareRound` / `commitRound` / `truncateRun` | agent/persist.go:303 / :355 / :373 | 落库三接触点（第 12 篇展开） |
| `closeUnknown` | agent/persist_replay.go:43 | 恢复期 outcome unknown 合成 |
| `iterationLimitResults` | agent/max_iter.go:8 | maxIter 终局 skip 结果 |
