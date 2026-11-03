# 41 · 设计决策索引（decisions index）

> 状态：v1（2026-10-29）
> 性质：**汇总索引**。本文不产生新决策，只把散落在各设计文档与裁决记录中的既有决策收拢为一处可检索的清单：每条决策给出背景、被否决的备选（有记录则引出处，无记录标"未记录"）与结论，并链回原文。代码中存在但尚无书面决策记录的点单列在末节「待沉淀决策」，标注 推断（源码考古）。
> 用法：修订某项决策时，先回到本文引用的原始文档改契约（docs-first，见 Q1），再回来同步本索引。相关内核文档：[00 全景](00-overview.md)、[10 循环](10-agent-loop.md)、[11 工具执行](11-agent-tools.md)、[12 持久化](12-agent-persistence.md)、[13 上下文](13-agent-context.md)、[14 控制面](14-agent-control-plane.md)、[20 消息模型](20-llm-model.md)、[21 传输](21-llm-transport.md)、[30 store](30-store.md)、[31 tool](31-tool.md)、40 不变量（另篇）。

## 0. 来源与编号

主要来源（按引用密度排序）：

| 简称 | 文件 | 内容 |
|---|---|---|
| DESIGN | `docs/DESIGN.md` | 设计契约正文：目标排序、契约表、路线图内嵌裁决（含 Responses 适配器 D1–D5、Run API 重塑 D1–D3） |
| CDR | `docs/control-plane-decision-record.md` | 控制面 D1–D10 裁决记录（rationale，"为什么是这些决策"） |
| CPD | `docs/CONTROL-PLANE-DESIGN.md` | 控制面设计 v3（契约文本：记录类型、API、drain 点、§12 修正案） |
| TOOL | `docs/TOOL-DESIGN.md` | tool 包设计文档 |
| MCP | `docs/MCP-DESIGN.md` | mcp 桥设计文档 |
| SKILLS | `docs/SKILLS-DESIGN.md` | skills 包设计文档 |
| CACHE | `docs/tool-registry-cache-design.md` | Registry Info 冻结与读缓存设计（2a + D3 裁决，§11 开放点） |
| RAR | `docs/runtime-architecture-research.md` | 外部架构调研（保留 / 优先研究 / 暂不做清单） |
| AGENTS / agent-AGENTS | `AGENTS.md` / `agent/AGENTS.md` | 反模式与注意事项（ANTI-PATTERNS、NOTES） |
| kernel/NN | `docs/kernel/00–31` | 内核文档系列中已展开的决策细节 |

条目编号仅用于本文引用便利：`A` 架构边界、`P` 持久化、`C` 控制面、`T` 工具与 schema、`L` 传输、`Q` 流程与质量、`S` 待沉淀。同名决策编号有三组互不相干的"D1–D5"（Responses 适配器、控制面、Run API 重塑）与一组"D-1–D-4"（Registry 缓存），出处栏可区分。

统一格式：**决策** → **背景/问题** → **被否决的备选** → **结论** → **出处**。

## 1. 架构边界

### A1 · 定位：可嵌入库，不是框架、云服务或入口程序

**决策**：go-agent 是可嵌入的 Go agent 运行时库——把"模型思考 → 调用工具 → 结果回填 → 继续思考"这个循环跑对、跑稳；不提供编排图、工作流引擎、记忆系统（不是框架）；不做云托管运行时（不是云服务）；不绑定模型厂商；内核不含 `package main`（`examples/` 仅为演示）。
**背景/问题**：使用者在自己的进程里构建可靠的 AI agent，需要的是库而非接管进程的框架；与 Claude Managed Agents 等云运行时互补而非竞争。
**被否决的备选**：框架化能力面——编排图、工作流引擎、记忆系统、内置 RAG、图形界面、模型训练/微调，全部进「永不做」清单，属显式否决而非待办；记忆/自进化/工具集放扩展包，验收标准是"落地时内核 diff 为零"。
**结论**：定位与边界冻结；扩展包在内核之外独立演进。
**出处**：DESIGN §1、§5「Backlog」「永不做」「扩展包」。

### A2 · 设计目标排序：稳定 > 可替换 > 小 > 好用 > 留口子

**决策**：五目标按优先级排列，冲突时后者让位于前者：稳定（兜住断流/崩溃/超长/失控）> 可替换（换供应商业务代码零改动）> 小（内核零第三方依赖）> 好用（四概念上手、全程可观测）> 留口子（今天存的数据为明天的功能保持完整可取）。
**背景/问题**：嵌入库的第一义务是"用户程序不能挂，花费不能失控"；目标冲突时需要元准则。
**被否决的备选**：未记录（排序本身未见对立提案）。
**结论**：该排序是后续多数裁决的裁决依据——例如工具结果按截断策略后的轨迹保存、超长原文不保留（稳定的代价），但截断后轨迹完整入史（留口子的收益）。
**出处**：DESIGN §2。

### A3 · 内核闭包 stdlib-only + MCP SDK 钉 v1.7.0 + 单仓单 module

**决策**：`agent`、`llm`、`store`、`tool` 的完整编译依赖闭包不含第三方 module；MCP SDK source import 只允许出现在根 module 的 `mcp/` packages，并逐字节钉死 `v1.7.0`；全仓库单一 `go.mod`，卫星包（`mcp/`、`agenttool/`、`skills/`、`harness/`、`examples/`）平铺于模块根；`pkg/` 目录废除（五包提升至根）。
**背景/问题**："小"目标要可执行地守住——CI 用 `go list -deps` 断言闭包、逐字节断言根 `go.mod` 直接 require；SDK 只该进入用到它的二进制。
**被否决的备选**：卫星嵌套独立 `go.mod`（多 module）——P3 讨论裁决否决（单 module 消除 kernel↔bridge 版本漂移，且 N 个卫星 = N 个 go.mod），仓库文档未记录全文，现状单 `go.mod` 为源码考古佐证；`pkg/` 子目录布局——已废除形态，重引入列为反模式。
**结论**：双 CI 守卫：`scripts/kernel-closure-guard.sh`（闭包 + SDK 钉版）与 `go list -m all` 基线比对；`gofmt -l .` 必须无输出。
**出处**：DESIGN §2 目标 3、§7、§8；AGENTS「ANTI-PATTERNS」「COMMANDS」；`docs/kernel/00-overview.md` §2.2。

### A4 · 内核装机制、扩展包装策略；观察用事件，改行为用包装

**决策**：压缩算法、记忆整理、进化方法是策略（扩展包），存储接口、事件模型、截断边界是机制（内核）；事件流只读，要改变行为就包装 `Tool` 或 `Provider`（中间件模式），不给 hook 随意改写流量的权限。包装器守则（P2-1）：观察型包装器透传完整 `ToolInfo`、注册名与模型可见名一致；`Execute` 传原 ctx 与 args；包装错误必须 `%w`，不得吞 `ErrStreamingNotSupported`；流式包装器不得预先 range，保持 `iter.Seq2` 惰性。
**背景/问题**：控制面对比 pi-mono 时确认这是设计选择而非缺陷，但它曾把"防 hook 破坏不变量"与"不提供 hook"绑在一起，拒绝了比必要更宽的东西。
**被否决的备选**：内核定义大而全的钩子面（pi 形态：hooks/steering/QueueMode/finishTurn/prepareRequest 全在内核层）与通用 hook 总线（`afterToolCall`/`transformContext`/`convertToLlm`）——均否决；保留的是"限点位、强契约、有限能力的内核缝隙"（steering、FinishRound、config 三个契约，见 C1–C17）。
**结论**：控制面落地后原则修正为"不给改写**流量**的权限"——FinishRound 是决策不是改写；中间件示例见 `examples/middleware`。
**出处**：DESIGN §4；CDR §1；CPD §0、§8。

### A5 · 单一执行路径：流是执行原语，同步执行是衍生物

**决策**：只维护一条模型/工具执行路径 `runStreamInternal`；`Run`、恢复 active run 的 `ResumeThread` 都是事件流的归并；流式形态统一 `iter.Seq2`，公开 API 无 channel，early-break 安全。
**背景/问题**：P0 之前 `Run` 与 `RunStream` 分叉，每个行为修两遍、测试矩阵翻倍。
**被否决的备选**：并行的 Run 与 RunStream 两条路径（P0 合并，反模式明令"不再长出第二条路径"）；独立的持久化入口 `RunThread`/`RunThreadStream`（2026-10-29 Run API 重塑 D1 裁决：直接删除，不留过渡期）。
**结论**：持久化路由统一为 `Run`/`RunStream` 的逐调用 `RunOption`（见 P1）。
**出处**：DESIGN §4「流是执行原语」、§5 P0、§8（Run API 裁决）；agent-AGENTS「ANTI-PATTERNS」。

### A6 · 密封接口纪律：事件七种不增不减，Provider 故意不密封

**决策**：`AgentEvent` 七种密封变体（`text_delta`/`thinking_delta`/`tool_call`/`tool_result`/`retry`/`done`/`compaction`）不增不减；数据类型密封经未导出 marker method，type switch 必须处理 value 与 pointer 双形态；`llm.Provider` 接口故意不密封（外部可自行实现接缝）。
**背景/问题**：sealed union 保证事件折叠的穷尽性；Provider 是需要开放实现的接缝——"为什么 Provider 不密封、数据类型密封"有成文论证。
**被否决的备选**：未记录（密封与开放的划分未见对立提案）。
**结论**：控制面三契约均承诺"事件面不变"——能力不新增事件类型，插话以 history 形式存在、不以事件重放；steering 注入不产生新事件。
**出处**：DESIGN §3、§5 运行中控制面；`docs/kernel/00-overview.md` §5；`docs/kernel/20-llm-model.md` §1.2；agent-AGENTS「CONVENTIONS」。

### A7 · glm/openai 转换代码故意不抽象（复制是有意的）

