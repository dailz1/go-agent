# go-agent 设计文档

> 状态：v0.1（2026-09-10）
> 本文档是项目的设计契约：定位、目标、原则、路线图。改接口先改这里。

## 1. 项目定位

**go-agent 是一个可嵌入的 Go agent 运行时**：一个零依赖的库，负责把"大模型思考 → 调用工具 → 结果回填 → 继续思考"这个循环跑对、跑稳，让使用者在自己的进程里构建可靠的 AI agent。

与相邻事物的边界：

- **不是框架**：不提供编排图、工作流引擎、记忆系统——这些是上层或扩展包的事；
- **不是云服务**：与云托管 agent 运行时（Claude Managed Agents、Codex cloud 等）互补——它们跑在云上，我们跑在用户进程里；
- **不绑定模型厂商**：任何 OpenAI 兼容 API、GLM 均可即插即用；
- **不含入口程序**：内核没有 `package main`；`examples/` 目录下的演示客户端独立于内核，仅供演示与上手，不是库的一部分。

## 2. 设计目标

按优先级排列，冲突时后者让位于前者：

1. **稳定**：模型断流、工具崩溃、超长返回、任务失控——库必须兜住。该重试的重试，该截断的截断，该刹车的刹车。用户程序不能挂，花费不能失控。
2. **可替换**：换模型供应商、换工具实现，使用者的业务代码一行不改。
3. **小**：内核只放"不管做什么产品都必须有"的机制；"看产品而定"的能力放扩展包。`agent`、`llm`、`store`、`tool` 的完整依赖闭包不得含第三方 module；SDK source import 只允许出现在根 module 的 `mcp/` packages。CI 用 `go list -deps` 断言该闭包，并逐字节断言根 `go.mod` 直接 require `github.com/modelcontextprotocol/go-sdk v1.7.0`。`tool` schema codec/Registry 只使用标准库（JSON 工具限 `encoding/json`、`fmt`，cycle identity 可用 `reflect`），不引入 validator。
4. **好用**：四个核心概念（Agent / Provider / Tool / Event）即可上手；全程事件流可观测、可录制、可回放；出问题不用猜。
5. **留口子**：今天存储的每笔数据（完整对话、每次工具调用与结果）都为明天的功能（记忆、自进化、审计、训练数据导出）保持完整可取。今天多守的规矩，就是明天新功能的入场券；工具结果按截断策略后的轨迹保存，超长原文不保留（稳定目标的代价，截断时记 Warn 日志）。

## 3. 架构总览

```
┌─────────────────────────────────────────────┐
│ 使用者的应用（业务逻辑、system prompt、工具实现） │
├─────────────────────────────────────────────┤
│ go-agent 内核（本库）                          │
│  agent  自主循环 + 事件流 + 审批 + 重试      │
│  llm    消息模型 / 事件模型 / HTTP·SSE / 重试│
│  tool   工具接口 + 注册表                   │
├─────────────────────────────────────────────┤
│ 适配器：openai（兼容接口）/ glm（智谱，JWT）      │
│          openairesponses（Responses 协议）        │
└─────────────────────────────────────────────┘
```

核心契约（一旦发布不再轻易变更）：

| 契约 | 作用 |
|---|---|
| `llm.Provider` | 接入任意模型供应商 |
| `tool.Tool` | 注入任意能力（自研函数、MCP 工具、子 agent）；参数 schema 为递归 typed fields 加按名称排序的完整词汇 carrier。`agenttool` 是内核外的 satellite composition surface：固定 `input:string`，每次只 `Run` 一次子 Agent、只回传最终文本，不转发子事件；不改 Tool interface。remote bridge 将 SDK `map[string]any` canonical-marshal 后交 `ParameterSchema.UnmarshalJSON` 重建 presence state；fidelity 限 SDK decode surface，Registry 只作 typed graph structural checks、从不解释 carrier，whole-schema raw escape hatch 禁止。 |
| `AgentEvent` 事件流 | canonical agent 行为的观测入口；观测 / UI / 审计 / 回放的唯一入口 |
| `Store`（建设中） | 会话持久化与崩溃恢复：内核自动持久化（`WithStore`/`RunThread`），提交点先落库后行动 |

现有事件类型：`text_delta` / `thinking_delta` / `tool_call` / `tool_result` / `retry` / `done` / `compaction`（与 OpenAI Agents SDK 等业界分类一致）。

## 4. 设计原则

- **内核装机制，扩展包装策略。** 压缩算法、记忆整理、进化方法都是策略；存储接口、事件模型、截断边界都是机制。
- **观察用事件，改行为用包装。** 事件流只读；要改变行为就包装 `Tool` 或 `Provider`（中间件模式），不给 hook 随意改写流量的权限。
  - 包装器守则（P2-1）:
    - 观察型包装器默认透传完整 `ToolInfo`（名称、schema、`RequiresApproval`）。允许有意改名，但注册与广播都按包装体最终 `Info().Name`，注册名与模型可见名必须一致。
    - `Execute` 传递原 `ctx` 与 `args`；软错误 `ToolResult` 不得提升为 Go error，Go error 也不得降级为软结果；包装错误必须 `%w`，保持 `errors.Is/As` 对哨兵与 `*llm.APIError` 的识别（尤其不得吞 `ErrStreamingNotSupported`，否则非流回退与重试分类失效）。
    - Provider 包装器原样传递 messages/tools/options/usage 与外层、迭代内错误；流式包装器不得预先 range，保持 `iter.Seq2` 惰性与早退安全；限流许可横跨迭代器生命周期（自然结束与早退都要释放）。
    - 日志示例只记元数据（名称、ID、长度）或显式截断/脱敏载荷，不逐字复制 prompt/参数/结果。可运行示例见 `examples/middleware`。
    - `agenttool` 是行为包装器：原样传递 ctx；malformed input、子 Agent 截断或无最终文本是软 `ToolResult`，子 `Run` 的 Go error 必须 `%w` 硬传；外层与子层 approval 各自生效，禁止注入子事件。每个 `New` 返回的 adapter 用 context-aware 单槽 gate 串行其子 `Run`；同一 child 的多个 adapter 不互锁，调用方负责共享 Provider、Compactor、Store、Registry 与子 Tool 的并发安全。
