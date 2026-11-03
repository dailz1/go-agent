# pi agent 内核 runLoop 精读笔记

> 对象：`packages/agent/src/agent-loop.ts` 的 `runLoop`（pi monorepo）
> 视角：Go 开发者，对照 go-agent 的 `runStreamInternal`
> 本文是学习笔记，非 pi 官方文档。

## 1. 心智模型：一句话版本

runLoop 是一个**由回调字段驱动的调度器**：内层 while 每圈执行一个回合
（一次 LLM 请求 + 工具执行），外层 while 处理"本来要停了但还有善后"的情况。
内核不含任何业务逻辑，所有扩展点都是 `AgentLoopConfig` 上的可选回调字段。

依赖链：`coding-agent → agent-core (内核) → ai (模型层)`。内核是框架，
coding-agent 是用框架写出来的应用——库是"你调它"，框架是"它调你"。

## 2. 核心概念：回合（turn）与运行（run）

```
run = 一次 agent_start → agent_end（用户的一次完整交互）
turn = 第 N 次 LLM 请求发出 → 第 N+1 次请求发出之前的所有事情
```

- **回合 = 一次 LLM 请求**。reasoning、文本、toolCall 都只是这次请求的
  响应里可能出现的组成部分，不单独成回合。
- 回合的边界不由内容决定，由**请求边界**决定。这是所有修改类钩子
  （prepareNextTurn/prepareRequest）的安全时机：此刻没有在途请求，上下文静止。
- 已发出的请求不可变（HTTP 语义）：回合进行中改 model/context 无效，
  所以框架把修改点收缩到请求边界，把物理约束翻译成显式 API。

## 3. 双层循环结构

```
外层 while(true)                ← "本来要停了，但还有善后/信用"
└─ 内层 while(工具驱动 or 队列驱动)   ← ReAct 主循环，每圈一个回合
   1. prepareNextTurn 钩子（第 2 圈起；可换 context/model/追加消息）
      └─ 钩子后补轮询 steering（钩子可能耗时，期间可能有新插话；
         仅在前次轮询为空时补，保住 one-at-a-time 语义）
   2. emit turn_start
   3. 注入消息：declareToolChanges([prepared..., pending...])
      逐条 emit message_start/end，push 进两本账
      pendingMessages = []（消费即清空）
   4. prepareRequest 钩子（最后一道闸门，含第一次请求）
   5. streamAssistantResponse：调 LLM，流式收完 → 定稿 message_end
   6. stopReason 为 error/aborted → 硬退出（钩子不可改变的退出）
      stopReason 为 length → 所有工具调用判失败（参数可能被截断）
   7. 有 toolCall → executeToolCalls（串行或并行）
      执行完 push 工具结果进两本账
      hasMoreToolCalls = !batch.terminate（全票 terminate 才停）
   8. finishTurn 钩子 → decision
      "end" → 直接退出；"continue" → 记为信用 explicitContinuation
   9. 轮询 steering → pendingMessages
      若 hasMoreToolCalls 或有插话 → 销掉信用（承诺已被自然满足）
外层退出条件检查（内层退出后依次）：
   a. follow-up 队列非空 → 作为 pending 重新进内层
   b. explicitContinuation 未核销 → 仅上下文白补一回合
   c. 都没有 → break，emit agent_end
```

终止条件只有自然终止（无工具调用 + 队列空 + 无信用），没有 maxIter 熔断——
pi 把防失控的责任交给了上层（coding-agent 的 session 层）。

### 内层循环的四个关键变量

| 变量 | 谁写 | 谁读 | 决定什么 |
|---|---|---|---|
| `hasMoreToolCalls` | 工具执行后 | 顶部 while、信用核销 if | 工具是否驱动下一圈 |
| `pendingMessages` | 各轮询点 | 顶部 while、注入循环、信用核销 if | 插话是否驱动下一圈 + 注入内容 |
| `explicitContinuation` | finishTurn 记、核销点/外层清 | 外层循环 | 是否补一回合兑现 continue |
| `lastCompletedTurn` | 每回合末尾 | prepareNextTurn | 回合间手术的现场数据 |