**决策**：glm 与 openai 适配器的消息转换代码按设计重复，改一头必须手动同步另一头；禁止抽公共转换 helper。
**背景/问题**：两个适配器面向不同厂商 wire 协议，表面相似的字段在协议演进时分化；共享转换层会把两个协议的变更耦合成一次合并事故。
**被否决的备选**：dedupe 出共享转换 helper——AGENTS 反模式明令（"duplicated by design — change both"）；重新合并的触发条件在 21 篇 §4.3 有明确界线（协议面真正收敛时再评）。
**结论**：复制纪律冻结；两家适配器各自的出站转换含跨厂商推理块降级表（见 C16）。
**出处**：AGENTS「ANTI-PATTERNS」；`docs/kernel/21-llm-transport.md` §4。

## 2. 持久化

### P1 · 内核侧持久化 + 逐调用 WithThread 三态

**决策**：Store 内核内置（`WithStore` 构造选项）；持久化路由为 `Run`/`RunStream` 的逐调用 `RunOption` `WithThread`，三态语义：缺省 = 一次性运行（零开销——配置了 store 也不生成线程 ID、不触碰持久化路径）、空串 = 内核生成新线程、非空 = 续接既有线程（首调即创建）；重复 `WithThread` 后者胜。
**背景/问题**：Store v1 设计时曾按消费者侧持久化（D1 原推荐）论证；对抗评审推翻"checkpoint = 正确性必需"论证后，owner 裁定持久化必须由内核做（生命周期记录链由内核写入）。
**被否决的备选**：消费者订阅事件侧写持久化（pi 形态——队列在内存、abort 即退还；RAR §3.3 复核确认 go-agent 当前事件接口不能直接替代内核持久协议：运行身份、回放与 SettlementToken 都与内核持久协议绑定）；`RunThread`/`RunThreadStream` 保留过渡期（2026-10-29 D1：直接删除）；`WithThread` 未配 store 静默降级为非持久运行（D2：同步返回 `ErrNoStore`，含空串）。
**结论**：`RunWithHistory`/`RunStreamWithHistory` 保持一次性导入语义，不入线程；agenttool 经 `Config.RunOptions` 透传（仅期望 `WithThread("")`，固定非空 ID 破坏每次 Execute 无状态承诺——D3）。
**出处**：DESIGN §5 P1、§7（st_01a08a3f 对抗评审）、§8（2026-10-29 Run API 裁决 D1–D3）；RAR §3.3、§4「保留」；`docs/kernel/12-agent-persistence.md` §4。

### P2 · append-only 日志为主体，checkpoint 是可重建加速器

**决策**：线程 = 规范记录的 append-only 日志，`Seq` 单调编号；checkpoint 只在 active 且无 open declaration 的压缩边界写入、携带 canonical history 投影、定位在自身 Seq，可丢弃重建，不是生命周期真相，也不承担文件系统回滚。
**背景/问题**：Store v1 初稿把 checkpoint 论证为"正确性必需"；对抗评审用"CompactionEvent 本身可全量重放"的事实推翻该论证，检查点定位修正为加速器。
**被否决的备选**：可变状态快照每轮覆盖写（崩溃窗口里"改到一半"新旧不可区分，恢复语义无从谈起——append-only 让每个崩溃点对应日志的合法前缀）；把多记录 Append 当断电事务（契约明确"JSONL 多记录 Append 不被当作断电事务"，用前缀幂等补偿，把对后端的事务性要求降到"单条崩溃原子 + 前缀分类重试"）；checkpoint 作为正确性必需（论证有误）。
**结论**：旧版本快照遇含新 kind 的日志**无条件**回退 `History(ctx, thread, fromSeq)` 全量重放——"版本标记不得让旧 reader 借旧快照绕过新语义"。
**出处**：DESIGN §5 P1、§7；`docs/kernel/30-store.md` §1.4、§4；`docs/kernel/12-agent-persistence.md` §1.3。

### P3 · 两阶段 declare/commit，先落库后行动

**决策**：`round_declared`（原子轮次宣告：完整助手消息——文本/推理 + 全部有序工具调用，先落库再执行任何工具、先于审批回调）→ `round_committed`（按宣告顺序的全部模型可见结果，含软失败：审批拒绝、未知工具、nil 结果、恢复的 panic；结果先截断后落库，与内存视图同源）；成功终态在 `DoneEvent` 发出前落库；`run_started` 逐字节保留运行输入与线程初始 system（回放以落库为准，防历史上下文被静默篡改）。
**背景/问题**：崩溃恢复需要"宣告了什么"与"拿到了什么"两个事实点；宣告先行使中断轮可判定、可合成配对结果。
**被否决的备选**：消费者订阅事件侧写日志（见 P1）；其余记录形态未见成文提案（未记录）。
**结论**：durable-before-visible 纪律推广到全部提交点：declareRound 先于 announce/execute/approval，compaction checkpoint 先于 CompactionEvent，terminal state 先于 DoneEvent。
**出处**：DESIGN §5 P1；`docs/kernel/12-agent-persistence.md` §1.2；agent-AGENTS「CONVENTIONS」。

### P4 · 恢复语义：不重放历史事件、不盲目重执行、unknown 如实合成

**决策**：恢复 = 重放日志重建状态，绝不向新流重放历史事件（新消费者只看本次运行的事件）；中断轮 = 有 `round_declared` 无 `round_committed`，对每个未提交调用合成一条 role=tool、`IsError=true` 的"结果未知"消息（每调用一条、按宣告顺序，满足 TC/TR 配对与 OpenAI/GLM 转换要求）；maxIter 截断 = 确定未执行 → 合成"因达到迭代上限未执行"软结果；绝不盲目重执行工具。
**背景/问题**：TC/TR 配对是出站转换的硬性要求；工具副作用不可逆，恢复必须诚实——"保存历史、接管运行、撤销文件是三份承诺"（RAR §3.3）。
**被否决的备选**：重执行工具（仓库内无成文提案——未记录；业界对照 Temporal 事件回放"不重新执行不确定操作"）；伪造 Done 或假 assistant 消息（"不写假 Done"原则）。
**结论**：崩溃先于终态落库的模型回复可能丢失并在恢复时重新生成（at-least-once，显式记录）；工具副作用完成后、`round_committed` 落库前的崩溃窗口如实报"结果未知"（不可避免的副作用窗口）。
**出处**：DESIGN §5 P1 恢复语义、§6；`docs/kernel/12-agent-persistence.md` §3；RAR §3.3。

### P5 · run_cancelled 单条原子取消记录（R07）

**决策**：`SettleThread(ctx, target SettlementToken)` 仅在 ThreadID、RunID 与 head 都匹配时以单条 `run_cancelled` 记录（schema=2、固定 RunID、`-cancel` 后缀记录 ID）完成持久结算；有 open declaration 时该记录同时保存全部未提交调用的有序 outcome-unknown 结果；无 open 时不得携带工具结果；已提交结果不改写、不追加假 assistant、不重执行、不回滚外部副作用。
**背景/问题**：调用方明确放弃未终结 run 需要终态；普通错误/ctx 取消默认保留可恢复的未闭合 run。
**被否决的备选**：追加假 assistant 最终回复 / 写 Done（明确禁止）；老二进制借未知 kind 隐藏新终态（必须报兼容性错误）。
**结论**：结算不发送取消信号、不调用模型或工具；错 run/round、错误配对、额外内容、重复 terminal 均 `ErrIncompatibleLog`；结算与随后同线程新 run 是两个所有权区间。
**出处**：DESIGN §5 P1「取消结算与日志兼容性」。

### P6 · 每线程非阻塞运行所有权 + SettlementToken + 非递归父子结算

**决策**：第二个并发运行在任何模型调用之前即得类型化冲突（`ErrThreadBusy`）；所有权按（Store 实例, 线程）全局登记，不按 Agent 实例；Store 值必须可比较（否则类型化错误不 panic）；`SettlementToken{ThreadID, RunID, ExpectedHead}` 必须在原 session 释放 ownership 前冻结，不得释放后从 Latest 选取；`SettleThread` 非递归——父结算不关闭子线程，子悬挂调用由子负责配对；嵌套 RunInterruptedError 外层 token 属父、内层属子。
**背景/问题**：串行化等待会伪装成冲突；迟到请求不得改变 revision。
**被否决的备选**：单纯加锁等待（显式否决——"串行化，不是冲突"）；跨 Store 事务 / durable 委派关系 / 递归结算（R07 不提供）。
**结论**：revision 或身份不符即 `ErrRevisionConflict`，不自动刷新；外部入队落库后，陈旧 token 的盲结算按冲突失败——"这是特性不是缺陷"（插话落库的事实应当阻断陈旧 token）。
**出处**：DESIGN §5 P1 并发、父子线程取消结算；CPD §5；`docs/kernel/12-agent-persistence.md` §2。

### P7 · 显式 DTO + 严格解码 + 未知 kind fail-loud

**决策**：store 记录用显式 DTO 编解码（sealed 接口直接 JSON 不可逆）；未知记录类型 / 信封版本 → 类型化兼容性错误 `ErrIncompatibleLog`，不静默忽略；旧版本偷带新字段仍拒绝；未知未来版本的 checkpoint 经 `History` 全量回退。
**背景/问题**：日志是跨版本长期资产，静默丢弃等于数据说谎。
**被否决的备选**：未知 kind 静默跳过 / 忽略（明确否决——"不得借未知 kind、KindError 或 checkpoint 隐藏新终态"）。
**结论**：与 store 层职责分工见 P9：写侧从严、读侧放行，协议策略只住 agent 重放器一处。
**出处**：DESIGN §5 P1 版本化信封；`docs/kernel/12-agent-persistence.md` §5；`docs/kernel/30-store.md` §1.3。

### P8 · 幂等重试逐字节同批

**决策**：不可变记录批次与 ID 只生成一次，重试逐字节同一内容；幂等分类算法（已落盘前缀的逐字节识别）是 store 与 agent 的共享代码；unset schema 在 `prepareBatch` 归一为 1，"忘了设 schema 的重试"仍与首次写入匹配。
**背景/问题**：断电可能让批次的任意前缀落盘；确认前写入错误可能已落地。
**被否决的备选**：后端提供跨记录断电事务（见 P2）；未记录其他提案。
**结论**：写入结果不确定时保留原 ID/head 幂等重试；最后确认的 head 可能落后时必须冲突失败而不是猜测。
**出处**：DESIGN §5 P1 并发；`docs/kernel/30-store.md` §5.3、§2.2。