- **流是执行原语，同步执行是衍生物。** 只维护一条模型/工具执行路径，`Run` 与恢复 active run 的 `ResumeThread` 是事件流的归并。`SettleThread` 是持久生命周期控制操作；`ResumeThread` 在最后一个 run 已 cancelled 时只读取终态快照，不执行循环，不制造 DoneEvent。
- **默认安全。** 危险工具须审批；工具 panic 不外泄；结果截断；预算可设。
- **每步可靠胜过整体聪明。** 每步 95% 可靠，连跑 10 步只剩 60%（误差复利）。内核的每一分投入都优先花在"每一步更可靠"上。
- **内核不变小、不变胖，只变稳。** Tool schema codec 以 nullable null-first、空值归一和 presence state 作确定性投影；`Properties`/`Items`/AP-schema/composition 的 active-stack cycle 失败关闭。carrier 是有序 `SchemaKeyword` 值，保留非 typed keyword，自己的 raw-JSON 路径拒绝重复键，但绝不解释其 JSON Schema 语义；whole-schema raw escape hatch 禁止。adapter 不得静默降级 schema。remote MCP 的 fidelity 限 SDK decode surface（duplicate key、encounter order 和精确 numeric token 已在 SDK 边界丢失），本地 agenttest v2 raw parser 保持 token 严格。

  外部 keyed construction 的语义：

  | 构造值 | presence / wire 语义 |
  |---|---|
  | `BooleanSchema != nil` | bare boolean schema（`true`/`false`） |
  | `Ref != ""` / `Ref == ""` | 前者输出 `$ref`；后者在无 codec state 时 absent |
  | `Type != ""` / `Type == ""` | 前者输出 type；后者在无 codec state 时维持旧模型零值 wire 行为 |
  | `Nullable:true`、非空 `Properties`、`Items`、AP、composition、carrier | presence；空容器按 omission 处理 |

  `UnmarshalJSON` 是唯一产生 explicit-empty type/description/`$ref` 等 pathological exact-presence state 的路径。

## 5. 路线图

### P0 结构统一（已完成 2026-09-10）
- 合并 Run/RunStream 为单一执行路径（`Run` = 事件流归并）
- 修复 OpenAI 流式重复 `DoneChunk`
- 事件与完整 history：Text/Thinking/ToolCall/ToolResult events 提供实时观察；同一成功 Done 中的 maxIter 工具请求按 declaration order 发出连续 ToolCallEvent，并各有一条 iteration-limit soft-error ToolResultEvent。`DoneEvent.History` / `RunResult.History` 是完整、无损的权威 history snapshot；因为 Responses `ReasoningItemChunk` 当前不映射为 AgentEvent，consumer 不得仅靠增量 events 重建含 reasoning items 的 assistant，必须在 Done 时以该 snapshot reconciliation。普通 Chat-only history 可作 event-only diagnostic fold，但不是通用完整-history 契约。

### P1 运行时安全
- 工具结果截断：按上下文占比设上限（约 30% 规则），掐头留尾，入库前执行（已落地，见 §7）
- 历史预算管理：`Compactor` 策略接口 + 最简实现；压缩单位是"消息组"（工具调用与其结果不可拆分，system 不可动）（已落地，见 §7）
- 会话持久化 `Store`：**内核自动持久化**（`WithStore` 选项 + `RunThread` / `ResumeThread` / `RunThreadStream` / `SettleThread` / `SettlementTarget` 线程入口；未配置零开销：不生成线程 ID、不触碰持久化路径，单一执行路径不变）。数据形状：线程 = 规范记录日志，生命周期记录链每条带稳定 `run_id` / `round_id`：
  - `run_started`：运行输入（逐字节保留，不规范化）+ 线程初始系统提示词（回放以落库为准，与重启后 `WithSystemPrompt` 配置无关，防止历史上下文被静默篡改）；
  - `round_declared`：**原子**轮次宣告——完整助手消息（文本/推理 + 全部有序工具调用），先落库再执行任何工具、先于审批回调；
  - `round_committed`：按宣告顺序的全部模型可见结果（含软失败：审批拒绝、未知工具、nil 结果、恢复的 panic）；结果先截断后落库（与内存视图同源）。成功终态（无工具调用的回复、或 maxIter 截断）在 `DoneEvent` 发出前落库；
  - `run_cancelled`：调用方明确放弃当前未终结 run 的终态，schema=2、固定 RunID 与 record ID。若仍有 open declaration，该单条记录同时保存所有未提交调用的有序 outcome-unknown error 结果；重放原子地追加完整 declaration/results 并关闭 run。没有 open declaration 时只关闭 run。已提交结果不改写；不追加假的 assistant 最终回复、不写 Done、不重执行工具、不回滚外部副作用。普通错误/ctx cancellation 不自动产生本记录。
  - 版本化事件信封：显式 DTO 编解码（sealed 接口直接 JSON 不可逆）；未知记录类型 / 信封版本 → 类型化兼容性错误，不静默忽略；
  - 检查点是可重建加速器，定位在自身 Seq；携带 canonical history、线程冻结 system、run_active、run_id、last_round。新写 checkpoint 使用 schema=2 与 `codec_version:2`，schema=1 快照仍可读。checkpoint 只在 active 且无 open declaration 的压缩边界写入，不在取消结算后新增快照。读取先校验记录与 payload 版本；未知未来版本返回类型化兼容错误，已支持版本的解码/结构校验失败经 `History(ctx, thread, fromSeq)` 全量回退。版本标记不得让旧 reader 通过新 checkpoint 绕过取消记录。日志仍全量保留，checkpoint 不是生命周期真相，也不承担文件系统回滚。
