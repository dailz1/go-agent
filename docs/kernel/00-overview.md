# 00 · 内核全景（总览）

> 内核文档集第 0 篇。本文给出 go-agent 内核的整体地图：四个内核包的职责切分与依赖方向、一次 `Run` 从入口到 `DoneEvent` 的跨包数据流、`Provider` / `Tool` / `Store` 三个接缝各自的隔离目标、密封接口的设计纪律。每一节只讲"是什么、为什么"，细节一律指向同目录的编号专篇（见文末索引）与既有专项文档。
> 行号锚点基于 2026-10-29 工作区（基线 65fea54，含 provider 热换改动），与 10-agent-loop.md 同批；接口契约以 docs/DESIGN.md 为准，两者冲突时以 DESIGN.md 为准。

## 1. 一句话定位

go-agent 是一个**可嵌入的 Go agent 运行时内核**：一个零第三方依赖的库，把"大模型思考 → 调用工具 → 结果回填 → 继续思考"这个循环跑对、跑稳，让使用者在自己的进程里构建可靠的 AI agent（docs/DESIGN.md:6-9）。它不是框架、不是云服务、不绑定模型厂商、不含入口程序（docs/DESIGN.md:11-17）。设计目标按优先级是：稳定 > 可替换 > 小 > 好用 > 留口子（docs/DESIGN.md:19-27）——后面每一节解释"为什么这么切"时，答案几乎都能落回这五条（docs/DESIGN.md:23-27）。

对"内核"一词的精确界定：`agent` / `llm` / `store` / `tool` 四个包及其编译闭包。仓库里的其他目录（`agenttest`、`agenttool`、`skills`、`mcp`、`harness`、`examples`、`codexauth`）是卫星层，不属于内核（§2.3）。

## 2. 四包切分与依赖方向

### 2.1 内核四包

| 包 | 职责（机制，不是策略） | 关键符号 |
|---|---|---|
| `agent` | 自主循环本体、事件流、审批、重试、压缩/截断、线程持久化生命周期、运行时控制面（steering / 配置热更新 / FinishRound） | `Agent` / `New`（agent/agent.go:109 / :278）、`runStreamInternal`（agent/agent.go:388）、`AgentEvent` 七密封变体（agent/event.go:15） |
| `llm` | Provider 接缝、provider-agnostic 的 `Message` / `Chunk` 密封联合、共享 HTTP + SSE 管道、重试原语、`APIError`；适配器子包 `glm` / `openai` / `openairesponses` / `codex` | `Provider`（llm/provider.go:19）、`Message` / `ContentBlock`（llm/message.go:22 / :29）、`Chunk`（llm/chunk.go:20） |
| `store` | 线程持久化后端契约：append-only 记录日志、版本化信封、checkpoint 加速器；JSONL 与内存两个实现 | `Store`（store/store.go:113）、`Record` 与 kinds（store/store.go:83 / :42-52）、`NewJSONL` / `NewMemory`（store/jsonl.go:69 / store/memory.go:18） |
| `tool` | 工具接缝、软错误 `ToolResult`、并发安全 `Registry`、递归 `ParameterSchema` 及其语义保真 codec | `Tool`（tool/tool.go:14）、`ToolInfo` / `ParameterSchema`（tool/tool.go:33 / :49）、`Registry`（tool/registry.go:20） |

切分的逻辑是"每个包守一个接缝加它的机制"：`agent` 是唯一会"做决策"的包（循环调度、终态判定）；`llm` / `store` / `tool` 各自是 `agent` 通向外部世界的一个孔（模型厂商、持久化介质、能力实现），孔后面只放机制。策略（压缩算法、审批 UI、工具实现、存储选型）全部在包外或以回调注入——这是设计原则第一条"内核装机制，扩展包装策略"（docs/DESIGN.md:58）的物理形态。

### 2.2 依赖方向是产品的一部分