### P9 · 写侧从严、读侧放行的 kind/schema 校验不对称

**决策**：store 认识 11 种 kind 但语义一概不知（语义归属 agent 层）；`Append` 拒绝未知 kind（`ErrUnknownKind`——"a stale binary cannot write records it does not understand"）与越界 schema；五种控制面/取消 kind（`run_cancelled`/`config_changed`/`input_queued`/`input_consumed`/`run_closed`）的 schema 钉死恰好 = 2；读取容忍未知 kind、原样透传。
**背景/问题**：协议策略只住 agent 重放器一处；如果 store 也对 kind 有意见，两层可能对同一条记录做出不一致裁决。
**被否决的备选**：store 层解释 kind 语义（否决——两层裁决会分叉）。
**结论**：agent 侧对未知 kind 的真实裁决是 `ErrIncompatibleLog`（见 P7）；读侧现存缺口见 S1。
**出处**：`docs/kernel/30-store.md` §1.3（store/store.go `validateRecord`）。

## 3. 控制面

控制面三契约（steering 队列 / 配置热更新 / FinishRound）共享一条总原则与一组不变量；D1–D5 为取舍组裁决、D6–D10 为阻塞性正确性裁决，均于 2026-10-28 由 owner 按推荐裁定（CDR §5、CPD §9–§10）。

### C1 · 总原则：外部意图走线程日志，回合边界生效，接受即持久化

**决策**：运行中来自外部的意图——插话、改配置、叫停——一律走线程日志，在回合边界的 drain 点生效；**接受即持久化**（accept implies persisted）：API 返回成功的时刻，消息必已在日志里，此后任何崩溃不背叛该承诺。未消费的插话是队列成员而非 history 成员——落库保护的是 API 的必达承诺。
**背景/问题**：对照 pi-mono 内核发现 go-agent 缺 steering 与回合间钩子；业界四家（Claude Code hooks、OpenAI Agents SDK RunHooks+max_turns、LangChain hooks-middleware、PydanticAI v2 Hooks）全部收敛在内核定义类型化生命周期钩子。go-agent 的差异化不是"也能插话"，而是**插话和对话一样可恢复**（pi 的队列在内存里，abort 即退还）。
**被否决的备选**："持久化要趁早"的表述（修正为"接受即持久化"——它允许"不接受"的合法实现）；v1 降级（run 期间拒绝入队，owner 否决："运行中插话"是功能定义本身）。
**结论**：三个契约共享同一组不变量：意图先持久化后生效（与"提交点先落库后行动"同构）、生效点唯一且在回合边界（不触碰 TC-first、declare/commit 两阶段、单一执行路径）、Resume 时日志重放即状态重建（无内存态依赖）。
**出处**：CPD §0；CDR §4 原则 A、§3 对照表。

### C2 · 分层判据：什么进内核

**决策**：判据一（执行时支持）——需要循环内部执行时支持的能力进内核，构造期能表达的不进（进：steering drain、回合末合作式终止、每轮配置读取；不进：模型/工具供应商级切换、per-tenant 能力面隔离）；判据二（状态归属）——per-thread 的东西（历史、插话、配置选择）= 线程日志状态，per-tenant 的东西（Registry、审批策略、system prompt 基线）= 独立 Agent 实例。
**背景/问题**：讨论中收敛的两条判据，比任何具体功能都重要；Agent 构造廉价、实例不可变天然并发安全。
**被否决的备选**：单实例多租户 + 运行时过滤（明确否决的方向）；`tools_changed` / per-thread 工具过滤（per-user Agent 实例 + 共享 Store 组合在构造期完成隔离）。
**结论**：与 A4 的"有限内核缝隙"原则互相锚定。
**出处**：CDR §2；CPD §8。

### C3 · 控制面 D1：匿名 run 不支持 steering

**决策**：无 store 的匿名 run 不支持 steering（`ErrNoStore` 语义延伸）；FinishRound 是纯内存回调，匿名可用。
**背景/问题**：steering 的价值主张就是 durable，内存版是退化形态。
**被否决的备选**：内存队列支持（引入两套语义）。
**结论**：与 `WithThread`、`ResumeThread` 等显式线程入口同规要求 Store。
**出处**：CDR §5 取舍组 D1；CPD §9-D1。

### C4 · 控制面 D2：QueueMode 固定 drain-all

**决策**：v1 队列消费模式固定 drain-all（一次排空 FIFO 前缀）。
**背景/问题**：pi 的 one-at-a-time 服务长对话逐条消化场景，go-agent 无此实证需求。
**被否决的备选**：仿 pi 双模式（可后加，记录格式不变，纯运行时策略）。
**结论**：记录格式不为 QueueMode 预留。
**出处**：CDR §5 取舍组 D2；CPD §9-D2、§8。

### C5 · 控制面 D3：残留队列 = 下一 run 首轮消费（follow-up 免费）

**决策**：run 结束时残留队列保留不消费，由下一个 run 的首轮 drain 消费；`DoneEvent` 不加字段。
**背景/问题**：`EnqueueInput` 在无 active run 时天然成为 follow-up，零新机制。
**被否决的备选**：DoneEvent 报告计数、由宿主决定（引入事件面变更；日志里的事实自明）。
**结论**：新 run 的输入先入 history，D1 再把积压插话补进首轮请求之前。
**出处**：CDR §5 取舍组 D3；CPD §9-D3、§3.4 D2。

### C6 · 控制面 D4：End 以工具结果收尾 + 探针前置

**决策**：FinishRound End 的 history 以本回合工具结果收尾（正常 Done，`Truncated:false`），不合成收尾消息；adapter 层新增明文契约"outbound 序列以配对完整的 tool result 收尾是合法形态，adapter 负责转换为各协议的合法 wire 表示"。
**背景/问题**：模型没说过的话不伪造（同"不写假 Done"）；该形状的 wire 层容忍度初稿留作实现期测试，owner 裁定先调研——业界证据（PydanticAI 修复层、pi#8756、vercel/ai PR#8524）表明该形状是崩溃恢复的业界常态，解法在 adapter 层归一化。
**被否决的备选**：合成一条 assistant 收尾消息；自研探针兜底而不调研（第一版方案，"先调研再设计"教训）。
**结论**：生效前置 = 三 adapter 探针验证（2026-10-28 已执行全过；服务端真实接受度纳入实现期 e2e）；该形状在现有系统已由 maxIter truncateRun、settle 合成未知结果、early-break 修复三条路径产生，探针同时验证存量行为。
**出处**：CDR §5 取舍组 D4、阻塞组 D7；CPD §12-1。

### C7 · 控制面 D5：决策钩子只决策

**决策**：`FinishRoundDecision` 不允许携带 config 覆盖；改配置走 `SetThreadConfig`，两条路径独立可推理。
**背景/问题**：单一职责——钩子回答"应不应该停"，配置回答"下一轮用什么"。
**被否决的备选**：decision 带 optional override（合并场景）。
**结论**：配置的操作身份与 steering 不同（见 C12）。
**出处**：CDR §5 取舍组 D5；CPD §9-D5。

### C8 · 控制面 D6：外部写入 = 乐观吸收协议

**决策**：runner 不盲信 session 缓存 head：每个写点（declare/commit/checkpoint/终态）Append 前重读日志尾，吸收白名单控制记录（`config_changed`/`input_queued`）后按真实 head 重试；遇白名单外未知记录仍冲突失败；外部 `EnqueueInput`/`SetThreadConfig` 在 run 活跃期间允许并发写入（bounded 次乐观重试，超限 `ErrThreadBusy`）。
**背景/问题**：runner 以缓存 head 追加与外部写者并发插入冲突；原表述"绝不影响在途 run"不成立。
**被否决的备选**：v1 降级（run 期间拒绝入队——owner 否决）；runner 代写（(b)：接受与落库分离，崩溃丢承诺）。
**结论**：吸收白名单是长期纪律——漏登记 = 偶发冲突，测试钉死"未知记录必须冲突"；已知代价（owner 知情接受）：Store 新增窄尾读接口、写点尾读常数级 I/O。
**出处**：CDR §5 阻塞组 D6；CPD §10-D6。

### C9 · 控制面 D7：FinishRound End 终态 = 新增 run_closed 记录

**决策**：End 的终态用新增版本化 `run_closed` 记录（schema=2，payload `{run_id,tool_calls,usage,total_usage}`，不携带消息），与 `run_cancelled` 对称——宿主策略收尾 vs 调用方放弃；重放层要求 active run、无 open declaration、RunID 匹配。
**背景/问题**：重放层拒绝"含工具调用的非 truncated Done"，复用现有 Done 编码走不通；传空 assistant 又伪造消息。
**被否决的备选**：扩展 Done 编码携带"终态无消息"标志。
**结论**：`DoneEvent.Message`/`RunResult.Message` 定义为本回合 assistant。
**出处**：CDR §5 阻塞组 D7；CPD §10-D7。

### C10 · 控制面 D8：钩子前移 + declare 泛化 terminal_text

**决策**：FinishRound 调用点移到终态判定之前（否则对"模型想停"的分支不可达，而那正是 Continue 的主用途）；无工具回合以 `terminal_text` 子状态持久宣告（`round_declared` 的 kind 字段泛化：空 = 传统工具回合；重放层接受无工具调用的 terminal_text 宣告并并入 history，其余无工具宣告仍拒绝——加白名单，非放松校验）。
**背景/问题**：无工具响应立即 finishRun，拟定钩子位置对该分支不可达；若被 Continue 延续，其持久化只能以关 run 的 Done 表示，callback 失败/崩溃会丢该 assistant 轮。
**被否决的备选**：Continue 仅在工具回合后可达（能力收窄）。
**结论**：Continue 后崩溃，宣告已 durable，重放不丢回合。
**出处**：CDR §5 阻塞组 D8；CPD §10-D8。