- 恢复语义：重放日志重建状态，**不向新流重放历史事件**（新消费者只看本次运行的事件）。中断轮 = 有 `round_declared` 无 `round_committed` → 对每个未提交调用合成一条 role=tool、`IsError=true` 的"结果未知"消息（每调用一条、按宣告顺序，满足 TC/TR 配对与 OpenAI/GLM 转换要求）；maxIter 截断 = **确定未执行** → 合成"因达到迭代上限未执行"软结果（非"未知"）。绝不盲目重执行工具。悬挂输入：日志中未终结 run 存在时，`RunThread` / `RunThreadStream` 仍拒绝新输入（`ErrRunIncomplete`），不得以放宽此保护实现改向。调用方可选择 `ResumeThread` 继续原输入，也可在执行者已收敛后通过 `SettleThread(ctx, target SettlementToken)` 明确结算原 run 为 cancelled，再提交新输入；target 必须在原 session 释放 ownership 前绑定其 ThreadID、RunID 与最后确认的 durable head，不得在释放后从 Latest 选择当前 run。恢复 active run 时仍先修复 unknown 配对、继续旧输入、不追加 user message；恢复最后一个已 cancelled 的 run 时只返回 `RunResult{Cancelled:true, ThreadID, History}` 与 nil error，不调用模型、工具、审批、overlay、退出回调或 compactor，不写日志，不产生事件。该结果 Message 与本次执行统计为零值，不代表旧任务成功或历史费用为零。此快照可重复读取，后续新 run_started 一旦提交便成为新的恢复目标。无记录、最后 run 为正常 Done/maxIter Done/兼容 KindError 时仍返回 `ErrNothingToResume`。崩溃先于终态落库的模型回复可能丢失并在恢复时重新生成（at-least-once，显式记录）；工具副作用完成后、`round_committed` 落库前的崩溃窗口同样如实报"结果未知"（不可避免的副作用窗口）。
- 消费者中途断开：普通可执行工具轮在 TC/TR delivery 中断时保持 incomplete/resumable，replay 依 durable declaration/results 补未知或继续。唯一例外是已经完整 durable precommit 的 `maxIter` terminal skip batch：它在首个 terminal TC 前已写 declaration、all skipped results、commit、Done；consumer 在 TC 或 TR break 只停止该 consumer 的 delivery，thread 已 closed，`ResumeThread` 返回 `ErrNothingToResume`。普通 early-break 不自动结算，且没有 RunInterruptedError 可交付；配置 WithRunExitFn 后，内核在 iterator unwind 完成且 ownership 尚未释放时同步交付本 session 的 SettlementToken，controller 保存后等 iterator 退出再结算。不增加 AgentEvent，不在 release 后回填 token；无 session/未 range 不回调。未配置回调的 early-break 不承诺交付原 run 身份，ThreadID 本身不能替代结算凭据。匿名持久 stream 同样可用退出回调取得原 token。
- 并发：每线程非阻塞运行所有权——第二个并发运行在**任何模型调用之前**即得类型化冲突（单纯加锁等待是串行化，不是冲突）；乐观版本号追加 + record_id 幂等重试作为其他写者的后盾。不可变记录批次与 ID 只生成一次，重试逐字节同一内容。SettleThread、SettlementTarget 和 cancelled 快照读取必须使用同一（Store 实例，ThreadID）所有权；仍有执行者时 ErrThreadBusy，不等待、不抢占。结算与随后 RunThread 是两个所有权区间。退出 token 的 RunID/head 在原 session 持有 ownership 时冻结；新写结算必须同时匹配 replay 的 runID 与 current head。revision 或身份不符即 ErrRevisionConflict，不自动刷新。唯一例外是原 ExpectedHead 处已存在同一原 RunID 的合法取消记录，此时只读确认、不追加。即使后继 run 已启动或已 incomplete，迟到请求也不得改变它。
- 线程 ID：调用方 ID 非空、有界、不规范化（转义后须满足文件名边界）；内核生成 = crypto-random 128 位，走 **rev=0 保留语义**：目标线程已存在即视为保留冲突，换新 ID 重试（绝不污染既有线程），重试仅限该次冲突。`RunResult` 与 `DoneEvent` 暴露 `ThreadID`；无 store 时不生成。显式线程入口（`RunThread` / `ResumeThread` / `RunThreadStream` / `SettleThread` / `SettlementTarget`）要求已配置 Store，否则返回 `ErrNoStore`，不静默降级为非持久运行。head=0 即"新建"，无"线程不存在"语义；调用方 ID 复用即"追加到既有线程"。`RunWithHistory` / `RunStreamWithHistory` 保持非持久（一次性导入语义，不入线程）。运行所有权按（Store 实例, 线程）全局登记，不按 Agent 实例；Store 值必须可比较（指针式实现，两个内置后端均满足），否则所有权登记返回类型化错误而不 panic。SettleThread 不创建线程。目标 RunID/head 匹配且已终结时返回 ErrNothingToSettle；原取消记录的幂等确认除外。无记录或身份/revision 不符的旧 token 返回 ErrRevisionConflict。只读 SettlementTarget 在 ownership 下检查当前 active run 并于释放前形成完整 token；无 active 返回 ErrNothingToSettle。它只用于新的显式恢复决策，不用于把旧“取消 A”请求自动改成取消当前 run。RunWithHistory/RunStreamWithHistory 始终不持久化，不交付退出 token，不能通过结算隐式导入。Store.Delete/重建线程不与运行控制并发，重建后丢弃旧 token。
- 失败、ctx 取消与消费者中断默认保留可恢复的未闭合 run，绝不写假的 Done。原 session 在释放 ownership 前冻结其 SettlementToken，调用方等合作收敛并释放后，才用新的有效且有界 context 和这个原 token 调 SettleThread。结算不发送取消信号、不调用模型或工具。新的显式恢复决策可针对崩溃或其他错误留下的精确 run，不改写旧错误原因；旧请求不得借恢复检查自动换目标或更新 revision。结算确认前的写入错误可能已落地，保留原 RunID/head 幂等重试；退出前执行写入结果不确定时，最后确认的 head 可能落后，必须冲突失败而不是猜测。结算与下一个 run_started 之间存在独立窗口：旧 run 已取消不等于新 input 已持久接受。maxIter terminal skip 的既有规则不变：preparation 前或 precommit 中的取消/失败仍可恢复或显式结算；已提交 skipped results 原样保留；完整 Done 一旦提交，后续 delivery 的取消/break 不撤销终态，也不能再结算为 cancelled。

#### 取消结算与日志兼容性

`SettlementToken{ThreadID, RunID, ExpectedHead}` 绑定单个 run 与 revision。
`SettleThread(ctx context.Context, target SettlementToken) error`
仅在二者都匹配时以单条 `run_cancelled` 完成持久结算。记录包含 `run_id`、
nullable `open_round` 和有序 `results`；所有键必需，无 open 时为
null 与空数组。记录 ID 为该 RunID 的固定 `-cancel` 后缀。
有 open 时，结果必须逐一匹配全部 declared calls 且明确 unknown；
无 open 时不得携带工具结果。错 run/round、错误配对、额外内容、
重复 terminal 或非 active run 的取消记录均为 ErrIncompatibleLog。