四个包构成一个严格单向的 DAG：

```
agent ──→ llm ──→ tool        （agent 同时直接依赖三者）
  │        │
  │        └─ 只用 tool.ToolInfo（向模型描述工具，不执行）
  └──→ store ──→ llm          （store 只用 llm.Message 作 checkpoint 载荷，不碰 Provider）
  └──→ tool
```

每条边的穿越类型都被刻意收窄：`llm → tool` 只依赖 `tool.ToolInfo`（描述，不含执行）；`store → llm` 只依赖 `llm.Message`（数据，不含 provider 调用）。没有反向边，也没有环——所以"换掉任何一个孔的实现"都不会波及其他三个包，这是可替换目标（docs/DESIGN.md:24）的结构保证。

更硬的一层约束是**内核零第三方依赖**：`agent` / `llm` / `store` / `tool` 四包的完整编译闭包不得含任何第三方 module。这不是口头约定，由 scripts/kernel-closure-guard.sh 机械断言：

```bash
go list -deps ./agent ./llm ./store ./tool   # 非本 module 的依赖必须为空
go list -m github.com/modelcontextprotocol/go-sdk   # 必须 v1.7.0 direct（供 mcp/ 卫星使用）
# SDK import 只允许出现在 mcp/ 下的 packages，其余 importer 一律报错
```

为什么值得花一条 CI 守卫：go-agent 的形态是"使用者进程里的库"，依赖闭包就是使用者的供应链与审计面；stdlib-only 让"嵌入"没有隐性代价（DESIGN.md 目标三"小"，docs/DESIGN.md:25）。唯一的例外被显式圈起来——MCP 官方 SDK 允许（且只允许）`mcp/` 卫星导入，根 go.mod 把版本钉死在 v1.7.0，防止间接漂移。内核的四包连这个例外都不沾。

适配器子包（`llm/glm`、`llm/openai`、`llm/openairesponses`、`llm/codex`）**在内核闭包之内**，同样 stdlib-only。glm 与 openai 的转换助手刻意逐字重复（项目反模式清单："Deduplicating glm/openai conversion helpers — duplicated by design"）：协议差异本质上是条目级的，抽公共层会把两个协议耦合成第三个不存在的东西；改行为时两处都改是接受的代价。

### 2.3 卫星层：依赖只指向内核

| 卫星 | 干什么 | 入口 |
|---|---|---|
| `agenttest` | 公开确定性测试替身（脚本化 Provider、录制/回放），让使用者零成本测自己的 agent | `ScriptedProvider`（agenttest/scripted_provider.go:77） |
| `agenttool` | 把子 Agent 包成一个 Tool：每次 Execute 一次无状态子 `Run`，只回传最终文本 | `New`（agenttool/agenttool.go:55） |
| `skills` | 技能目录扫描（SKILL.md frontmatter 子集）→ 目录分发器 Tool | `Load` / `Set.Tool`（skills/skills.go:76 / :113） |
| `mcp` | MCP 客户端桥（包名 `mcpbridge`），远程工具经 SDK 进 Registry；全仓库唯一的 SDK importer | `Connect`（mcp/bridge.go:47） |
| `harness` | 业务层参考应用（TUI 编码助手），消费内核公开 API，可引入 Bubble Tea 等 UI 依赖；不属于内核 | 契约见 harness/DESIGN.md |
| `examples` / `codexauth` | 演示入口与 Codex OAuth 交互登录卫星 | `examples/` 各 main |

箭头全部从卫星指向内核，反向不存在。这个方向性有两层含义：内核公开 API 的任何演化都会立刻被卫星消费暴露出来（卫星是最好的兼容性测试床）；而卫星引入的任何依赖（Bubble Tea、MCP SDK）都被 kernel-closure-guard 挡在闭包外。"依赖形状是产品"（项目 UNIQUE STYLES）说的就是这件事。

## 3. 一次 Run 的端到端数据流