### C11 · 控制面 D9：checkpoint codec 升 3 + 双投影

**决策**：checkpoint `codec_version` 升 3，快照新增 `effective_config` 配置投影与 pending 队列投影（存内容不存 ID）；v1/v2 快照遇新语义无条件回退全量重放。
**背景/问题**：队列/配置投影不入快照则 checkpoint 之后的控制语义丢失；不升版本则旧 reader 借 checkpoint 绕过新语义。
**被否决的备选**：不升 codec 版本（与 P2 的回退裁决同构，直接延伸）。
**结论**：`input_consumed` 引用被消费的 `input_queued` 记录 ID（FIFO 前缀精确界定，非文本匹配）。
**出处**：CDR §5 阻塞组 D9；CPD §10-D9。

### C12 · 控制面 D10：幂等语义 = 操作身份，不是内容哈希

**决策**：steering 的幂等键由调用方提供（操作身份 = 调用意图：同键跨重启确认，`ErrInvalidRecord` 时全量日志回查；不同键同文本是两条消息）；config 的操作身份是"每次调用"（fresh 随机 ID，A→B→A 必须终态 A）。
**背景/问题**："重读 head、同内容重试"对 ack 丢失不成立；"哪次调用算同一条消息的重试"只有调用方知道，内核不做隐式哈希。
**被否决的备选**：内容哈希作操作身份（内容相等不等于操作相等——评审第一轮 blocker）；API 仅文档化不确定结果 + 调用方去重责任。
**结论**：两种身份都是对的——steering 按意图、config 按次数。
**出处**：CDR §5 阻塞组 D10；CPD §10-D10、§3.1。

### C13 · 控制面三条评审修正案：同单位计量、护栏只放宽、删 force 改延后

**决策**：①准入与排水全部以**序列化消息**计量（`estimateRunes`，与压缩/drain 预算同单位——转义文本 raw 30 rune 可序列化成 115，raw rune 准入可被穿透后永不排水）；②预算护栏只能放宽、不能收紧（护栏 = 旧基线与注入容忍取大；曾把 provider 场景从 canonicalCap 收紧到 target，破坏 nil-compactor 既有契约——无压缩时大历史本应放行）；③删除 force 注入，改为**延后排水**：drain 只消费装得下的 FIFO 前缀，装不下的留在队列随下一轮压缩重试，绝不 force 注入、绝不因注满杀 run。
**背景/问题**：v2 评审拉锯暴露的三个原方案缺陷，均有确定性回归测试锁死。
**被否决的备选**：raw rune 准入；收紧式护栏；force 最老一条（隐含"压缩必达 target"假设——压缩禁用/失败时仍触发预算错误杀 run）。
**结论**：已接受的输入可能迟到（容量不足的诚实信号），但绝不会把 run 顶过硬限制；存量测试（nil_compactor、compactor_failures）是护栏纪律的守门员。另附流程教训：修复写入必须逐步自校验落盘（见 Q2）。
**出处**：CDR §5「评审拉锯产生的三个修正」。

### C14 · drain 顺序：读配置 → 压缩 → drain → 预算检查 → 请求

**决策**：每轮顶部固定顺序为读配置 → **压缩 → drain** → 预算检查 → 请求（先压缩后注入）。
**背景/问题**：owner 提出——压缩基于上一圈结束时的稳定 history 决策；新消息注入在压缩后天然免于被压掉；配合准入硬限制，消除"用户插话触发 `ErrCompactionBudgetExceeded` 杀 run"的路径。
**被否决的备选**：drain 在压缩前（v2 原序：读配置 → drain → 压缩 → 预算 → 请求）。
**结论**：drain 是独立单记录 Append（`input_consumed` 永远落在前一轮 commit 之后、本轮宣告之前——两时刻之间隔着一次完整 LLM 请求，合批在设计上不可能）。
**出处**：CPD §12-3、§3.4；CDR §4 原则 B。

### C15 · maxIter 熔断优先级最高，不可被回调延长

**决策**：数值熔断保留且优先级高于一切决策回调——最后一片允许迭代的圈（`i == maxIter-1`）的 Continue 视为默认调度（进入 terminal skip），不可延长 run；terminal skip 分支零决策零执行保持冻结（不 drain、不咨询钩子）。
**背景/问题**：护栏必须不依赖外部代码的正确性——maxIter 回答"还能不能跑"，FinishRound 回答"应不应该停"，分层并存（与 OpenAI Agents SDK 的 max_turns + hooks 同构）。
**被否决的备选**：Continue 延长预算护栏；回调判定取代 maxIter。
**结论**：文档如实写明 Continue 的"保证再发一次请求"受 maxIter 约束。
**出处**：CDR §4 原则 D、§12-2；CPD §12-2。

### C16 · provider 热切换：成对切换、线程权威、轮顶生效、降级住适配器

**决策**：切换通道即 `SetThreadConfig`（`ConfigChange{Model,Thinking,Provider}`），粒度为线程级持久记录；**provider+model 成对切换**（携带非空 `provider` 的记录必须同时携带非空 `model`，写面拒绝、解码面 `ErrIncompatibleLog`），同记录原子生效；线程模型记录一经写入即权威，机械遮蔽 agent 级 `WithModel`（选项后写胜、适配器 options 模型优先，无抑制规则）；切换在下一轮顶部生效，永不打断在飞请求，同轮全部 retry attempt 复用同一 provider；解析到未注入的名字该轮以 `ErrUnknownProvider` fail-closed 中止，线程可由后续配置记录修复；`agent.New` 位置参数 provider 为默认，`WithProviders(...)` 追加备用（键 = `Provider.Name()`，重名后者胜）；跨 provider 回放的历史**不改写**——降级规则落在适配器出站转换层（内核 loop/store/重放零参与）：openai 跳过 `ReasoningItemBlock`、透传 `ReasoningBlock`；glm 两种推理块均 strip；openairesponses/codex 回放自有 `ReasoningItemBlock`（加密内容 + ID 原样、保序）、丢弃外来 `ReasoningBlock`；任何适配器不因外来推理块硬错误；codex 对 ID/Name 缺空或非法 JSON arguments 的既有校验照旧 fail-loud；**rs_ 条目保序约束**——截断/压缩丢弃历史时 `ReasoningItemBlock` 必须与其后配对的 message/function_call 条目一起按序保留或丢弃（孤儿 rs_ 项会被 OpenAI 400，openclaw #1540 根因）；摘要压缩跟随线程生效 provider 与 options（见 Q7）。
**背景/问题**：`llm.Options` 本是调用级，但 `a.llmOpts` 在 `New` 时焊死；热切换若只切 provider 不动 model 则模型语义悬空。
**被否决的备选**：仅切 provider 不携带 model（写面拒绝）；内核统一改写跨厂商历史（否决——降级契约住适配器）；v1 字段域刻意收窄为 Model + Thinking + Provider（Temperature/MaxTokens 等留待真实需求再扩，扩字段 = payload 加列，向前兼容）。
**结论**：旧二进制遇含 `provider` 字段的记录按 strict-decode 纪律 fail-loud；跨厂商选项（Stop/JSONMode/MaxTokens/Temperature/Thinking）随线程跨 provider 生效，目标适配器按既有契约接受、显式拒绝或静默忽略（差异由适配器文档如实承载）。
**出处**：DESIGN §5 运行中控制面①；`docs/kernel/14-agent-control-plane.md` §3–§4；agent-AGENTS「WHERE TO LOOK」（config.go）。

### C17 · 控制面明确不做清单（negative decisions）

**决策**：不做 `tools_changed` / per-thread 工具过滤；不做 `afterToolCall` 结果改写、`transformContext`、`convertToLlm` 通用钩子（与 Tool/Provider 包装器、Compactor 链职责重复，双轨制是净损失）；不做 QueueMode 双模式（见 C4）；不做中断式插话 abort-and-reprompt（ctx cancel + settle + resume 已覆盖，选择权在交互层）；不做 follow-up 独立队列（由 C5 零新机制覆盖）。
**背景/问题**：negative decisions 防止范围蠕变。
**被否决的备选**：即清单中各项，理由逐条见出处。
**结论**：清单与 DESIGN §5 运行中控制面同步维护。
**出处**：CPD §8；CDR §2。

## 4. 工具与 schema

### T1 · 软失败双通道契约：ToolResult{Status: ResultError}，不是 Go error

**决策**：业务失败（坏参数、资源不存在）返回 `ToolResult{Status: ResultError}` 喂回模型，循环继续；Go error 只留给"循环本身无法继续"的系统级故障；双向禁令：软错误 ToolResult 不得提升为 Go error，Go error 也不得降级为软结果；`NewErrorResult` 是唯一合法的软失败构造器（禁止手写 Status 字符串）。
**背景/问题**：四层理由——①失败的归属不同（"模型与工具的对话问题" vs "程序与环境的故障"，塞进一个通道要么把对话问题升级成事故、要么把事故降级成噪声）；②TC/TR 配对必须永远完整（Go error 冒泡会留下未配对的 tool_use，历史从此不合法——无法 commit、无法 resume）；③循环连续性（自主恢复是内核核心价值，一次坏参数不该打断整个 run）；④软失败是一等公民：进 history、进 Store（round_committed 含审批拒绝、未知工具、nil 结果、恢复的 panic），失败轨迹不消失。
**被否决的备选**：工具错误也返回 Go error、循环层统一转软（"哪些 error 该转"恰恰是工具作者掌握的领域知识，接口层强制作者表态，内核不做猜测）；硬错误物化成占位结果跑完本轮再报告（违反禁令后半句，掩盖"环境已不可信"）。
**结论**：软错误同样受结果限长约束（报错文本也占上下文预算，上限约窗口 30%，见 Q6）。
**出处**：DESIGN §4；TOOL §4.3；`docs/kernel/11-agent-tools.md` §3.1–3.2；AGENTS「ANTI-PATTERNS」。

### T2 · args 原样透传，schema 只描述、不校验