Store 新增 SchemaV2=2 和 KindRunCancelled，不改变 Store 接口、
sequence、fsync 与完整内容幂等合同。unset schema 仍规范化为 1；
旧 kind/payload 保持旧版本，新取消记录必须 schema 2。
新版 checkpoint 采用 schema 2 与显式 codec_version=2。
旧日志无需迁移；老二进制不保证读取新取消日志，必须报兼容性错误，
不得借未知 kind、KindError 或 checkpoint 隐藏新终态。
JSONL 多记录 Append 不被当作断电事务；R07 的 unknown 修复与
cancelled 终态放在同一条记录内。

sealed AgentEvent 仍为七种，不增加取消事件，不把取消快照伪装成
DoneEvent。RunResult 新增 Cancelled，仅 Resume 的终态读取分支
为 true；实际执行完成的 RunResult 与 DoneEvent 仍保持对应，
但取消快照不是从事件流归并得到的执行结果。

#### 父子线程的取消结算

SettleThread 非递归，仅结算指定 Agent/Store 中原 token 绑定的 run。
父结算不关闭或删除子线程；子结算不向父 declaration 提交结果。
父未提交的 child 工具调用在父结算时仍为 outcome-unknown，
不根据子成功/取消状态伪造父已提交结果；子悬挂调用由子负责配对。

agenttool 每次 Execute 只调用一次 child.Run，不保有可续跑的子
session；配置 Store 的 child 会生成独立持久线程。父 ctx 取消
传给子执行，但不等于父、子 durable run 已结算。宿主必须先等待
所有 child owned work 和 parent 调用/iterator 退出，保存各自
退出前捕获的 SettlementToken 与对应 Agent/Store，再从子到父显式
清理，父结算确认后才开始新输入。任一 child 清理失败不得报告
整个委派已清理，也不得换用它的最新 run 身份重试。

嵌套 RunInterruptedError 的外层 token 属父、内层属子；不能只取
一次 errors.As 就假定已经收齐所有 child。并行 child 的完整目标
集合由宿主接线各自退出观察回调并关联工具调用；回调不递归结算。
子正常终态不重写；只有子结算时父仍 incomplete，只有父结算时
子仍可能 incomplete。后续 agenttool 调用新建 child run，不恢复
旧子线程，但新建不代表旧子线程已清理。

R07 不提供跨 Store 事务、durable 委派关系或匿名 child 的跨崩溃
发现。进程在 child token 交付前崩溃可能留下无宿主索引的
incomplete 子日志；宿主必须标记清理未确认并负责后续发现/处理。
最小 one-shot child 可不配置 Store；承诺持久子线程崩溃清理的
产品须另行提供 durable 子调用身份索引，不得声称父结算已涵盖。

#### 逐轮运行环境上下文（冻结契约）

`WithRoundContextProvider(RoundContextProvider)` 允许宿主在每次直接
`Provider.ChatStream` 或 `Provider.Chat` 调用前产生当前运行环境快照。
callback 接收原始 `context.Context` 和 `{Round, Attempt, ThreadID}`：
Round 为逻辑模型轮（Resume 延续 durable 序号），Attempt 为该轮直接
Provider method invocation 的一基序号，故不同于只计既有 ChatStream
retry-loop 的 `RetryInfo.Attempt`。仅 ChatStream **直接返回**的可重试
error 触发既有 retry；iterator yield 的 error（含零 chunk）不 retry。
callback 在单次 run 内串行；同一 Agent 的并发 run 由宿主保证 callback
并发安全。

非空快照以固定来源标识包装为本次 outbound request 末尾的 role=user
message。它是**瞬态 request overlay**：不进入 canonical history、
`DoneEvent.History`/`RunResult.History`、AgentEvent、Store 日志或
checkpoint；不改变 sealed 7-event set。canonical history 的 system 前缀
是 kernel grammar/checkpoint 冻结契约；Responses 又会把 system 提升为
单个 instructions，故中途 system 没有统一语义。所有现有 adapter 保留
trailing user，故它是共同 carrier。每次 callback 返回完整有效快照，内核
不按内容去重；空字符串表示该次无 overlay。

预算只承诺 `estimateRunes([]llm.Message)` 的 message-rune 启发式，不含
tool schema、provider wire 映射、reserved output 或真实 tokenizer。启用
时令 overlayCap 为 window 的 20%，canonicalCap 为其余部分；compaction
后无条件检查 canonicalCap（包括 nil/error/invalid compactor 路径），超限
即在 callback 前以 `ErrCompactionBudgetExceeded` 失败。随后以完整 outbound
candidate 的实际 `estimateRunes` 计入 envelope/JSON 开销；只有 envelope、
marker、首尾 rune 都可容纳才截断，否则以同一预算错误失败。若 tiny window
连 envelope 也容不下，同样在 callback 前失败，绝不发送破碎 overlay。
压缩/checkpoint/`CompactionEvent` 只处理 canonical history；因此 callback
failure 前可能已有合法 CompactionEvent/checkpoint，但绝无 overlay、
provider-response 或 Done event。

只要持久 session 已成功确认 `run_started`，`threadSession` 最外层就将
**所有**向调用方交付的非 nil error 包成可 Unwrap 的
`RunInterruptedError{ThreadID, Err}`；round-top cancel、Compactor/
checkpoint、预算、callback、Provider/iterator、工具、commit/Done 都适用，
既保留 `errors.Is/As` 原因，也让匿名 `Run` 总能 `ResumeThread`。仅
`openSession` 成功前的错误保持原样；消费者主动 early-break 没有 error，
不属于本契约。每个 session boundary 均无条件新套当前 session 的
ThreadID；嵌套 child 的 inner wrapper 因此保留为 cause，而 caller 的
第一个 `errors.As(*RunInterruptedError)` 始终是可 Resume 的 parent。
`errors.Is` 仍透过完整链识别 `ErrThreadBusy`、`ErrNothingToResume`、
`ErrRunIncomplete`；但 generated-thread 的重试只接受**无**
RunInterruptedError 的 reservation/setup `ErrThreadBusy`，不会把
post-session cause 误当作安全重建。callback 专有错误则在该外层内部保留
`RoundContextError{Round, Attempt, Err}`；callback 前取消不伪造该内层。
Resume 先重放 canonical history，再执行 live callback，故不重放、累积
或双计旧快照。