本节给全景：从使用者调用 `Run` / `RunStream` 开始，到拿到 `DoneEvent` / `RunResult` 结束，数据怎么跨包流动。每一段的完整契约在编号专篇里，这里只标注流转方向与入口。

```
使用者
  │  Run(ctx, input, opts...)          agent/agent.go:335
  │  RunStream(ctx, input, opts...)    agent/agent.go:384
  ▼
agent.runSequence                      agent/persist_api.go:63   唯一分岔点
  ├─ 无 WithThread → 临时运行：history = [system?, user]
  └─ 带 WithThread → 线程运行：openSession（agent/persist.go:40）
        → replayThread 重放日志（agent/persist_replay.go:86）
        → run_started 落库（store.Record, store/store.go:83）
  ▼
agent.runStreamInternal                agent/agent.go:388   唯一执行路径，惰性 iter.Seq2
  │
  │  每一轮顶部固定顺序（控制面三机制全部挂在这条线上，docs/DESIGN.md:244）：
  │    读配置 → 压缩 → drain（steering）→ 预算检查 → 请求
  │      读配置: refreshConfig（agent/persist.go:205）+ configState 折叠
  │              （agent/persist_dto.go:184-204），provider 解析 + 热切换
  │      压缩:   compactHistoryRound（agent/agent.go:757）→ Compactor.Compact
  │              （agent/compact.go:49）→ checkpoint → CompactionEvent
  │      drain:  sess.drainInputs 注入排队的插话（agent/steering.go）
  │
  ├─ agent.chatWithRetryAndFallback    agent/provider_call.go:13   重试/降级边界见 10 篇 §5
  │     ▼
  │  llm.Provider.ChatStream           llm/provider.go:19
  │     ▼  适配器（llm/openai/adapter.go:50 / glm:104 / openairesponses:56 / codex）
  │        出站转换 Message→厂商 wire，HTTP + SSE（llm/http.go、llm/sse.go）
  │     ▼
  │  Chunk 流（六密封变体, llm/chunk.go:20）
  │     ▼  accumulator 累积（agent/accumulator.go:27）
  │        Text/Thinking delta → 实时 yield AgentEvent（TextDelta/ThinkingDelta）
  │        ToolCall/ReasoningItem chunk → 只进 accumulator，不发事件
  │
  ▼  assembleAssistantMessage（agent/agent.go:836）→ history += assistant
  │
  ├─ 无工具调用 → 终态：finishRun / closeRun 先落库
  │     → yield(DoneEvent)             agent/event.go:98   History 为权威快照
  │
  └─ 有工具调用 → declareRound 先落库（agent/persist.go:303，任何宣告/审批/执行之前）
        → ToolCallEvent 宣告批（TC-first，细节见 10 篇 §3.2）
        → executeToolRound             agent/tool_execution.go:77
  │        tool.Registry.Get（tool/registry.go:98）→ 审批（fail-closed）
  │        → tool.Tool.Execute（tool/tool.go:14）
  │        → 结果截断（agent/truncate.go，约窗口 30%）
  │        → settlement 按声明序物化 → ToolResultEvent
        → commitRound 落库（agent/persist.go:355）→ 下一轮
  │
  │  最后一轮仍请求工具（maxIter 终局）：truncateRun 持久预闭合
  │  （agent/persist.go:373）+ iterationLimitResults（agent/max_iter.go:8）
  │  → TC×N → skipped TR×N → DoneEvent(Truncated=true)
  ▼
DoneEvent.History（权威快照）
  ├─ RunStream 消费者：直接 range 事件流
  └─ Run 消费者：foldRunStream 归并       agent/agent.go:343 → *RunResult
```

几个跨包的关键交接点，值得单独点出：