**决策**：`Execute(ctx, args json.RawMessage)` 原样透传模型产出的原始 JSON（不丢失字段顺序、重复键）；仓库内没有任何地方拿 schema 校验参数；校验只发生在 `Registry.Register` 时针对 schema 自身结构。
**背景/问题**：三个成文理由——模型 args 解析失败时软失败喂回、模型自纠成本低；维护 JSON Schema 校验器是巨大的第三方面积，与内核零依赖冲突；校验器误杀合法参数的代价高于放过非法参数（后者可恢复，前者不可）。
**被否决的备选**：内核参数校验执行（TOOL §7 列为非目标）；args 用 `map[string]any` 或泛型（反序列化是工具自己的事，接缝不承担类型系统翻译成本）。
**结论**：反序列化失败的边界 = 工具内软失败；`Data` 只对当次运行的 `ToolResultEvent` 消费者可见，history/Store 只保留 `Content`（结构化数据可能很大，不应膨胀持久化日志）。
**出处**：TOOL §4.1、§4.3、§5.3、§7。

### T3 · 审批 fail-closed

**决策**：`RequiresApproval=true` 且未配置审批回调时，工具被拒绝执行，绝不默认放行；MCP 桥 `WithApprovalRequired` 默认 true（宽松是被动的、收紧是默认的）。
**背景/问题**："默认安全"设计目标的锚点；MCP 的"默认全部审批"只是在这个锚点之上叠加的策略。
**被否决的备选**：无回调时默认放行（AGENTS 反模式明令 fail-closed）。
**结论**：审批回调与执行同一同步边界；消费者未确认宣告会推迟对应执行。
**出处**：TOOL §4.2；DESIGN §4「默认安全」；AGENTS「ANTI-PATTERNS」；MCP §4.2。

### T4 · ParameterSchema：递归类型模型 + presence state + carrier，禁止 raw escape hatch

**决策**：`ParameterSchema` 是递归 typed fields（`Type`/`Properties`/`Items`/组合子/`$ref`）+ 按名称排序的完整词汇 carrier（`SchemaKeyword{Name, Value}`），不是 `map[string]any`；未导出 presence 标志区分"字段为零值"与"字段未出现"（`type: ""` 与 `type` 缺失是两个不同的 schema）；null-first 归一化；codec 只保真不解释——绝不解释 carrier 的 JSON Schema 语义，自己的 raw-JSON 路径拒绝重复键；**whole-schema raw escape hatch 禁止**；adapter 不得静默降级 schema；外部 keyed construction 的 presence/wire 语义有明文表格（`UnmarshalJSON` 是唯一产生 explicit-empty pathological presence state 的路径）。
**背景/问题**：JSON Schema 经过 Go 结构体往返一次后，模型看到的东西必须与原始 schema 语义等价——三个手段：presence 标志消除零值歧义、有序 carrier 保留关键词出现顺序（顺序丢失会导致同名关键词语义翻转）、循环检测（见 T5）。
**被否决的备选**：`map[string]any` 直载 schema（零值歧义、顺序丢失、无法结构校验）；whole-schema raw 逃生门（绕过全部结构检查）。
**结论**：Registry 只作 typed graph 结构检查、从不解释 carrier；remote MCP 的保真限 SDK decode surface（duplicate key、encounter order、精确 numeric token 已在 SDK 边界丢失）。
**出处**：DESIGN §4；TOOL §4.5、§5.2；`docs/kernel/31-tool.md` §4.1–4.7。

### T5 · schema 环检测 fail closed，绝不挂起

**决策**：`$ref` 指向的递归结构由 marshal/unmarshal 共享的 active-stack 检测器处理，检测到环即报错。
**背景/问题**：朴素的 `encoding/json` 会栈溢出或死循环；标准库对自定义 `MarshalJSON` 没有任何循环保护。
**被否决的备选**：未记录（无对立提案）。
**结论**：环用例断言返回错误而非超时。
**出处**：TOOL §5.2；`docs/kernel/31-tool.md` §4.5。

### T6 · Registry Info 冻结与两层读缓存（P2-5，裁决 2a + D3）

**决策**：`Info()` 按注册时刻不可变处理（业界共识语义，经外部调研裁决）：`Register`/`Replace`/`Apply` 在取锁前调用一次 `Info()` 完成全部校验，随后深拷贝冻结为注册表内部 canonical，并把冻结 schema 的序列化字节缓存于条目内；`List()` 不再调用任何用户代码，返回排序快照的逐调用深拷贝；两层缓存：条目层（tool + 冻结 info + 序列化字节，插入后不可变）+ 聚合层（排序快照，写路径同一临界区置脏，`List` 惰性重建，批量 `Apply` 折叠为一次重建）；重建只做排序与装配，深拷贝与 marshal 都在锁外；注册后工具实现修改自身状态、或 `Info()` 返回漂移值（含改名），均不影响已注册视图；需要刷新元数据用 `Replace` 整体换实现（原子重算缓存）；注册时序列化失败即注册失败、零变更（现有校验严格覆盖序列化前置条件，该分支正常不可达，属防御性前置）。
**背景/问题**：旧 `List` 在 RLock 内对每个工具调 `t.Info()`——三个具象危害：确定性死锁三角（工具 `Info()` 内调 `Register`：RLock → Info → Lock → 等 RLock 释放，三角互等）；慢 `Info()` 阻塞一切写者；漂移别名（快照跟着工具内部状态漂移、List 显示名与 `Get` 键脱节）。调研的全部对照运行时（MCP go-sdk / genkit / OpenAI Agents / LangChain / Eino）都不在共享锁内执行用户描述回调。
**被否决的备选**：`List` 锁内活调 `Info()`（现状即被否决形态）；`List` 浅共享冻结快照（任一消费者嵌套误写会静默腐蚀进程级共享缓存，爆炸半径是共享它的全部 Agent）；`Get` 走缓存（执行路径需要的正是活的 `Tool` handle）；校验在锁内（用户 `Info()` 不可信——可能慢、可能 panic，绝不在持锁期间调用）。
**结论**：Registry 任何方法在持锁期间不执行用户代码、不执行 schema 序列化、零 I/O；`List` 空注册表返回空非 nil 切片（nil 专用于脏标记）；子决策裁决记录状态见 S2。
**出处**：CACHE 全文（§1 问题、§5、§11 开放点）；DESIGN §5 P2-5（已裁决 2026-10-28）；TOOL §4.4、§5.1；`docs/kernel/31-tool.md` §2–3。

### T7 · 工具热变更三方法（P2-4）

**决策**：`Registry.Unregister(name)` 幂等注销、返回是否移除；`Registry.Replace(t)` 同名严格换实现（校验同 `Register`；名字不存在报错，不做 upsert）、单锁内完成、观察者看不到名字缺席的中间态；`Registry.Apply(removed, added)` 批量原子应用（先全量校验再统一写入，任一失败零变更；removed 中未知名静默忽略）；三个方法可在任意 goroutine、`Execute`、审批回调、RoundContextProvider 回调中调用；Registry 从不回调用户代码、从不等待在飞执行、不新增 goroutine；历史与 Store 永不因变更改写，再次点名已消失名字得到软错误 ToolResult。
**背景/问题**：MCP Refresh 对账与 skills 换血需要原子批变更；Registry 是进程级共享状态，变更影响共享它的全部 Agent/线程。
**被否决的备选**：upsert 语义；运行期变更的 Must 变体；per-agent 工具子集（属未来策略层）。
**结论**：在飞 Execute 自然跑完、结果如实入史，变更在下一个解析点生效。
**出处**：DESIGN §5 P2-4；TOOL §4.4；MCP §4.3。

### T8 · 工具并行执行 opt-in（P2-2）

**决策**：默认串行保证确定性（历史按调用顺序追加）；`WithToolConcurrency(n)` 显式 opt-in（`n <= 0` 归一为 1，默认也是 1）；`n > 1` 三阶段流水线——planner（按声明顺序冻结执行 handle 与 `Info` + 整批 approval）→ dispatch（最多 `min(n, len(calls))` 个 worker，有界）→ settle（槽位状态机：resolved/hardError/missing，有序结算）；完成顺序绝不泄漏到 history、事件或 Store；hardError 优先于 missing；共享 settlement routine 供两种模式，只有规划/dispatch 不同；排空没有人为上界（工具必须 honor ctx，内核不以泄漏 goroutine 伪造终止）。
**背景/问题**：同轮副作用可重叠必须是显式选择；乱序完成不能污染确定性历史；`n=1` 保持可观察行为不变（逐调用 live `Get -> approval -> Execute`）。
**被否决的备选**：默认并行（确定性优先）。
**结论**：并行规划边界与审批路径的活 `Info()` 调用点有意保留（不在注册表锁下，见 T6）。
**出处**：DESIGN §5 P2-2 冻结契约；`docs/kernel/11-agent-tools.md` §1–2。

### T9 · MCP 桥边界：tools-only、静态发现、手动 Refresh、SDK 单一 importer

**决策**：`mcp`（package `mcpbridge`）是全仓唯一允许 import MCP SDK 的包（kernel-closure-guard 字节级钉 v1.7.0）；client-only、tools-only 的静态发现桥：`Connect` 分页拉全 `tools/list` 一次性注册静态 wrapper，不自动订阅 list-changed；对账靠手动 `Refresh(ctx)`——重新分页并经一次 `Registry.Apply` 原子对账自有集合（新增注册、消失反注册、变化 Replace），拉取或 Apply 失败时注册表与自有集合双双保旧；非自有 `ns__` 前缀名永不触碰（撞名列入 Refresh 摘要冲突清单、该名不落地、其余照常）；`Close` 先原子置 closing 门（新 Execute 立即软失败）→ 等写锁排水 → 指针身份校验后反注册仍拥有的名字再关 session；Connect 失败路径清理已注册部分（半接状态不存在）；注册前全量预检（避免"注册了 80 个、第 81 个失败再回滚"）；最终名 literal `${Namespace}__${remoteName}`，namespace 与最终名匹配 `^[A-Za-z0-9_-]{1,64}$`（namespace 不得含 `__`），不转义、截断或加后缀；协议面三层分解：传输与会话归官方 SDK 100%、tools 行本桥逐字段覆盖、非 tools 能力（resources/prompts/sampling/elicitation/logging/roots）v1 冻结不做。
**背景/问题**：SDK 只该进入用到它的二进制；静态发现换来无后台协程、无订阅、可预测。
**被否决的备选**：自动订阅 list-changed / 自动对账（§7 非目标）；agent/server 模式、OAuth 策略、自动安装、网络发现、桥内重试/重连/每工具超时/共享 session（§7 非目标清单）。
**结论**：已知代价如实文档化——未 Refresh 窗口内 stale tool 的 protocol error 是硬错误（Refresh 后注册表未命中走内核软错误）；SDK 边界限制（`ListTools` 静默丢弃非法 `x-mcp-header` 工具、解码丢失重复键/顺序/精确数字字面量）只文档化断言现状，不承诺修复上游。
**出处**：MCP §1–§4、§7；DESIGN §5 P3。