RunInterruptedError 保留 ThreadID、Err 与 Unwrap，新增
Settlement *SettlementToken；执行错误交付前，在原 session 仍持有
ownership 时捕获其 RunID 与最后确认 head。外层包装必须携带父
session 的 token，内层 child token 保留在原因链，二者不可替换。
它不是取消终态，不自动触发 SettleThread，errors.Is/As 保持原义。
WithRunExitFn(func(SettlementToken)) 在成功建立的 session unwind
之后、ownership release 之前交付 token，覆盖 early-break 且不造错误
或事件；callback 仅保存值，不重入线程、不等待持锁操作。
调用方必须等原调用/iterator 退出后再使用 token。无 session 的 setup
错误保持无 wrapper；没有原 token 时只能做新的显式恢复检查，不能
依据 ThreadID 自动认领当前 run。SettlementTarget、SettleThread 与
cancelled 快照读取都不启动执行，不新增该包装或退出回调。

### OpenAI Responses 协议适配器（`llm/openairesponses`，建设中）

独立子包实现同一 `llm.Provider` 接口（`Name` 为 `openai_responses`），与 glm/openai 平行；协议差异是条目级的，不做双协议混包。已裁决（2026-09-11）：D1 独立子包；D2 状态策略 A（`store:false` + 手动回放）；D3 新增条目级 reasoning 块；D4 内建工具不做；D5 Chat+ChatStream 全量、mock SSE 单测 + e2e。

- 状态策略：`store:false` + 手动全量回放 items——历史由调用方持有，与 Compactor/截断/Store 完全正交；不使用 `previous_response_id` 与 Conversations API（B 方案留作未来可选模式）。
- 输入映射：system 消息 → 顶层 `instructions`；user/assistant 消息 → `message` item；assistant 的 `tool_use` block → `function_call` item；`tool_result` → `function_call_output`（`call_id` 关联）；`reasoning_item` block → `reasoning` item（`encrypted_content` 原样透传，置于其 `function_call` 之前，保持声明顺序）。
- 输出映射：`message.output_text` → `TextBlock`；`reasoning` → `ReasoningItemBlock`；`function_call` → `ToolUseBlock`（`call_id` 即块 ID）；未知输出 item 类型忽略（debug 日志，向前兼容）。
- 流式映射（保序）：`response.output_text.delta` → `TextDeltaChunk`（携带 output_index）；`response.reasoning_summary_text.delta` → `ReasoningDeltaChunk`；`response.output_item.added`(function_call) → `ToolCallStartChunk`（index = output_index）；`response.function_call_arguments.delta` → `ToolCallArgsChunk`；`response.output_item.done`(reasoning) → `ReasoningItemChunk`（携带加密推理项与其 output_index）；`response.completed` → 补发未交付的 reasoning 项 + `DoneChunk`（usage）；`response.failed`/`response.incomplete`/`error` → 错误。未知流式事件类型忽略。装配按 output_index 跨类合并（reasoning 项与 function_call 交错时保持宣告顺序）；CC 协议无 reasoning 项，维持原分桶装配。
- `llm` 新增 `ContentBlock` 变体 `ReasoningItemBlock{ID, EncryptedContent, Summary}`（type `reasoning_item`）：条目级推理项，与 `ReasoningBlock`（CC 协议内嵌推理文本）语义不同；持久化原样承载（深拷贝含 Summary 切片；值/指针两种形态都妥善处理）。既有适配器转换时跳过该 block，不产出也不出错。
- `llm` 新增 `Chunk` 变体 `ReasoningItemChunk{OutputIndex, Item}`：流式交付加密推理项；`TextDeltaChunk` 增加 `OutputIndex`（CC 适配器恒为 0）。装配器在有 reasoning 项时按 output_index 跨类排序，否则维持 CC 分桶。
- 公共选项映射：`WithMaxTokens` → `max_output_tokens`；`WithTemperature` → `temperature`；`WithStop` 在 Responses 中无对应参数——显式返回类型化错误，不静默丢弃。非流式 `Chat` 对 `status != completed`（incomplete/failed）返回错误，不把部分输出当成功。默认 HTTP 客户端带 120s 超时。
- v1 不支持：内建工具（web_search/file_search/code_interpreter/computer/MCP）、结构化输出（`text.format`）、`previous_response_id`、WebSocket 模式。ToolInfo 只产 function 定义，内建工具无法被请求。
- SSE 复用 `llm.DoStreamRequest`/`llm.ScanSSEEvents`；非 2xx 用 `llm.APIError`；Chat（非流式）解析 `output` items 组装最终消息。

### P2 扩展面
- `Tool` / `Provider` 中间件（包装器模式）——已落地（P2-1，docs+example，见 `examples/middleware`；不加内核类型或链式 API）
- 工具并行执行（默认串行保证确定性；历史按调用顺序追加，完成可乱序靠 ID 配对）——已落地（P2-2，冻结契约见下）
- 公开测试替身包（脚本化 Provider、录制回放），让使用者零成本测试自己的 agent——已落地（P2-3，冻结契约见下）
#### 工具并行执行（P2-2 冻结契约）

`WithToolConcurrency(n int)` 设置同一模型回复内的工具执行并发度；`agent.New` 将 `n <= 0` 归一为 `1`，默认也是 `1`。`n=1` 保持今天的可观察行为：逐调用、live `Get -> approval -> Execute`，且现有 panic 仍按原路径逃逸。`n>1` 是显式 opt-in，工具的同轮副作用可重叠。

所有并发度下，模型可见的 tools 都是一次 run 开始时 `registry.List()` 取得的快照，后续模型轮不得刷新该列表。仅 `n>1` 在每个工具轮规划时冻结执行 handle 和 `Info`；规划后 `Register` 的工具不影响本轮，虽可由之后工具轮的 live `Get` 找到（模型广播仍是 run-start 快照）。

`n>1` 在任何 dispatch 前按声明顺序完成 handle/`Info` 规划和整批 approval。unknown tool、无 callback 与拒绝是对应声明槽位的软结果；callback 或 planner `Info()` panic 在该边界 recover 为 hardError，停止后续规划和全部 Execute。`n=1` 的 approval/`Info` panic 行为不变。

每个声明有一个槽位，终态为 resolved、hardError 或 missing。worker 与预计算软结果在发布槽位前立即执行 `applyToolResultLimit`；完成顺序绝不泄漏到 history、事件或 Store。共享 settlement/materialization routine 供两种模式使用，只有规划/dispatch 不同：