### 信用核销（易读错的点）

内层末尾的 `if (hasMoreToolCalls || pendingMessages.length > 0)
{ explicitContinuation = false }` **不是循环条件**，只销账：
finishTurn 的 continue 只承诺"再发一次请求"，若工具或插话已自然驱动
下一圈，该承诺已被兑现，销掉信用避免外层白补一回合。
插话的处理时机不受此影响——下一圈循环体开头就会注入它。

## 4. steering 消息的完整旅程

```
用户输入 → Agent.steer() 入队（任意时刻，立即返回，只是登记意图）
        → 到达后的第一个轮询点被 drain：
            循环启动时 / prepareNextTurn 后（条件补）/ 每回合末尾
        → 下一圈循环体开头：declareToolChanges + 注入历史
        → 随下一回合的请求发给模型
```

- 摘取（drain）和处理（注入+发送）是两个时刻：摘取在回合 N 末尾，
  处理在回合 N+1。真正的"处理"必然落在某个 LLM 请求上。
- 注入顺序：`[...preparedMessages, ...pendingMessages]`——
  钩子的系统铺垫在前，用户插话在后。
- 与"打断式"（abort 后立刻重新 prompt，如 Codex 的体感）对比：
  pi 是排队式，不打断在途工作；打断式响应快但丢弃已完成的工作。
  pi 内核两条路都支持（steer / abort+prompt），选择权在交互层。

## 5. 注入点详解：preparedMessages 与 pendingMessages

```
for (const message of declareToolChanges(ctx, [...prepared, ...pending])) {
    emit(message_start); emit(message_end);   // 背靠背：这些消息不是流式生成的
    currentContext.messages.push(message);    // 账本一：模型可见历史
    newMessages.push(message);                // 账本二：本次运行增量（返回值）
}
pendingMessages = [];
```

- `declareToolChanges` 是纯函数：保证 transcript 里声明的工具集与
  context.tools 一致（差异写成 system 消息的 toolsAdded/toolsRemoved）。
- 两本账记同一份数据，因为消费者不同：前者进请求体，后者是返回值。
- `pendingMessages = []` 必须显式清（跨圈变量）；preparedMessages 是
  圈内局部变量，无需清。

## 6. 钩子体系：三类调用模型

| 钩子 | 类别 | 入参带现场 | undefined 含义 | 决定什么 |
|---|---|---|---|---|
| getSteeringMessages | 纯拉（轮询） | 否 | 无事发生（[]） | 未来注入什么 |
| getFollowUpMessages | 纯拉（轮询） | 否 | 无事发生（[]） | 自然停止后是否续 |
| prepareNextTurn | 咨询 | 有（上回合快照） | 维持默认 | 下一回合的状态 |
| prepareRequest | 咨询 | 有（当前请求状态） | 维持默认 | 这一发请求带什么 |
| beforeToolCall | 纯推（否决） | 有 | 不干预 | 当前动作执行与否 |
| afterToolCall | 纯推（改写） | 有 | 不干预 | 当前结果的形状 |

用 Go 直觉：钩子不是 interface，是 **Config struct 里的回调函数字段**
（TS 的 `?:` ≈ Go 的 nil 函数字段，`?.` ≈ nil 检查）。
互相独立、按需挂载；不挂 = 功能不存在，循环照常跑。

### prepareNextTurn vs prepareRequest

- prepareNextTurn：回合间手术台。能换 context/model/thinkingLevel + 追加
  messages；替换对后续所有回合持续生效；典型用途 compaction。
- prepareRequest：请求出门前最后检查。不能追加消息；同样持续生效；
  典型用途"以持久化投影为准重建上下文"+"虚拟模型路由（仅本发生效）"。
- 顺序：先 prepareNextTurn 后 prepareRequest，后者为准。

### compaction 的位置（架构关键）

runLoop **没有** compaction——它只在 prepareNextTurn 留了插槽。
压缩实现在 coding-agent（agent-session.ts:904 → _compactBeforeNextAssistantResponse）：
查阈值 → 超了调 LLM 总结旧消息 → 返回压缩后的 context → 内核替换。
内核留插槽、消费者填内容；换压缩算法不需要动 packages/agent。