### T10 · MCP 审批与结果投影：默认审批、转义占位、1 MiB Data 预算

**决策**：`WithApprovalRequired` 默认 true；显式 false 时也仅 `annotations` 明确 read-only 且不 destructive 的工具免审批。结果投影：text 按序以 `\n` join 进 Content；image/audio/resource_link/embedded resource 生成按 rune 转义的类型占位符（`\\`/`\"`/`\r`/`\n`/`\]`，远端字段不能闭合占位符或注入另一行）+ 带原 content index 的 `{"mcp_content":[...]}` Data 信封；`WithResultDataLimit` 默认 1 MiB，逐条按最终累计 JSON 编码预检，恰上限接纳，超限条目变 `"[mcp:<type> omitted: bridge data limit]"` 占位文本而非错误；软/硬分界：mapper 失败（结果整形）是软、协议/传输/ctx/nil result/wire-decode 是硬；桥内零重试；`IsError`/`NeedsInput` 是软错误，后者因 `MultiRoundTrip.Disabled:true` 精确一次 call；StructuredContent 紧凑 JSON 为最后一个片段；未知 dynamic content 或 nil embedded resource 是整项软错误（不返回 partial success）；`OutputSchema`、Title/Icons、annotations、`_meta`、progress 全部丢弃。
**背景/问题**：重试语义归上层；Data 仅供 live ToolResultEvent（不膨胀持久化日志，见 T2 结论）。
**被否决的备选**：超限报硬错误（改为占位裁剪）；Data 进 history/Store。
**结论**：schema 可表示 ≠ provider 接受（两件独立的事，投影成功不承诺每个 provider 都吃下这个 schema）。
**出处**：MCP §4.2、§5.1–5.2、§6；DESIGN §5 P3 冻结契约表。

### T11 · skills：distributor 模式 + 手写 frontmatter 子集 + catalog 硬错误

**决策**：整个技能集合注册为**一个**分发器工具：目录清单（名字+描述）长在工具 Description 里，`Execute` 按名字原样返回正文（progressive disclosure 的最小实现：清单便宜常驻、正文贵但按需）；stdlib-only、对内核 diff 零；frontmatter 手写平面 `key: value` 解析器（只认 `name`/`description`），明确不用 yaml；`Set` 不可变（刷新 = 重新 Load + 宿主 `Replace`），包内不做 watcher；catalog 总 rune 超预算是 Load 硬错误（错误信息带条目数、限额与解法）而非静默截断；单技能非法（缺围栏、重复键、块标量、坏行、空正文、重名遮蔽、描述超 1024 rune 截断）跳过并记 Issue，不中断 Load；只扫一层子目录；符号链接不跟随；每份 SKILL.md 以 1 MiB LimitReader 封顶；分发器 `RequiresApproval` 固定 false，永不返回 Go error。
**背景/问题**：工具数量不膨胀模型声明（每个声明都有 token 成本）；被截掉的条目是模型永久发现不了的技能——目录清单的存在意义就是完整性，宁可在装配期炸出来；catalog 在 Description 里而内核截断预算只按历史 rune 计，所以预算由本包自守（默认 2048）。
**被否决的备选**：每技能一个独立工具（token 成本、集合变化影响面大）；yaml 依赖（两个纯文本字段配一个 yaml 依赖是给零依赖原则撒盐；多行块标量/锚点/别名对技能描述毫无意义）；静默截断 catalog。
**结论**：失败策略跟着副作用形状走：skills 天然无副作用（纯构造新值）所以"宽容收集"，mcp 写注册表所以必须 fail-closed-rollback——不是品味差异。
**出处**：SKILLS §2、§4–5.4、§7；DESIGN §5 扩展包。

### T12 · agenttool：无状态单次 Run，固定 input:string

**决策**：`agenttool.New(*agent.Agent, Config)` 固定 required `input:string` schema；每次 Execute 无状态地只调一次子 `Run`，按顺序只提取 value-form `TextBlock`，Data 为 nil；malformed input、截断、无最终文本是软结果，子 `Run`/ctx 错误以 `%w` 硬传；每个返回的 adapter 以 context-aware 单槽 gate 串行其子 `Run`，同一 child 的多个 adapter 不互锁（调用方负责共享协作者的并发安全）；不做 pool、retry、nested event forwarding（父层只见一个最终 `ToolResult`）；child 配置 Store 时每次 Execute 生成独立持久线程（`Config.RunOptions` 透传 `WithThread("")`，固定非空 ID 破坏无状态承诺），adapter 不暴露子线程 ID。
**背景/问题**：agent-as-tool 是内核外的卫星组合面，不改 Tool interface；不共享 session/thread。
**被否决的备选**：子 session 续跑 / 事件转发 / pool（均列为不做）。
**结论**：父 ctx 取消传给子执行，但不等于父子 durable run 已结算（结算纪律见 P6）。
**出处**：DESIGN §5 P3、§5 P1 父子线程取消结算；`docs/kernel/31-tool.md` §5.2。

### T13 · agenttest：公开测试替身与版本化录制 grammar（P2-3）

**决策**：`agenttest` 是公开、stdlib-only 的测试替身包，只 import `llm`、`tool` 与标准库，绝不反向 import `agent`；提供全局严格有序的 `ScriptedProvider`、`Recorder`/`Replayer`、最小 `ToolFunc`；`Request` 深拷贝并严格比较（函数型 option 的身份不是契约）；录制 bytes 是显式版本化契约：v1/v2 grammar 冻结、Recorder 写 v3、Replayer 读 1/2/3；INTERRUPTED 录制（nil-error chunk 后 early-break、abandoned iterator、读失败）必须以 `ErrInterruptedRecording` 拒绝回放，绝不回放成成功；错误 DTO 保留 `errors.Is/As` 与网络错误分类身份；未知 version/type/字段一律 `ErrIncompatibleRecording` 类型化拒绝。
**背景/问题**：让使用者零成本测试自己的 agent；`agent.MockProvider` 建立在未导出类型上，外部包不可用。
**被否决的备选**：外部包使用 `agent.MockProvider`（AGENTS 指向 agenttest）；pointer-form chunks 归一化为值形式（v1 不做，`Bytes` 返回 `UnsupportedChunkError`）。
**结论**：agenttest 可依赖 `llm/codex/auth` 与 `llm/codex`，内核不得反向依赖。
**出处**：DESIGN §5 P2-3、agenttest 录制 v3 节；AGENTS「WHERE TO LOOK」；`docs/kernel/31-tool.md` §5.3。

## 5. 传输

### L1 · Responses 适配器五裁决（2026-09-11）

**决策**：D1 独立子包 `llm/openairesponses`（`Name` 为 `openai_responses`，协议差异是条目级的，不做双协议混包）；D2 状态策略 A：`store:false` + 手动全量回放 items（历史由调用方持有，与 Compactor/截断/Store 完全正交）；D3 新增条目级 `ReasoningItemBlock{ID, EncryptedContent, Summary}`（type `reasoning_item`，与 CC 协议内嵌 `ReasoningBlock` 语义不同）；D4 内建工具不做（web_search/file_search/code_interpreter/computer/MCP，ToolInfo 只产 function 定义）；D5 Chat+ChatStream 全量、mock SSE 单测 + e2e。
**背景/问题**：Responses 协议与 Chat Completions 是两套条目模型（message/function_call/reasoning items）。
**被否决的备选**：`previous_response_id` 与 Conversations API（B 方案留作未来可选模式）；结构化输出 `text.format`（v1 不支持，见 Backlog）；双协议混包。
**结论**：输入映射——system → 顶层 `instructions`，`tool_use` → `function_call`，`reasoning_item` → `reasoning` item（`encrypted_content` 原样透传、置于其 `function_call` 之前保持声明顺序）；`WithStop` 在 Responses 无对应参数——显式类型化错误，不静默丢弃；非流式 `Chat` 对 `status != completed` 返回错误，不把部分输出当成功。
**出处**：DESIGN §5「OpenAI Responses 协议适配器」。

### L2 · bounded I/O 三限额：10 MB / 1 MiB / 120s

**决策**：响应体与非流式 JSON 上限 10 MB，SSE 单行上限 1 MiB，默认 HTTP 超时 120s；流式路径成功时返回原始 body 惰性消费，只对非 2xx 错误体整读；行上限/单项累计/完成响应上限同样约束 codex。
**背景/问题**：三个数都是"远大于合法最大值、远小于危险区"的量级封顶；默认安全与"用户程序不能挂"之间的诚实取舍——超过 120 秒的单条流被 `Client.Timeout` 截断，需要更长流的宿主用 `WithHTTPClient` 接管时间边界。
**被否决的备选**：不设限、依赖调用方传 deadline（`context.Background()` 完全合法，进程安全不能押在调用方自律上——限额是机制不是策略）；信任 `Content-Length`（服务器可谎报，chunked 无长度可言；字节计数是唯一不依赖对方诚实的度量）；只留 `Client.Timeout` 一道（拦时间不拦内存，反之滴注式流只有超时能拦，二者不可互替）；流式路径整读（摧毁增量交付语义）。
**结论**：同一常量在 codex 里兼任单事件与完成响应上限，避免两套限额打架。
**出处**：`docs/kernel/21-llm-transport.md` §1.3–1.4；DESIGN §5 codex 传输节；AGENTS「NOTES」（bounded I/O）。