- hardError：选取最低声明索引的 hardError，仅 materialize 它之前连续 resolved 前缀，返回带 iteration/name 上下文的错误，不 commit；
- missing：无 hardError 时仅 materialize 第一个 missing 前的连续 resolved 前缀，返回 `ctx.Err()`，不 commit；
- 全部 settled：按声明顺序 materialize 全部结果，随后单次 `commitRound`；结果位置必须逐项对应 declaration，满足 `validateResultBatch`。

hardError 优先于 missing。`n=1` 每完成一个槽位即走共享 settlement，以保留 c1 在 c2 派发前 materialize 的旧时序；`n>1` 停止派发并排空已启动任务后才结算。并行调度使用最多 `min(n, len(calls))` 个 worker；每调用在 dispatch 前检查 ctx，取消停止派发、把 ctx 传给已启动工具并排空。排空没有人为上界：工具必须 honor ctx，内核不以泄漏 goroutine 伪造终止。

TC-first 与 materialization：普通轮的全部 TC 连续宣布后才执行；break 在 TC/TR 期间停止 materialization，且 Store 仍未 commit，因此可 resume。`maxIter` terminal skip round 是唯一已知结局的例外：无 planner/approval/worker/Execute，先整体 durable close（Store）或构造完整 terminal history（non-Store），再同步 TC → skipped TR → Done。其 break/cancel 规则见 P1。
- 公开测试替身包（脚本化 Provider、录制回放），让使用者零成本测试自己的 agent

#### 公开测试替身（P2-3 冻结契约）

`agenttest` 是公开、stdlib-only 的测试替身包；它只 import `llm`、`tool` 与标准库，绝不反向 import `agent`。它提供全局严格有序的 `ScriptedProvider`、`Recorder`/`Replayer` 和最小的 `ToolFunc`。`Request` 深拷贝并严格比较 messages、按名称排序的 tools 与 `llm.ApplyOptions` 后的 options；函数型 option 的身份不是契约。Chat 与 ChatStream 共享一个全局 Exchange 顺序，方法交替也必须匹配。成功的 ChatStream 在调用时保留步骤，迭代自然结束、已交付的 terminal stream error 或 nil-error chunk 后的 early break 才释放；保留期间的任意调用返回 `ErrConcurrentScriptUse`。脚本耗尽返回带方法和一基 step 编号的 `ErrScriptExhausted`；不匹配返回含零基全局 step、expected/actual method 的 `RequestMismatchError`（`ErrScriptMismatch`）；`Verify` 对未消费或 active stream 返回含 Next、Remaining、Active 的 `ScriptVerificationError`（`ErrUnverifiedScript`）。

录制 bytes 的 **v1 grammar** 固定不变：顶层 `version` 与全局有序 exchanges；每项有 canonical Request、method discriminator，以及 chat 或 stream response。stream 记录 chatstream outer error 与 iterator stream error 的独立位置和 completion 状态。六个值形式 chunk DTO 都有 `type` discriminator，并保留 TextDelta 的 Text/OutputIndex、ReasoningDelta 的 Text、ToolCallStart 的 Index/ID/Name、ToolCallArgs 的 Index/ID/Delta、ReasoningItem 的 OutputIndex/Item、Done 的 FinishReason/Usage；Done Usage 的 nil 与非 nil 必须可区分。自然 iterator exhaustion 总是 COMPLETE（不要求 DoneChunk，Agent 以已装配工具调用判断终态）；已交付 terminal StreamErr 是可回放的 COMPLETE-WITH-ERROR；只有 nil-error chunk 后消费者 early-break、abandoned iterator 或 recorder read failure 是 INTERRUPTED，`NewReplayer` 必以 `ErrInterruptedRecording` 拒绝，绝不可将其回放成成功。`Bytes` 在 active recording 时返回 `ErrActiveRecording`。所有 pointer-form chunks（含 typed nil）继续按原动态形式交付下游，但 `Bytes` 返回带动态类型的 `UnsupportedChunkError`/`ErrUnsupportedChunk`；v1 不将其归一化为值形式。

error DTO 依次编码和重建 `context.Canceled`/`DeadlineExceeded`、`llm.ErrStreamingNotSupported` 哨兵身份、带 StatusCode/RetryAfter/Body 的 `*llm.APIError`、`llm.IsNetworkError` 所认定的 timeout/temporary `net.Error`、`url.Error` 包装与 ECONNREFUSED/ECONNRESET/EPIPE，以及最后的仅 message generic error。回放必须保留相应 `errors.Is`、`errors.As` 或 `llm.IsNetworkError` 分类。未知 schema version、chunk/error type 或字段、非法 DTO 字段，均以 `ErrIncompatibleRecording` 类型化拒绝；多个 DoneChunk 原样回放；没有 Done 且没有 tool calls 的流按自然 exhaustion 成功。Replayer 同样严格执行全局 exchange 顺序、call-time reservation 和 Verify。

`ToolFunc` 直接暴露 `Definition tool.ToolInfo` 与 `ExecuteFunc func(context.Context, json.RawMessage) (*tool.ToolResult, error)`；每次 Execute 先深拷贝记录 args，`Calls` 返回受 mutex 保护的深拷贝快照。nil handler 返回普通 Go error，panic 原样透传，便于测试 Agent 的既有边界。

录制 bytes 是显式版本化契约：既有 **v1** 录制继续按上述冻结 grammar 读取；**v2** 由 Recorder 写入，Replayer 读取 v1/v2。v2 对 parameters 使用 `ParameterSchema.UnmarshalJSON`，重建 carrier、composition、AP-schema、`$ref`、BooleanSchema 与 codec presence state，而不是扩展 v1 allow-list。