1. **单执行路径**。`Run` 和 `RunStream` 不是两个引擎：前者是后者事件流的归并（`foldRunStream`，agent/agent.go:343），P0 裁决"流是执行原语，同步执行是衍生物"（docs/DESIGN.md:66、:85）。循环本体的完整状态机见 10-agent-loop。
2. **`llm` 边界上的两次形变**。出站：provider-agnostic `[]llm.Message` + `[]tool.ToolInfo` 在适配器内转成厂商 wire；入站：厂商 SSE 事件流转成密封 `Chunk`。跨过这条线，`agent` 包对"这是哪家模型"零感知——包括线程级 provider 热切换（`WithProviders` agent/agent.go:227 + `SetThreadConfig` agent/config.go:59）也不需要循环知道任何厂商细节，它只是在轮顶换了一个 `Provider` 实例。
3. **`tool` 边界上的软/硬分流**。工具失败若以 `ToolResult{Status: ResultError}` 返回（软错误），结果截断后回灌给模型自我纠正；若以 Go error 返回（硬错误），循环中止。这条二元契约（tool/tool.go:25-35 注释）是"模型可恢复的失败"与"系统级故障"的分界线，详见 31-tool。
4. **`store` 边界上的先落库后行动**。线程运行里每个外部可见动作之前都有一次持久化：declareRound 先于任何工具宣告/审批/执行（agent/persist.go:303）、压缩视图先 checkpoint 再发 CompactionEvent、终态先落库再发 DoneEvent。落库的形态是 `store.Record`（store/store.go:83）的版本化信封；生命周期 kinds 的语义归 agent 层所有，`store` 只管字节与一致性（§4.4）。恢复、结算（`ResumeThread` agent/persist_api.go:105、`SettleThread` agent/settle.go:58）的完整语义见 12-agent-persistence。
5. **事件流的终点是权威快照**。`DoneEvent.History` 是完整无损的历史快照（深拷贝交付），消费者不得仅靠增量事件重建含 reasoning items 的历史（docs/DESIGN.md:87）；折叠契约与七种事件的发射点见 10-agent-loop §3。

## 4. 三个接缝：Provider / Tool / Store

### 4.1 什么算接缝

DESIGN.md 的核心契约表（docs/DESIGN.md:45-53）列了四项：`llm.Provider`、`tool.Tool`、`AgentEvent` 事件流、`Store`。其中三项是**接缝**——内核通过它们调用外部世界、且允许多个由使用者提供的实现替换：换厂商、换工具、换存储，业务代码不改（可替换目标）。判据有三条：接口的实现在使用者手里；数据流是"内核调出"而非"使用者推入"；替换不要求改内核其他包。`AgentEvent` 不满足后两条（它是内核单向外发的观测面，事件流只读——"观察用事件，改行为用包装"，docs/DESIGN.md:59），所以它是契约但不是接缝。

### 4.2 `llm.Provider`：隔离厂商协议差异

```go
type Provider interface {                                    // llm/provider.go:19
    Name() string
    Chat(ctx, messages, tools, opts...) (*Message, *Usage, error)
    ChatStream(ctx, messages, tools, opts...) (iter.Seq2[Chunk, error], error)
}
```

隔离目标：**全部厂商协议差异被吸收在这一个接口后面**。内核与使用者只讲 provider-agnostic 的 `Message` / `ContentBlock`（llm/message.go:22 / :29）与 `Chunk`（llm/chunk.go:20）；wire 格式、鉴权方式（含 GLM 的 JWT 嗅探）、SSE 事件形态、reasoning 条目的编码，全在适配器子包内。三个设计支撑点：

