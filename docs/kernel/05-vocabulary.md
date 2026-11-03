# 05 · 执行层级词汇表与文档地图

> 内核文档集第 5 篇。本文回答两个跨文档的元问题：其一，描述内核执行过程时用的层级词汇（Thread / Run / Trace / Round / Turn）各自精确指什么、彼此什么关系、与其他框架（pi、LangChain 系）的同名概念如何对应——12 篇编号文档各自使用这些词时含义以本文为准；其二，给出文档集的阅读地图：每篇讲什么、按什么顺序读、与 pi 源码解读系列（my-notes `pi/源码解读/typescript`，10 章）如何对应。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），与其他编号文档同批。与既有专项文档的关系：循环机制的 pi 对照细节在 docs/pi-agent-loop.md，本文只收词汇与地图，不重写机制。

## 1. 执行层级：四层嵌套，两套名字

内核的执行过程是严格的四层嵌套，每层有持久化或事件流上的物理形态：

```
Thread（线程，跨 Run 持续存在）
├── Run 1（一次 runSequence 分派 → runStreamInternal → DoneEvent）   = pi 的 Trace
│   ├── Round 0..k（for 循环一圈）                                    = pi 的 Turn
│   │   ├── 一次模型调用（chatWithRetryAndFallback，agent/provider_call.go:13）
│   │   └── 本次调用产出的全部工具执行（TC-first 宣告 → 执行 → TR）
│   └── Round k+1..（模型不再请求工具时 run 结束，agent.go:535 起）
└── Run 2（ResumeThread，agent/persist_api.go:105）
    └── Round k+1, k+2, ...（编号接着 Run 1 往下数，不从 0 重数）
```

### 1.1 各层定义与锚点

| 层 | 定义 | 物理形态 | 锚点 |
|---|---|---|---|
| **Thread** | 一个持久化会话的完整生命周期：多次 Run 共享同一份 append-only 记录日志 | Store 里一个 thread 下的全部 Record（store/store.go:83） | `WithThread`（agent/persist_api.go:32）、`ResumeThread`（agent/persist_api.go:105） |
| **Run**（≈ Trace） | 一次 `Run` / `RunStream` 调用从入口到终态（DoneEvent / 错误 / maxIter 截断）的完整过程。两入口折叠到同一执行路径 | 一段 run_sequence：`run_started` 起、`run_closed` / `run_cancelled` / 截断终局止 | `runSequence`（agent/persist_api.go:63）、`runStreamInternal`（agent/agent.go:388）、`DoneEvent`（agent/event.go:97） |
| **Round**（≈ Turn） | for 循环一圈：恰好一次模型调用 + 该次调用请求的全部工具执行。轮次边界是控制面的固定结算点（热换 → 压缩 → steering 注入 → 预算检查，按此顺序，agent/agent.go:417-436） | 每圈经 `declareRound` 先落库（agent/persist.go:303），带全局连续编号 | 循环体 agent/agent.go:398 起；`RoundContextRequest{Round, ThreadID}`（agent/round_context.go:23） |
| **工具批** | 一个 Round 内模型同一次回复请求的全部工具调用（并行或串行执行，见 11-agent-tools） | TC-first 宣告批 + 对应 ToolResultEvent 序列（agent/event.go:36-44） | 宣告批 agent/agent.go:526-543 |

两条命名纪律：

- **Round = Turn，且是跨 Run 连续编号的**。`sessionRoundBase`（agent/agent.go:901）返回恢复起点偏移：新会话 `roundBase=0`（agent/persist.go:115），ResumeThread 时 `roundBase = lastRound + 1`（agent/persist.go:92）——恢复后的第一圈接着断点编号往下走。这样持久化日志里的 round 编号在 Thread 生命周期内全局递增，恢复前后不会混淆。pi 没有这一层：它的 Turn 只活在单次运行的事件流里，会话树（Session Tree）是另一个独立概念。
- **Trace 不是 go-agent 的词，Run 是**。本文与其他文档写 Run；与 pi 文档对照时 Run ↔ Trace、Round ↔ Turn。pi 系列第 3 章的定义：Trace = 一次完整运行（agent_start 到 agent_end），Turn = 一次模型调用加它触发的全部工具执行——与 Run / Round 逐字吻合。

### 1.2 跨框架对照词表