### L3 · SSE 子集解析与 codex 终态补齐纪律

**决策**：`ScanSSEEvents` 实现"SSE 规范中与 OpenAI 风格流式 API 相关的子集"：注释行跳过（心跳不是事件）、`data:` 前缀累计、空行分帧、`[DONE]` 终止、`id:`/`event:`/`retry:` 字段忽略；EOF 冲刷部分帧；四个适配器复用 `DoStreamRequest`/`ScanSSEEvents`，非 2xx 用 `llm.APIError`。codex 侧终态补齐：以已累积的 output_index/items 为基础、按 item id 核对终态项，只交付尚未发出的内容，再发唯一 `DoneChunk`；不得重复追加 delta、不得用终态稀疏数组重排已有 item；矛盾内容/身份、索引空洞、非法完整工具参数是协议错误，无 Done，不重试半条流；请求和认证均在首次 range 后才发生，提前 break/取消关闭响应体。
**背景/问题**：多余 SSE 字段在内核里没有消费者；codex 终态 output 可以省略、为空或只重述部分已流式交付的项。
**被否决的备选**：完整 SSE 规范实现（无消费者即不实现）。
**结论**：解析器每条行为都有测试钉住。
**出处**：`docs/kernel/21-llm-transport.md` §2；DESIGN §5 codex 传输、映射与流节。

### L4 · 重试边界：pre-stream only，mid-stream 禁止重试

**决策**：只有 `ChatStream`/`Chat` **直接返回**的可重试错误（429/5xx/网络错误，有上限）走既有 retry；iterator yield 的 mid-stream 错误（含零 chunk）不重试；fallback 链 `ErrStreamingNotSupported` → `Chat` → synthesizeChunks；重试住在循环（`chatWithRetryAndFallback`）不住适配器；codex/OAuth 首次 401 关闭响应后 `Refresh(&used)` 并最多重发一次，其余网络、429/5xx 不在 adapter 重试。
**背景/问题**：mid-stream 重试无法区分"远端未执行"与"已部分执行"——零 chunk 不是远端无副作用证明（RAR 复核维持撤回"没有输出就可以重试"的旧判断）；分层重试必须分开说：传输/请求重试、模型尝试恢复、任务修复是三种重做单位，多层可存在但必须各自标明单位、预算、取消与可观测信息（RAR §3.2）。
**被否决的备选**：重试 mid-stream SSE 错误（反模式明令）；适配器各自内置重试；照搬 Codex/OpenCode 的有状态服务级重试协议。
**结论**：惰性迭代内错误不触发现有 agent 的外层 pre-stream RetryEvent，不承诺自动恢复。
**出处**：AGENTS「ANTI-PATTERNS」；agent-AGENTS「ANTI-PATTERNS」；`docs/kernel/10-agent-loop.md` §5.2–5.4；RAR §3.2、§6；DESIGN §5 codex 错误节。

### L5 · APIError 叶子错误 + NonRetryable 逃生阀

**决策**：`llm.APIError` 是叶子（无 Unwrap），`Retryable` 先检查 `NonRetryable` 标记再按 429/5xx 判断；`codex.Error{Kind,Code,RetryAt,Cause}` 保留错误链（Kind 区分 auth/quota/capability/protocol；quota 用真实 HTTP 状态且 NonRetryable=true，未知 429 仍可重试）；`auth.Error` 保留脱敏原因与 `errors.Is(ErrLoginRequired)` 身份；错误 DTO 在 agenttest 录制 v3 可忠实编码（最多三层、循环/过深使 `Recorder.Bytes` 明确失败）。
**背景/问题**：状态码判定是策略，个别业务错误需要显式不可重试标记；`NonRetryable` 作为公开字段一次定型（新增公开字段会影响外部 unkeyed literal）。
**被否决的备选**：APIError 包装下层错误（破坏叶子身份与 `errors.As` 消费链）；OAuth 伪造推理 APIError。
**结论**：跨包消费链以 `errors.As(*llm.APIError)` 为准（leaf，无 Unwrap）。
**出处**：DESIGN §5 codex 错误节、agenttest 录制 v3 节；`docs/kernel/20-llm-model.md` §4。

### L6 · 鉴权住适配器：bearer / JWT / OAuth 三模式

**决策**：bearer 无状态留传输层；glm JWT 自签 + 嗅探 + 缓存（API key 含 `.` 即选 JWT 模式——含点 bearer key 会误认证，已知陷阱如实文档化）；codex OAuth 四件套：`auth.Source` 的 Token/Refresh 职责分离（Token 只复制最近发布快照、不联网不保存；Refresh 独占到期判断、401 恢复、single-flight 与持久化）、刷新锁不跨 I/O、新凭据先 Persist 后发布（保存失败保留 pending State，后续只重试保存，Token 仍返回旧快照）、已发送刷新但结果不确定时要求重新登录（不能盲目复用可能已轮换的 refresh token）；固定 PKCE profile（issuer/client_id/redirect/scope，2026-09-22 owner 依 invalid_client 实测纠正并与 Codex CLI 二进制交叉核对）；推理必须发送 `chatgpt-account-id`（来自 access-token JWT claim）；默认独立文件 `~/.go-agent/codex/auth.json`（目录 0700、文件 0600、临时文件 sync/rename 后同步目录）。
**背景/问题**：三种鉴权模式的生命周期差异大于共性，共享层会造出最坏交集（21 篇 §5.5）。
**被否决的备选**：共享鉴权层；写入官方 auth.json（只允许显式 access-only 读取，永不使用其 refresh token）；自动发现凭据或切换计费方式（API-key 模式使用 openairesponses）；无头模式自动换端口或 device flow（固定 ssh -L 1455 端口转发）。
**结论**：FileSource 用 O_EXCL 占用锁，记录随机 owner 与 PID，Close 校验后释放，不按时间抢遗留锁；同进程共享一个 Source，不承诺跨进程 broker。
**出处**：`docs/kernel/21-llm-transport.md` §5；DESIGN §5「Codex 订阅适配器」认证节；AGENTS「NOTES」（GLM auth sniffing）。

### L7 · codex 卫星拆分：交互登录与内核协议分离

**决策**：交互登录、文件读写、默认路径与占用锁在 `codexauth` 卫星；`llm/codex/auth` 只实现非交互协议与注入式持久化；仅使用 ChatGPT 订阅 OAuth，内核只依赖标准库；自动浏览器启动仅在 main；先绑定 loopback 再展示 URL。
**背景/问题**：登录是入口程序职责；库代码不读 env、无 init()。
**被否决的备选**：device flow；跨进程 broker；发现 profile 漂移自行改参数或冒充官方客户端（明确停止报告）。
**结论**：`WithAuthSource` 必填；`WithBaseURL` 校验无 userinfo/query/fragment，默认 HTTPS，允许显式 loopback HTTP；自定义 client 也通过副本禁止 redirect；`originator` 默认 go_agent，`codex_cli_rs` 仅显式 opt-in，不根据 403 自动切换。
**出处**：DESIGN §5「Codex 订阅适配器」认证与配置节。

## 6. 流程与质量

### Q1 · docs-first：契约正文先行

**决策**：`docs/DESIGN.md` 是项目设计契约（定位、目标、原则、路线图），改接口先改这里；双语镜像 README 由 `scripts/check-readme-contract.sh` 守卫。
**背景/问题**：接口即产品的库形态下，口头契约不可审计；Store v1 等大裁决均契约先行（DESIGN.md P1 修订先落地再写代码）。
**被否决的备选**：未记录。
**结论**："已有专项文档一律不修改不删除；重写某主题时新建文档覆盖，旧文件原样保留"是文档层配套纪律（项目工作约定，2026-10 起生效）。
**出处**：DESIGN 文档头；AGENTS「COMMANDS」「CONVENTIONS」。

### Q2 · 决策流程方法论：先调研、独立评审、反例区分度

**决策**：先调研再设计（业界早已付过学费的形状不要自研兜底——D4 形状教训，见 C6）；评审要独立且带代码证据（blocker 必须是源级反例，不是风格意见）；回归测试要写"反例区分度"（ABA 用例的默认值必须与目标值不同，否则旧 bug 下假通过；冲突注入器不得顺延 expected，否则绕过被测路径——"一个在 bug 存在时也能通过的回归测试不是回归测试"）；修复写入必须逐步自校验落盘（曾两次 eval 写入静默失败导致评审者读到旧代码、浪费两轮复审）。
**背景/问题**：控制面两批实现各经历 2–4 轮评审拉锯，四个正确性 blocker（D6 head 缓存、D10 内容 ID、checkpoint 重定基、度量不一致）是自查与测试都没抓到的。
**被否决的备选**：第一版直接设计自研探针兜底（被"先调研"替代）。
**结论**：方法论写进 CDR 供未来修订追溯。
**出处**：CDR §7。

### Q3 · TC-first：宣告先于执行（2026-09-10 裁决）

**决策**：同轮的全部 `ToolCallEvent` 连续发出后才逐个执行；`ToolCallEvent` 语义是"模型请求的宣告"而非"已执行"；交付同步——消费者未确认宣告会推迟对应执行，宣告批内提前断开则本轮不执行任何工具；宣告批次无 ctx 检查（拆分会留下半宣告的 assistant）。
**背景/问题**：跨轮因此可分辨；取消或首个硬失败时，已宣告调用随 assistant 消息完整可重建。
**被否决的备选**：未记录（裁决当年定型，无对立提案存档）。
**结论**：maxIter terminal skip 轮是唯一已知结局的例外：先整体 durable close 再同步 TC → skipped TR → Done（见 Q4）。
**出处**：DESIGN §7；agent-AGENTS「CONVENTIONS」；`docs/kernel/10-agent-loop.md` §3.2–3.3。

### Q4 · maxIter terminal skip（C10）：确定未执行，零审批零执行