- **最小方法面**。`Chat` 与 `ChatStream` 分立而非合一，让"不支持流式"成为一个可表达的降级信号（`ErrStreamingNotSupported`，llm/provider.go:14）：循环在同一次尝试内降级到 `Chat` 并把完整回复合成回放为合规 chunk 流，对上层完全透明（细节见 10-agent-loop §5.4）。`Name()` 让 provider 成为**可命名的线程级资产**——热切换（`ConfigChange` agent/config.go:34）按名字在注入表里解析，解析不到就 `ErrUnknownProvider`（agent/config.go:26）fail-closed，绝不静默回退。
- **统一错误叶子**。`*llm.APIError`（llm/errors.go:9）是叶错误（无 Unwrap），`Retryable()`（llm/errors.go:23）按 429/5xx 判定。全部适配器把厂商错误如实折算成它，重试分类才能住在循环层统一做（重试预算是"花费不能失控"的内核职责，不该每家适配器各算各的）。
- **密封消息模型**。`ContentBlock` / `Chunk` 是密封联合（§5），协议新字段的吸收是 llm 包内部决策；跨厂商回放历史时的降级规则（谁跳过外来 reasoning 块、谁原样回放）落在各适配器的出站转换层，循环与 store 零参与（契约表见 docs/DESIGN.md §运行中控制面的降级契约）。

### 4.3 `tool.Tool`：隔离能力来源

```go
type Tool interface {                                        // tool/tool.go:14
    Info() ToolInfo        // 冻结入注册表：名字、schema、RequiresApproval
    Execute(ctx, args) (*ToolResult, error)
}
```

隔离目标：**能力的实现与来源对内核不可见**。自研函数、MCP 远程工具（mcp/bridge.go:47）、子 agent（agenttool/agenttool.go:55）、技能目录（skills/skills.go:113）全部以同一个 `Tool` 形态进 `Registry`——`agent` 包不知道也不关心一个工具是本地闭包还是另一台机器上的进程。三个支撑点：

- **软/硬错误二元**（tool/tool.go:25-35）。软错误 `ToolResult{Status: ResultError}` 回灌模型；Go error 中止循环。这条分界线同时定义了审批拒绝、未知工具、panic 恢复的归属——它们都是回灌的软结果，不是系统错误。若没有这条线，"工具参数写错"和"数据库连不上"会被迫挤进同一个错误通道，模型失去自我纠正的机会，或者系统故障被误当可重试输入。
- **描述与执行分离 + 注册期冻结**。`Info()` 在 `Register`（tool/registry.go:79）时冻结入表，`List`（tool/registry.go:110）只返回排序的冻结深拷贝、永不回调用户代码——这让"运行中改注册表"（`Unregister` / `Replace` / `Apply`）与在飞执行并发安全，工具快照语义才说得清。
- **schema 是忠实载体不是验证器**。`ParameterSchema`（tool/tool.go:49）的 codec 保真搬运递归结构与未类型化 keyword（保序），但内核从不拿它验证模型给的实参——args 原样穿透给 `Execute`。语义保真的理由与 codec 细节见 31-tool 与 docs/TOOL-DESIGN.md。

### 4.4 `store.Store`：隔离持久化介质

```go
type Store interface {                                       // store/store.go:113
    Append(ctx, thread, expected, records...) (int64, error)   // 乐观并发 + 幂等重试
    Latest(ctx, thread) (ThreadState, error)                    // 最新 checkpoint + 增量尾
    History(ctx, thread, from) ([]Record, error)                // 全量回退读
    Delete(ctx, thread) error
}
```

隔离目标：**介质（JSONL 文件 / 内存 / 未来的数据库）与生命周期协议解耦**。切分方式是"agent 拥有协议，store 拥有字节"：记录 kinds 的语义（`round_declared` 是什么、何时写）、declare→commit 的顺序、恢复时如何合成 unknown 结果，全部住在 agent 的 persist*.go；`store` 只负责追加、校验、加速读取。三个支撑点：