## 7. 事件模型与持久化

- runLoop 对外只通过 `emit` 说话（AgentEventSink 回调）。
  Agent 类注入回调 → processEvents 更新内存状态 → 转发给订阅者。
- 消息定稿推送点共三处：流式助手消息收尾（done/error 分支，定稿替换
  半成品之后）、注入循环、emitToolResultMessage。推送的永远是定稿。
- `await emit(...)` 同步等待订阅者跑完才推进循环——"事件已送达"先于
  "循环继续"，顺序性保证。
- **持久化在消费者层**：coding-agent 订阅 message_end，逐条 appendMessage
  写 JSONL（消息粒度）。压缩摘要/bashExecution/分支摘要是批次级，另行落盘。
- 与"返回式"的本质区别：不是 runLoop 返回后上层再持久化，而是运行中
  实时逐条落盘。中途崩溃最多丢一条。投影（持久化侧）永远不落后循环
  超过一条消息——这是 prepareRequest 无条件重建的底气。
- prepareRequest 的投影重建**不比较**，无条件采用持久化侧：权威只有
  一个时，正确做法是"从权威重建"，不是"调和两个副本"。

## 8. 与 go-agent runStreamInternal 的对照

| 维度 | go-agent | pi |
|---|---|---|
| 循环 | 单层 for + maxIter 熔断 | 双层 while，无上限 |
| 运行形态 | 嵌入式批处理（跑完一件事），durable thread 可多次续跑 | 长驻会话（插话驱动，可无限延续） |
| 插话 | steering 队列（已实现：durable FIFO，drain 点在压缩后，按硬上限准入） | steering/follow-up 双队列 |
| 扩展点 | functional options + 内置策略；回合边界已开两个钩子：FinishRound（回合末调度决策）与 RoundContextProvider（每轮请求前的回合上下文） | config 回调钩子（机制全外置） |
| 压缩 | 内置在循环顶部（Compactor 接口） | 外置到 prepareNextTurn 钩子 |
| 重试 | 循环自带（chatWithRetryAndFallback） | 在 pi-ai 的 StreamFn 里 |
| 持久化 | 内核内置（declare/commit 两阶段） | 消费者订阅事件（消息级 JSONL） |
| 持久化原子性 | 回合级（declared/committed 配对 + closeUnknown 修复） | 消息级 |
| 事件 | iter.Seq2 + 7 sealed 变体，TC-first | EventStream + tagged union |
| 错误哲学 | 工具失败 = soft result 回灌 LLM | 同（isError 编码，从不 reject） |

两边最小骨架一致（LLM → 有工具则执行回灌 → 否则结束），
分歧全在服务的运行形态：批处理内核 vs 长驻会话内核。

对照本文写作时（初版）列出的差距，现状更新：

1. ~~steering 队列~~——已实现（steering.go）：durable FIFO（EnqueueInputWithID，
   幂等键 + 准入硬上限），drain 点固定在每轮压缩之后、请求之前，剩余容量不够
   的部分留队等下一轮。与 pi 的轮询式语义对齐（摘取与注入分离）。
2. ~~回合边界以钩子暴露~~——部分实现：FinishRound 钩子（finishround.go，回合末
   调度决策）与 RoundContextProvider（round_context.go，每轮请求前注入回合上下文，
   同时收紧了 canonical history 预算）已落地；Compactor 仍是内置接口而非钩子。
可选：自定义消息类型（等有非 LLM 内容入历史的场景再做）、follow-up 队列
（steering 已覆盖当前需求）。
不需要补的：maxIter/重试/持久化内置——是嵌入式定位的优势，非缺失。

## 9. 阅读内核的通用方法

1. 先读调度时序（runLoop 的循环骨架），再读业务回调——框架的控制流
   是倒置的，钩子的执行时序藏在框架里。
2. 认"请求边界"：所有修改类机制的合法性都来自这里。
3. 认"定稿点"：message_end 是内存入账、事件推送、持久化三者的汇合点。
4. 对照测试读：`packages/agent/test/agent-loop.test.ts` 每个用例都是
   一种输入下的确定行为。