### P3 生态
- MCP 桥（基于官方 `modelcontextprotocol/go-sdk`，独立子包；P3-3 review 修订中）。`mcp` 的 Go package 名为 `mcpbridge`，是 client-only、tools-only 的静态发现桥：`Connect(ctx, registry, transport, Config{Namespace}, opts...)` 在调用方提供的 fresh、未发布且完全静止的 Registry 中分页发现并一次性注册 wrapper；调用方只可在成功后发布 Registry。不会 refresh/unregister；stale tool 的 protocol error 是硬错误。SDK `MultiRoundTrip.Disabled=true` 是冻结前提，`NeedsInput` 映射为一次软错误且 bridge 零 retry。最终工具名为 literal `${Namespace}__${remoteName}`，namespace 与最终名必须匹配 `^[A-Za-z0-9_-]{1,64}$`（namespace 还不得含 `__`）；不转义、截断或加后缀。

  `mcp.WithApprovalRequired(true)` 默认要求全部 bridge tools 审批；显式 false 时，也只有 `annotations != nil && annotations.ReadOnlyHint && (annotations.DestructiveHint == nil || !*annotations.DestructiveHint)` 的可证明只读工具可免审批，其他 annotation 组合仍要求审批。参数 schema 采用 recursive typed fields 与 name-sorted carrier：bridge 将 SDK `map[string]any` canonical-marshal 后交 `ParameterSchema.UnmarshalJSON` 重建 presence state；仅 decoded-object root、命名、marshal/projection structural error fail closed 且零注册，provider 接受度独立。

  Execute 对非法 JSON、null、数组或标量参数返回软错误且不发 RPC；MCP `IsError` 和 `NeedsInput` 是软错误，protocol、transport、context、wire/decode 与 marshal 错误是保留 `%w` 的硬错误。文本 content 按顺序以换行 join；image/audio、resource link、embedded resource 生成已转义的单行 placeholder，并在 `ToolResult.Data` 生成带原 content index 的 `{"mcp_content":[...]}` envelope；embedded text 仍在 Content。StructuredContent 紧凑 JSON 为最后一个片段；未知已解码 dynamic content 或 nil embedded resource 为整项软错误。placeholder 对 `\\`、`"`、CR、LF、`]` 分别转义为 `\\\\`、`\\"`、`\\r`、`\\n`、`\\]`。`WithResultDataLimit` 默认 1 MiB，非正值在 Connect 前拒绝；每个 entry 按最终累计 JSON 大小预检，恰好上限接受，超限以 omitted placeholder 代替，且全部省略时 Data 为 nil。

  工具 `OutputSchema`、top-level Title/Icons、content Annotations、ResourceLink Icons 及所有 `_meta` 均丢弃；progress notification 静默丢弃。Close 的公开前置是调用方先取消并以精确 join 信号等待所有 in-flight Execute 返回；不支持 Close 与 Execute 并发，Bridge 直接委托 SDK session Close。`mcp/README.md` 记录这些边界、Data 仅供 live `ToolResultEvent`（history/Store 只有 Content）和静态生命周期。

#### MCP client bridge（P3-3 冻结契约）

| MCP 输入或结果 | bridge 映射 |
|---|---|
| static discovery | 仅连接时分页 `tools/list`；不订阅 list-changed、不 refresh/unregister。已删除 remote tool 的 protocol/transport error 是 Go hard error。 |
| request args | 只接受 JSON object（`{}` 合法）；invalid JSON、null、array、scalar 为 soft result，零 RPC。原 ctx 传给 `CallTool`；bridge 不 retry/redial。 |
| `IsError` / `NeedsInput` | 都是 soft ToolResult；后者内容固定为“v1 不支持 MCP input-required continuation”，并因 `MultiRoundTrip.Disabled:true` 精确一次 call。protocol、transport、ctx、nil result、SDK wire/decode 与 structured marshal error 都保留 `%w` 为 hard error。 |
| text | 保持 content 原顺序并以 `\n` join；不写 Data entry。 |
| image / audio | 分别写 `[mcp:image mimeType="<escaped>" bytes=<n>]`、`[mcp:audio mimeType="<escaped>" bytes=<n>]`，并写带原 content index、type、mimeType、JSON-base64 data 的 Data entry。 |
| resource link | 写 `[mcp:resource_link name="<escaped>" uri="<escaped>" title="<escaped>" mimeType="<escaped>"]`，Data entry 带 index、uri/name/title/description/mimeType/size。 |
| embedded resource | 写 `[mcp:resource uri="<escaped>" mimeType="<escaped>" bytes=<n>]`，其 nonempty text 紧随该行；Data entry 带 index、uri/mimeType/blob JSON-base64，text 不重复进 Data。 |
| StructuredContent / decoded mapper failure | StructuredContent 紧凑 JSON 是最后一个 Content 片段；未知已解码 dynamic content type 或 nil embedded Resource 是整项 soft error、不返回 partial success。 |

`<escaped>` 按 rune 将 backslash、quote、CR、LF、`]` 分别写为 `\\`、`\"`、`\r`、`\n`、`\]`，其余 rune 原样写入。因此远端字段不能闭合 placeholder 或注入另一行。Data envelope 形状为 `{"mcp_content":[...]}`；image/audio entry 无论 mimeType 或 data 是否为空，均固定有 `index`、`type`、`mimeType`、JSON-base64 `data` 字段。`WithResultDataLimit` 对每次候选 entry 的最终累计 JSON 编码预检，默认 1,048,576 bytes，恰上限接纳，超一 byte 的 entry 改为 `[mcp:<type> omitted: bridge data limit]`（embedded text 仍保留）。没有接纳 entry 时 Data 为 nil。OutputSchema、top-level Title/Icons、content Annotations、ResourceLink Icons、工具/调用/content/resource `_meta` 全部 drop；不接受 per-call `_meta` 注入，progress 也不发 event/content/data。

**SDK v1.7.0 discovery boundary:** `ClientSession.ListTools` unconditionally logs and silently excludes a tool whose inputSchema has an invalid `x-mcp-header`: an annotation on a non-string/integer/boolean property, a non-string/empty/invalid HTTP-field-name value, or a duplicate case-insensitive header value at any property nesting. The bridge cannot observe or reject those filtered rows. Server authors must fix the annotation; a future SDK option is the only v1 escape. This is an SDK-boundary limitation alongside decoded-map duplicate-key/order/numeric-token loss, and provider acceptance remains independent from schema representability.
- agent-as-tool（已完成）：卫星包 `agenttool` 冻结为 `agenttool.New(*agent.Agent, Config) tool.Tool`，`Config` 只含工具面向父模型的 Name、Description、RequiresApproval。schema 固定 required `input:string`；每次 Execute 无状态地只调一次 `Run`，按顺序只提取 value-form `TextBlock`，Data 为 nil。malformed input、截断、无文本是软结果；子 `Run`/ctx 错误以 `%w` 硬传。每个 returned adapter 以单槽 gate 串行，多个 adapter 包装同一 child 时不互锁，调用方负责共享协作者的并发安全；不共享 session/thread，child 配置 Store 时仍由每次 `Run` 生成独立 thread 且 adapter 不暴露它；不做 pool、retry 或 nested event forwarding，父层只见一个最终 `ToolResult`。
- README + `examples/`（本轮完成目标）