- **日志是真相，checkpoint 是可重建加速器**（store/store.go:8-10 注释）。checkpoint 只携带 canonical history 快照且定位在自身 Seq；不信快照时 `History` 全量回放永远可用。因此 checkpoint 可以丢弃、可以升级 codec_version，而不会出现"真相丢失"。
- **乐观并发 + 全内容幂等**。`Append` 以 expected head 做乐观锁；崩溃后重试同批次时，已落库前缀必须与重试内容逐字节一致（ID/Kind/Schema/Payload）才被接受，否则 `ErrRevisionConflict`（planAppend，store/store.go:180）。"不确定写"因此可以安全重试而不产生重复记录——这是整个持久化契约的地基。两个后端共用 `prepareBatch` / `planAppend`（store/store.go:153 / :180），语义不可能分叉。
- **版本化信封，fail-loud 兼容**。未知 kind、未来 schema 版本返回类型化兼容错误（`ErrUnsupportedSchema` / `ErrIncompatibleLog`），不静默忽略——旧二进制读到新记录必须报错，不能假装理解（设计依据 docs/DESIGN.md P1"版本化事件信封"）。

### 4.5 不是接缝的东西

`Compactor`（agent/compact.go:49）、`ApprovalFunc`（agent/agent.go:105）、`RoundContextProvider`（agent/round_context.go:19）、`WithFinishRound` 的钩子（agent/finishround.go:65）都是接口/回调，但不是接缝：它们只消费内核类型、数据流单向，不引入新的依赖方向，实形态是策略注入点（"内核装机制，扩展包装策略"里策略那一侧）。区分的意义在于演化责任：接缝的变更牵动所有使用者的替换成本，必须走 DESIGN.md 契约表；策略回调可以按需增删，只要默认行为不变。

## 5. 密封接口设计纪律

### 5.1 机制：未导出 marker method

内核有三层密封联合，机制相同——接口含一个未导出方法，只有本包类型能实现：

| 联合 | 接口 | marker | 变体 |
|---|---|---|---|
| 事件 | `agent.AgentEvent`（agent/event.go:15） | `eventType() string` | 7 种：TextDelta / ThinkingDelta / ToolCall / ToolResult / Retry / Done / Compaction |
| 内容块 | `llm.ContentBlock`（llm/message.go:29） | `blockType() string` | 6 种：Text / Reasoning / Image / ToolUse / ReasoningItem / ToolResult |
| 流片段 | `llm.Chunk`（llm/chunk.go:20） | `chunkType() string` | 6 种：TextDelta / ReasoningDelta / ToolCallStart / ToolCallArgs / ReasoningItem / Done |

为什么密封而不是开放接口：**变体集的演化必须是内核内部、docs-first 的决策**。open interface 允许任何使用者在事件流里塞进自己的类型，consumer 的 type switch 从此不可能穷尽，事件语义（哪些事件标记轮次边界、哪个事件携带权威快照）会在生态里碎掉。密封后，"加一个事件类型"天然要求改 DESIGN.md、改本包、改所有 default 分支的语义——演化成本前置且可见。附带的工程事实：密封类型不能直接 JSON 往返，持久化因此必须走显式版本化 DTO（`eventEnvelope`，agent/persist_dto.go:28；依据 docs/DESIGN.md P1"显式 DTO 编解码"），这层"麻烦"换来了落库格式的显式契约。

### 5.2 两个默认分支，一严一宽

消费者对未知变体的处理是**有意不一致**的：

- **事件层从严**：`foldRunStream` 对未知 `AgentEvent` 直接报错（"agent stream emitted unknown event"，agent/agent.go:367）。事件是行为语义，装作没看见等于静默丢行为。
- **chunk 层从宽**：流式累积循环对未知 `Chunk` 静默忽略、向前兼容（agent/agent.go:516）。chunk 是协议事实，服务器加新字段不该打断旧内核。

判据是"这层的变体增长由谁负责"：chunk 变体随厂商协议长（外部世界的事，宽容）；事件变体随内核契约长（自己的事，从严）。写新消费代码时先问自己站在哪一层。

### 5.3 value 与 pointer 双形态纪律