**决策**：maxIter 终局轮每个请求的调用都被宣告后给出确定性 skip `ToolResultEvent`（IsError、"iteration limit"），零执行、零审批；Store 先整体 durable close（或构造完整 terminal history）再交付；该分支零决策零执行保持冻结——不 drain、不咨询 FinishRound；consumer 在 TC 或 TR break 只停止该 consumer 的 delivery，thread 已 closed。
**背景/问题**：熔断时模型可见历史仍须 TC/TR 配对完整；Store 与非 Store 的 Done History 相同且可 paired continuation。
**被否决的备选**：未记录。
**结论**：实现为 `agent/max_iter.go`（`iterationLimitResults`）与截断分支；折叠契约由 `agent_fold_test.go` 钉住。
**出处**：DESIGN §5 P0、§8；agent-AGENTS「CONVENTIONS」；`docs/kernel/10-agent-loop.md` §4。

### Q5 · DoneEvent.History 是权威快照；ReasoningItemChunk 不发事件

**决策**：`DoneEvent.History`/`RunResult.History` 是完整无损的权威 history snapshot；`ReasoningItemChunk` 不映射为 AgentEvent（consumer 不得仅靠增量 events 重建含 reasoning items 的 assistant，必须在 Done 时以该 snapshot reconciliation）；普通 Chat-only history 可作 event-only 诊断折叠，但不是通用完整-history 契约；流以无 Done 结束 = 错误；未知 AgentEvent 中止折叠。
**背景/问题**：Responses 的加密 reasoning items 无法在事件流中保序保真表达。
**被否决的备选**：宣称 raw event stream 独立编码所有 assistant blocks；为 ReasoningItemChunk 增加事件（反模式明令）。
**结论**：折叠契约：initial input + fold(events) = DoneEvent.History。
**出处**：DESIGN §5 P0、§7；agent-AGENTS「CONVENTIONS」「ANTI-PATTERNS」；`docs/kernel/10-agent-loop.md` §3.4。

### Q6 · 压缩链与截断规则

**决策**：压缩默认窗口 80% 触发，链式策略依序执行（折叠老工具组 → 滑窗 → 可选 AI 摘要）；压缩单位是"消息组"（工具调用与其结果不可拆分、system 不可动）；恒保护 system/首末 user/最近组；`Compactor` 接口可手动对任意历史执行；工具结果截断：单结果 ≤ max(128, 窗口×30%) rune，50/50 掐头留尾带省略标记，入库前执行（内存视图与 Store 同源），截断记 Warn；软错误同样受限长；已知限制如实声明：保护组自身超预算报 `ErrCompactionBudgetExceeded`，rune 估算为启发式非保证。
**背景/问题**：截断教训——只掐头不留尾是 bug 来源（Hermes 案例）；上游 API 不会静默截断（Claude 超限直接报错），限幅必须发生在本层；压缩参照 Microsoft Agent Framework 策略组合；压缩失败非致命（Warn 后以原历史继续）。
**被否决的备选**：只掐头不留尾；静默丢弃超长（超长原文不保留但记 Warn——稳定目标的代价）。
**结论**：压缩/截断/checkpoint/`CompactionEvent` 只处理 canonical history（与 Q8 的瞬态 overlay 边界互锚）。
**出处**：DESIGN §2 目标 5、§6、§7；`docs/kernel/13-agent-context.md` §2–3、`docs/kernel/11-agent-tools.md` §3.4。

### Q7 · AI 摘要跟随线程生效 provider 与 options

**决策**：摘要压缩跟随当轮生效 provider 与线程生效 options：`Compactor.Compact(ctx, history, budget, provider llm.Provider, opts []llm.Option)` 调用面携带 provider 与 opts，循环内由内核自线程配置投影解析注入（线程热切换 provider 后摘要用新 provider）；`NewSummarizationCompactor()` 不再收 provider 参数；手动调用者显式传参，nil provider 走既有类型化错误并按非致命路径降级。
**背景/问题**：线程热切换 provider 后，若摘要仍调用切换前的 provider，压缩会在切换后静默使用已失效的厂商与凭据。
**被否决的备选**：Compactor 构造期绑定 provider（热切换下绑定即失效）。
**结论**：与 C16 的配置投影机制同源（provider+model 成对切换后，摘要跟随新厂商）。
**出处**：DESIGN §5 P1 历史预算管理；`docs/kernel/13-agent-context.md` §2.5。

### Q8 · round context 瞬态 overlay 与中断错误包装

**决策**：`WithRoundContextProvider(RoundContextProvider)` 在每次直接 `Provider.ChatStream`/`Chat` 调用前产生运行环境快照，非空快照以固定来源标识包装为本次 outbound 末尾的 role=user **瞬态 overlay**：不进 canonical history、`DoneEvent.History`、AgentEvent、Store 日志或 checkpoint；canonical history 的 system 前缀是 kernel grammar/checkpoint 冻结契约（中途 system 无统一语义——Responses 又会把 system 提升为单个 instructions），trailing user 是所有适配器的共同 carrier；每次 callback 返回完整有效快照，内核不按内容去重，空字符串 = 该次无 overlay；预算只承诺 `estimateRunes` 消息 rune 启发式（不含 tool schema、wire 映射、reserved output 或真实 tokenizer；overlayCap = 窗口 20%），超限在 callback 前以 `ErrCompactionBudgetExceeded` 失败，绝不发送破碎 overlay；持久 session 确认 `run_started` 后，最外层将所有非 nil 交付错误包成可 Unwrap 的 `RunInterruptedError{ThreadID, Err, Settlement}`（嵌套 child 的第一个 `errors.As` 始终是可 Resume 的 parent）；`WithRunExitFn` 在 iterator unwind 后、ownership 释放前同步交付本 session 的 `SettlementToken`（覆盖 early-break，不造错误或事件）。
**背景/问题**：宿主需要注入运行环境（时钟、目录、任务事实）而不污染持久历史；匿名 `Run` 的中断也要总能 `ResumeThread`。
**被否决的备选**：动态改 system 前缀；依据 ThreadID 自动认领当前 run 结算（明确禁止——没有原 token 时只能做新的显式恢复检查）；`RunInterruptedError` 自动触发 `SettleThread`（它不是取消终态）；release 后回填 token；无 session/未 range 也回调。
**结论**：Resume 先重放 canonical history 再执行 live callback，不重放、累积或双计旧快照。
**出处**：DESIGN §5 P1「逐轮运行环境上下文」；`docs/kernel/13-agent-context.md` §4。

## 7. 待沉淀决策

代码中存在、但尚无书面决策记录的可疑点。逐条标注证据形态；修复方向若已有注记则一并引用。这些不是已裁决决策，收录于此供未来补记。

### S1 · SchemaV3 读侧拒绝缺口 —— 已修复（90759e6，2026-10-29）

**现象（修复前）**：写侧已接受 SchemaV3（`store/store.go:225` `validateRecord` 上限 `SchemaV3`；codec-3 checkpoint 自控制面轮起由 agent 以 SchemaV3 写入），但 JSONL 读侧 `load` 仍以 `r.Schema > SchemaV2` 判不支持（`store/jsonl_file.go:44`）；`store/jsonl.go:33` 的契约注释甚至仍写 SchemaV1（两次版本提升都只改了写侧，未同步读侧与注释）。实测后果：SchemaV3 checkpoint 可成功落盘、同进程内可读，`Close` 后重开目录即 `ErrUnsupportedSchema`；store 包自身测试未覆盖该组合。
**处置**：用户裁决「这不就是个 bug 吗？那修」。commit 90759e6：读侧上限对齐 SchemaV3，jsonl.go 契约注释同步修订，新增 schema_v3_test.go 钉死两个契约（V3 checkpoint 双后端落盘重开可读回；伪造 SchemaV4 记录读侧仍类型化拒绝）。
**后见之明**：根因是版本提升时写、读、注释三层需人工同步；store 包没有 schema-N 回环回归测试兜底。教训与 40-invariants.md §8 元规则一致——写读两侧共享常量并以回环测试钉死，优于人工同步。
**归属**：store 包；属正确性缺口而非语义分歧。

### S2 · P2-5 四个开放子决策的最终取值无单独裁决记录 —— 推断（源码考古）

**现象**：`docs/tool-registry-cache-design.md` §11 列出四个开放决策点各带推荐——D-1（序列化字节存不存）、D-2（`List` 深拷贝 vs 浅共享）、D-3（marshal 入口走公共 `json.Marshal` 还是包内直调 `marshalParameter`）、D-4（惰性重建用单锁 double-check 还是 `atomic.Pointer`）。落地文本 DESIGN §5 P2-5 只明确体现 D-1=(a) 存字节与 D-2=深拷贝；D-3 与 D-4 的取值只能从实现反推：`tool/registry.go` 的 `freezeForRegistration` 在锁外调用 `json.Marshal(frozen.Parameters)`（公共路径，D-3 推荐），`List` 用单 RWMutex double-check 惰性重建（D-4 推荐）。
**现状**：实现与推荐一致，无分歧嫌疑；缺的只是书面裁决记录——CACHE 文档头仍标"待 owner 裁决 §11 开放决策点"，与 P2-5 的"已裁决"状态不同步。
**归属**：可在 DESIGN §5 P2-5 或 CACHE 文档补一行裁决注记，消除两文档状态差。

---

## 8. 索引统计

本文收录决策 61 条：架构边界 7（A1–A7）、持久化 9（P1–P9）、控制面 17（C1–C17）、工具与 schema 13（T1–T13）、传输 7（L1–L7）、流程与质量 8（Q1–Q8）；另有待沉淀条目：S1 已于 2026-10-29 修复（90759e6，见其处置记录）；S2 仍待沉淀（CACHE 文档头与 DESIGN P2-5 状态不同步）。被否决备选中，整条标注"未记录"的 6 条（A2、A6、T5、Q1、Q3、Q4——对立提案无存档）；A3、P3、P4、P8 的备选部分有据（引 pi 对照形态或业界参照）、部分未记录；其余条目的被否决备选均有出处，或其备选即 negative decisions 清单本身（如 A1、C17）。