### Backlog（按需）
结构化输出（JSON schema）；花费上限；原生 Anthropic / Ollama 适配器；更丰富的人工介入（改写、批准后继续）。

### 永不做
模型训练/微调、图形界面、内置 RAG、图编排引擎。

### 扩展包（内核之外，独立演进的卫星包）
记忆包（情景/语义记忆）、自进化包（程序记忆：从轨迹总结经验、改进提示词、学新工具）、工具集包。验收标准：这些包落地时内核 diff 为零。

## 6. 关键调研结论（2026-09-09）

- 事件分类、只读事件 + 包装器扩展，均与业界共识一致（OpenAI Agents SDK / Vercel AI SDK / PydanticAI）。
- 截断教训：只掐头不留尾是 bug 来源（Hermes 案例）；按上下文占比限幅（openclaw 30% 规则）；上游 API 不会静默截断（Claude 超限直接报错），限幅必须发生在本层。
- 压缩参照 Microsoft Agent Framework：策略组合（丢最老工具组 → 滑窗 → 摘要），在 token 预算下依序执行。
- 持久化参照 LangGraph（线程/检查点）与 Temporal（事件回放，恢复时不重新执行不确定操作）。
- 行业正在把本层命名为 "agent runtime" 并与 agent 逻辑分离——本项目方向与行业收敛一致。

## 7. 当前状态（2026-09-10）

- 模块 `github.com/dailz1/go-agent`，Go 1.26，git 已建（main，基线 `d5281c9`）；kernel package closure 维持 stdlib-only，MCP SDK 仅允许位于未来 `mcp/` packages。
- 已有能力：基础循环、流式事件、工具注册与审批、429/5xx/网络错误重试（有上限）、openai/glm/openairesponses 适配、token 用量统计、完整测试（7 包全绿）。
- 工具结果截断（单结果 ≤ max(128, WithContextWindowTokens×30%) rune，50/50 掐头留尾，默认窗口 8192）。
- 历史预算管理（默认窗口 80% 触发压缩链：折叠老工具组→滑窗，可选显式注入 provider 的 AI 摘要；恒保护 system/首末 user/最近组；Compactor 接口可手动对任意历史执行）。
- 近期变更：已删除 provider factory 死代码；`Run` 已并入 `runStreamInternal`，形成单一执行路径。
- 事件/History 契约已更新：maxIter terminal tool calls 以 TC + deterministic skipped TR 公开，Store 与非 Store 的 Done History 相同且可 paired continuation。`DoneEvent.History` 是完全历史的 canonical snapshot；OpenAI Responses reasoning items 不单独成为 AgentEvent，故 consumer/test fold 在 Done snapshot reconciliation，而非宣称 raw event stream 独立编码所有 assistant blocks。覆盖位于 `agent_fold_test.go`、`reasoning_order_test.go` 及 persistence stream tests。
- Store v1 契约已裁决冻结（2026-09-10）：内核自动持久化（WithStore/RunThread），双提交点（run-start/round-commit），规范记录日志（run_started/agent_event 信封/round_commit/error，含 schema 版本、record_id、逻辑序号），检查点为可重建加速器（History + next_seq），乐观并发 + record_id 幂等，v1 单进程每线程串行，JSONL fsync 先于确认/撕裂尾截断/内部损坏报错。设计经对抗评审 st_01a08a3f：D1 按其修订采纳内核写入与生命周期记录；检查点定位按 M1 修正为加速器（原“正确性必需”论证有误，CompactionEvent 本身可全量重放）；HITL 中断等待仍留 Backlog。Store v1 已实现并提交：agent 侧 16feb3b（WithStore/RunThread/ResumeThread/RunThreadStream），store 契约实现 dfb7803。
- OpenAI Responses 适配器已实现并提交（fe2c7d6，llm/openairesponses）：typed items、store:false 全量回放、reasoning items（含 encrypted_content）；内置工具/结构化输出/previous_response_id 不支持（v1）。
- 工具调用事件采用"先宣告后执行"顺序：同轮的全部 `ToolCallEvent` 连续发出后再逐个执行，跨轮因此可分辨（2026-09-10 裁决）；取消或首个调用硬失败时，已宣告调用随 assistant 消息完整可重建，`ToolCallEvent` 语义为"模型请求的宣告"而非"已执行"。交付是同步的：消费者未确认宣告会推迟对应执行，在宣告批内提前断开则本轮不执行任何工具。
- 已落地工具结果截断与历史预算管理（Compactor），聚合溢出已知限制关闭；残余：保护组自身超预算时报 ErrCompactionBudgetExceeded；rune 估算为启发式非保证。
- P3-1 已扩展 `tool.ParameterSchema`/`Property`：typed recursive schema、nullable canonical codec、carrier、presence state 与 marshal/Registry cycle safety 均已落地；旧公开模型构造值的 JSON 保持 byte-exact。

## 8. 状态更新（2026-09-20）

- 路线图 P0–P3 全部闭环，已发布 v0.1.0（tag 于 `f064c63`）；§5 的 C10 修订（maxIter 终局轮宣告 ToolCallEvent + 确定性 skip ToolResultEvent，零执行零审批）已落地为 `agent/max_iter.go` 与折叠契约的 Done reconciliation（`DoneEvent.History` 为权威快照）。
- 仓库结构：pkg/ 目录废除，五包提升至模块根（`agent/`、`llm/`、`store/`、`tool/`、`agenttest/`）；卫星包 `mcp/`（包名 `mcpbridge`）、`agenttool/`、`examples/` 平铺于根；import 路径为 `github.com/dailz1/go-agent/<pkg>`。
- 内核闭包（agent/llm/store/tool 零第三方依赖）由 scripts/kernel-closure-guard.sh 与 CI（.github/workflows/ci.yml，battery + kernel-closure 两 job）永久守护；`go list -m all` 现为 14 行（MCP SDK + 传递依赖），属预期形态。
- 许可证：MIT（LICENSE）；知识库 AGENTS.md 已于 f064c63 重生成（root + 6 包），锚点经机检。