三个 marker 都是**值接收者**（如 `func (TextBlock) blockType() string`，llm/message.go:39）。Go 的方法集规则因此允许 `T` 与 `*T` 同时满足接口——`[]ContentBlock` 里可以混存值形态与指针形态，而且两种形态在实践中都真实出现：

- 构造器与 JSON 解码产出**值形态**：`unmarshalContentBlock`（llm/message.go:192）按 type 判别符解出 `TextBlock{}` 等值；`SystemMessage` / `UserMessage` 等构造器同理。
- 使用者历史、中间件包装、跨适配器回放可能持有**指针形态**：适配器契约明确要求"值与指针形式都支持"（如 codex 的入站映射，docs/DESIGN.md §Codex 适配器）。

由此产生一条硬纪律（项目 CONVENTIONS："type switches must handle value AND pointer forms"）：**任何对密封联合的 type switch 必须同时列出 `T` 与 `*T` 两个 case**。仓内范本是 `deepCopyMessages`（agent/agent.go:35）——对每个可变块同时处理 `llm.TextBlock` 与 `*llm.TextBlock`、`llm.ToolUseBlock` 与 `*llm.ToolUseBlock`……漏掉一个形态不会编译报错（default 分支会吞掉它），只会让深拷贝静默共享底层切片/指针，酿成跨 run 的数据别名 bug。所以这条纪律靠两层保证：写 switch 时成对列 case；测试里显式构造两种形态各跑一遍（持久化与适配器的 round-trip 测试均覆盖指针形态）。

## 后续文档索引

内核文档集按编号分层：10–14 讲 agent 包（循环与它的四个专题面），20–21 讲 llm 包，30–31 讲 store / tool 包，40–41 是横切的不变量与决策索引。本文撰写时同目录已落盘 10-agent-loop.md，其余按此编号规划补齐；编号文档未覆盖的专题以既有专项文档为准。

| 编号 | 文档 | 一句话 |
|---|---|---|
| 10 | [10-agent-loop.md](10-agent-loop.md) | 自主循环：单执行路径、轮状态机、七密封事件、maxIter 终局、重试边界 |
| 11 | 11-agent-tools | 工具执行模型：审批、串行/并行槽位、软硬错误分流、结果截断 |
| 12 | 12-agent-persistence | 持久化与结算：记录协议、重放恢复、ResumeThread / SettleThread、run_cancelled |
| 13 | 13-agent-context | 上下文管理：压缩策略链、预算检查、工具结果截断、round context overlay |
| 14 | 14-agent-control-plane | 运行时控制面：steering 队列、配置热更新（provider 热切换）、FinishRound |
| 20 | 20-llm-model | 消息与事件模型：Message/ContentBlock/Chunk 密封联合、ReasoningItem 语义 |
| 21 | 21-llm-transport | HTTP 与 SSE：共享管道、有界 I/O、重试原语、APIError 分类 |
| 30 | 30-store | 持久化后端：Store 契约、JSONL/内存实现、checkpoint 加速器、并发与幂等 |
| 31 | 31-tool | 工具与 schema：Tool 接缝、Registry 冻结语义、ParameterSchema 保真 codec |
| 40 | 40-invariants | 内核不变量：单路径、durable-before-visible、TC-first、折叠契约、闭包守卫 |
| 41 | 41-decisions | 设计决策索引：历次裁决的动机、备选与后果，交叉引用 DESIGN.md |

专题深读（专项文档，编号文档不取代）：总体契约 docs/DESIGN.md（接口变更先改这里）；控制面裁决 docs/CONTROL-PLANE-DESIGN.md 与 docs/control-plane-decision-record.md；工具与 schema docs/TOOL-DESIGN.md；MCP 桥 docs/MCP-DESIGN.md；技能加载 docs/SKILLS-DESIGN.md；循环与 pi 的对照 docs/pi-agent-loop.md；卫星契约 harness/DESIGN.md。