| 概念 | go-agent | pi（pi-mono） | 说明 |
|---|---|---|---|
| 会话 | Thread | Session（Session Tree） | pi 的会话是树形（回退=移动 leafId，分支=回退后追加）；go-agent 是线性 append-only log + run_sequence，回退/分叉语义不同（30-store、12-agent-persistence） |
| 一次完整运行 | Run | Trace | pi 有显式 `agent_start` / `agent_end` 事件对；go-agent 的对应物是 run_started / run_closed 记录与 DoneEvent |
| 轮次 | Round | Turn | pi 有显式 `turn_start` / `turn_end` 事件对；go-agent 的轮次边界是隐式的（TC-first 宣告批 + round top 结算顺序），但持久化层面 Round 是一等实体且跨 Run 编号，比 pi 更硬 |
| 模型调用 | `chatWithRetryAndFallback`（agent/provider_call.go:13） | `streamAssistantResponse()` | go-agent 唯一 provider 触点，含预流重试与 fallback；pi 在步骤 B 内 |
| 运行中插话 | steering 队列（agent/steering.go，`EnqueueInputWithID`） | `steer()` | 收集在轮次末、注入在下一 Round 顶部发请求前（drain 在 agent.go:450，压缩之后）；pi 同位（turn_end 收集、下一 turn 顶部注入） |
| 完成后追加任务 | 无独立队列；可由 FinishRound Continue（agent/finishround.go）+ steering 队列拼出 | `followUp()` | pi #403 拆分：steer 抢轮次出口、followUp 抢运行出口（同一 Trace 内续命）；区分维度是投递时机，不是发送方（TUI 里 Enter=steer、Alt+Enter=followUp） |
| 回合末决策 | `FinishRound`（agent/finishround.go:65） | 无直接对应（`shouldStopAfterTurn` 最接近） | go-agent 可在模型不再请求工具时决定 Default 自然关 / Continue 续命 / End 强制关 |
| 轮次上下文注入 | `RoundContextProvider`（agent/round_context.go:19） | 无直接对应 | 每轮带 Round 编号回调，产出 overlay |
| 工具软错误 | `ToolResult{Status: ResultError}` 回灌（tool/tool.go:25-35） | "永不抛出，错误也是一条消息" | 同构；pi 的五步管道多 before/after 钩子层 |
| 上下文压缩 | 压缩策略链（agent/compact.go），80% 窗口触发 | Compaction（切割点算法 + 结构化摘要模板） | 两者都在轮次间触发；算法不同（13-agent-context vs pi 第 9 章） |

## 2. 文档集阅读地图

### 2.1 编号即依赖顺序

编号分层：10–14 讲 agent 包（循环与四个专题面），20–21 讲 llm 包，30–31 讲 store / tool 包，40–41 横切。首读顺序建议 00 → 10 → 40 → 其余按需；改动某一机制前读对应编号篇 + 41 里该机制的决策条目。

| 编号 | 主题 | 对应 pi 章节 | 一句话 |
|---|---|---|---|
| 00 | 内核全景 | 第 1、2 章 | 四包切分、依赖方向、三接缝、密封纪律 |
| **05**（本文） | 词汇表与地图 | 第 3 章第 2 节 | 执行层级命名、跨框架对照、阅读顺序 |
| 10 | agent 循环 | 第 3 章（Agent Loop） | 单执行路径、轮状态机、七密封事件、maxIter 终局、重试边界 |
| 11 | agent 工具执行 | 第 5 章（工具系统） | 审批、串/并行槽位、软硬错误分流、结果截断 |
| 12 | agent 持久化 | 第 10 章（会话管理） | 记录协议、重放恢复、Resume / Settle、run_cancelled |
| 13 | agent 上下文 | 第 8、9 章（上下文工程 / 压缩） | 压缩策略链、预算检查、工具结果截断、round context overlay |
| 14 | agent 控制面 | 第 3 章 steering 部分 + pi 无对应 | steering 队列、配置热更新、FinishRound |
| 20 | llm 消息与事件模型 | 第 4、6 章（模型调用 / 消息系统） | Message / ContentBlock / Chunk 密封联合、ReasoningItem 语义 |
| 21 | llm 传输 | 第 4 章传输部分 | HTTP + SSE 共享管道、有界 I/O、重试原语、APIError 分类 |
| 30 | store 后端 | 第 10 章 JSONL 部分 | Store 契约、JSONL/内存实现、checkpoint 加速器、幂等 |
| 31 | tool 接缝 | 第 5 章类型层 | Tool 接缝、Registry 冻结语义、ParameterSchema 保真 codec |
| 40 | 内核不变量 | 无（pi 无此横切篇） | 单路径、durable-before-visible、TC-first、折叠契约、闭包守卫 |
| 41 | 决策索引 | 无 | 历次裁决的动机、备选与后果 |

### 2.2 两套文档的分工

pi 源码解读系列（my-notes `pi/源码解读/typescript`）与内核文档集服务不同读者：pi 系列是叙事教程（认知路径、生活类比、"下一站"），适合建立直觉；编号文档是设计契约（机制细节、file:line 锚点、被否决备选），适合改动前查证。对照阅读的固定姿势：先 pi 章节建直觉，再编号文档查契约，差异点查 docs/pi-agent-loop.md（循环机制的逐段对照）与 41-decisions（决策动机）。pi 有而本仓库刻意没有的两块：pi-tui（UI 属业务层，harness 边界之外）与 pi-orchestrator（多 agent 编排，本仓库以 agenttool 子 agent 即工具的方式覆盖其最小子集）。

pi 章节里的概念凡与本文 §1.2 词表冲突的，以本文为准；pi 章节里描述的 pi 源码行为（如新版 `steer()` 的"跳过剩余工具"投递语义）只描述 pi 自身，不构成对 go-agent 的契约描述——go-agent steering 的契约在 14-agent-control-plane。
