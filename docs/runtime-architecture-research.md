# 最小 Agent Runtime 架构调研：真实应用复核

日期：2026-10-23。版本：v2，取代上一轮以 SDK 为主的判断。状态：取证、关键源码交叉核查与独立推理复核完成。

## 1. 本轮问题与证据标准

问题是：在保留可靠执行承诺的前提下，go-agent 应当怎样保持最小内核？本轮不再从 SDK 的包名或接口清单推导答案，而是沿真实编码助手的任务链读源码：输入接收 → 模型请求 → 工具及副作用 → 停止/改向 → 重启恢复，再核对上下文与重试职责。

事实标为“源码”；建议标为“判断”。没有运行外部项目、调用真实模型、注入进程崩溃或测量性能。以下不是项目优劣排行榜，也不是全仓审计。

### 样本与固定修订

| 项目 | 固定修订 | 选择原因及限制 |
|---|---|---|
| OpenAI Codex | d4bdafd378a8e328a67a4cd3557b5fe634af378c | 终端/app-server 与核心执行协作；所查主要为 Rust 实现 |
| OpenCode | 02671e66454260d77fe04d63cf5eb94e76921dee | 服务端会话、持久输入、重启恢复和快照；默认分支为 dev，不是 main |
| Pi coding-agent | 36be9c612e64082a4f0d4d0a94706c38ecbc20a1 | 真正编码应用对最小 agent-core 的接入；不是上一轮独立 durable 包 |
| Aider | 5dc9490bb35f9729ef2c95d00a19ccd30c26339c | 回复后应用编辑、Git 撤销与错误反馈的对照样本；不是等价的通用异步工具 agent |

修订来自官方 GitHub API 的对应分支提交查询，截止条件为 2026-10-23；记录的是实际检查版本，不宣称最新发布版。Aider 该提交时间为 2026-05-22，其余三项为 2026-10-23。

## 2. 四条实际任务链

### 2.1 Codex：输入路由与采样循环是不同的状态问题

**源码链路**：TUI 的 turn/start、turn/interrupt 请求进入 app-server；核心 CodexThread 按开始、插话、仅空闲开始等模式路由到提交队列。run_turn 处理输入和采样前压缩，run_sampling_request 调流消费与工具运行。工具 orchestrator 在执行前决定审批和沙箱策略。停止走取消与任务收敛，恢复则从 rollout 重建历史。[C1][C2][C3][C4][C7][C8]

**决定性细节**：start_or_steer_turn 返回已开始、已插话或未提交，而非把“收到请求”当作无条件接受。worker 交接路径先 flush、再取消、等待任务退出，最后关闭 writer 后才确认交接；待处理输入仍可能是进程内状态，源码明确指出持久重放需要额外协议。[C1][C5]

**重试与上下文**：断流处理有自己的重试/重连预算与通知；采样重试从当前历史重新构造 prompt，而不是简单回放相同的半截字节流。流处理中已完成的项与未确认的部分项分开处理。压缩还涉及输入保存顺序与当前历史版本，不能只看是否调用了摘要模型。[C2][C6]

**边界**：rollout 恢复、运行任务退出与工作区文件回滚不是同一操作。所查历史重建和线程回退路径不足以证明文件或远端副作用被撤销；不作全仓“没有文件恢复功能”的断言。

**判断**：值得借鉴的是输入模式、运行身份、交接确认与请求尝试的语义边界。Codex 将不少控制职责放在自己的 core 中，并不证明可嵌入 Go 内核也应照搬它的部署、沙箱与服务端组织。

### 2.2 OpenCode：持久收件箱是明确的产品选择，不是循环的天然组成

**源码链路**：服务端 session.prompt → Session.prompt 准备输入、按消息 ID 协调重复提交、admit 收件箱后唤醒执行 → drain 清理残留状态、在步边界投递输入 → 单步模型流与工具 fiber → 权限判断、快照、事件保存。[O1][O2][O3]

**停止与重启**：单步收尾等待/中断工具；重启服务检查遗留执行声明，添加合成继续输入，同时修正残留工具/压缩状态。它明确声明恢复为 at-least-once，不保证外部副作用不重复。恢复扫描还依赖“旧 owner 已死”的宿主协议；不是任何进程看到一条 claim 就能接管。[O2][O4]

**重试**：同请求重试受 outputStarted 门控；该标记包含文字、推理和工具输入/调用事件，不只是终端已显示多少字。已有输出后的部分失败可以添加合成继续输入，而不是透明重放原流。原请求重试与模型继续，是两种行为。[O3][O5]

**压缩与撤销**：预算结合最近 provider 实测与后续消息估算，初始估算包含 system、工具定义和消息。revert 分为文件快照恢复与会话边界提交；二者依旧不是撤销任意 shell 或远端效果。[O6][O7]

**判断**：只有产品承诺排队输入跨重启不丢时，持久 inbox 才是明确需求。不要把服务端 Bus、数据库投影、作业系统和自动恢复扫描全部装入最小内核。

### 2.3 Pi 编码助手：最小循环能成立，是因为应用会话层承担了真实工作

**源码链路**：交互输入交给 AgentSession.prompt；sdk.ts 将会话历史与模型调用装入低层 Agent；agent-loop 采样并执行工具，应用层订阅完成消息后写 SessionManager。重启时打开会话、重建模型上下文，再创建运行时。[P1][P2][P3][P4]

**控制与恢复**：AgentSession 管理 steer/followUp、压缩、重试和切换会话。abort 不止发送 signal，还取消摘要/重试并等待 idle；bash 实现负责终止子进程并等待退出。模型历史恢复不能替代进程清理。[P2][P5]

**重要反例**：该版本的 Agent 会按顺序等待事件 listener 的 Promise，idle 也要等 listener 结束。不能把它描述成不等待的通知。应用的 message_end 写入可参与执行次序，但所查 JSONL 写入方法没有显式 fsync；这不是与 go-agent 的确认持久性等价的证据。[P4][P6]

**重试与上下文**：provider 请求重试设置与 AgentSession 的运行级恢复并存；失败回答可保留在原始记录、从发给模型的投影中排除，再继续运行。压缩也是有取消、失败、重试和历史边界的操作，而非纯展示动作。[P1][P2]

**判断**：会话控制职责有真实应用证据，但不等于必须新增公共 Go 包。Pi 的等待型接缝说明，存储可以通过显式执行协议放在循环外；这不证明 go-agent 当前事件接口足以替代 Store。后者的运行身份、回放与 SettlementToken 都与内核持久协议绑定，写失败后仅 early-break 也不能完成该协议，不能靠“订阅并 flush”直接替换。

### 2.4 Aider：改变副作用发生时机，本身就能减少恢复问题

**源码链路**：CLI → Coder.run/run_one → send_message/send 完成模型回复 → 解析与校验编辑、确认新文件/越界编辑 → editblock_coder 写文件 → 可选自动提交、lint/test 反馈再进入下一轮。[A1][A2]

**取消与重试**：该路径的流回复被 KeyboardInterrupt 打断时，会在 apply_updates 之前返回；但这不证明文件写入中途也能原子取消。重试包围 send 调用，不能据此声称所有真实 agent 都遵循“输出后绝不重试”。[A1]

**恢复与撤销**：restore_chat_history 读取 Markdown 历史，不是恢复持久任务调度器。undo 针对本会话记录的最近 Git 提交，并检查脏文件、已推送等条件；它不是运行时对任意副作用的撤销。[A1][A3][A4]

**上下文**：应用自己构造文件、仓库信息与消息上下文，ChatSummary 按模型计数和预算摘要。[A1][A5]

**判断**：这是解释权衡的对照，而不是把 Aider 的简单循环移植成 go-agent 的理由。go-agent 必须支持任意工具，不能通过假设所有编辑都发生在完整回复之后来简化自己的执行契约。

## 3. 跨项目结论：什么应当小，什么不能假装不存在

### 3.1 最小执行内核，不等于整个应用只有一个循环

共同出现的是几种逻辑责任，而非相同包结构：

- 执行：模型调用、工具批次、结果配对、执行退出及提交顺序。
- 会话控制：输入受理、当前任务身份、停止等待、插话/排队、恢复决策和失败展示。
- 产品及环境：文件权限与快照、shell 清理、终端/RPC、部署接管、模型配置。

Codex 有部分职责位于 core，Pi 位于 AgentSession，OpenCode 分为会话和执行服务。证据支持显式责任边界，不支持“统一拆成三个 module”或“一律把控制器放到循环外”的定理。

go-agent 已有 agent 与 harness/internal/app.Controller；后者的 Submit、Stop、Continue、Abandon、streamWorker 就是控制职责。建议是校准并验证已有边界，不是再创造一个名字相近的新层。当前 Controller 仍是 harness 内部实现，是否提取公共库要另有真实复用需求。

### 3.2 三种重试，必须分开说

| 范围 | 重做什么 | 对外语义 |
|---|---|---|
| 传输/请求重试 | 重发某次请求或更换连接 | 需 Provider 事实与风险判断；可能仍有远端工作/计费 |
| 模型尝试恢复 | 带已完成历史续写，或替换失败尝试 | 会改变模型输入、展示与历史投影，不能伪装成透明网络重试 |
| 任务修复 | 根据格式错误、lint/test 或工具结果再执行一轮 | 需要任务语义、预算和新副作用决策 |

“只能有一层重试”太绝对。多层可以存在，但必须标明各自重做的单位、预算、取消与可观测信息；否则会相乘或混淆。go-agent 已区分 RoundContextRequest.Attempt 与 RetryInfo.Attempt，不应把已有机制再列为待建；需要补充的是分层清单及未来新恢复范围的契约。不能因为真实应用支持续跑，就放宽 go-agent 的 iterator 错误重放规则。零 chunk 仍不证明远端未执行。

### 3.3 保存历史、接管运行、撤销文件，是三份承诺

本项目对未提交工具结果补 outcome-unknown，而不是盲目再执行；OpenCode 则明确承认 at-least-once 的外部重复风险。Pi 的消息重建和 Aider 的 Markdown 恢复又是更窄的能力。应按承诺比较，不能把它们都叫“resume”就判成等价。

同样，取消成功不等于撤销文件；恢复旧上下文不等于恢复磁盘；有快照不等于远端效果能回滚。最小内核应明确未知，而不是为了接口简单返回假成功。

### 3.4 上下文预算不再只是一个缺乏需求的假设

真实应用把 system、工具定义、文件内容、未测量工具结果、输出余量和 provider 实测放在请求准备里考虑；OpenCode 甚至给估算遗漏写了明确 TODO。证据足以支持研究可校准的请求预算接缝，但不能据此宣布 rune 合同违规或立即引入 tokenizer/价格目录。

本项目 docs/DESIGN.md:179 已诚实声明当前预算只含消息 rune，agent/round_context.go 也是该范围。需要讨论的是下一阶段是否扩大承诺、谁提供估算，而非把当前实现重新命名成“精确 token 预算”。已有 Usage 是可研究的校准信号；是否足以覆盖变更后的 system、schema 与新增消息仍需验证，不在此承诺准确性。

## 4. 对本项目的建议及代价

### 保留

- Run 与 RunStream 共用执行路径；不另造一套宿主专用循环。
- 现有可选 Store、声明先于执行、结果未知恢复、原 token 结算和线程所有权。
- Compactor 接口与可用默认策略；审批机制留窄接口，具体权限和沙箱不进入通用循环。

这些是本地契约的选择，不是因为四个应用都这样做。若未来确要外置可靠性机制，也须有等价的确认点、错误传播与兼容性设计，不能仅靠移动文件。

### 优先研究与验证

1. **已有控制层的受理与终态边界。** Controller.Submit 先存 metadata 后启动 goroutine；RunThreadStream 的输入提交在首次消费才发生。因此 Submit 返回、任务被持久接受、模型已开始并不是同一个确认点。这不是据此认定契约违规；如果要承诺“已接受输入跨崩溃不丢”，需要另行定义，而不是默认已有 durable inbox。停止要继续区分请求取消、工作退出、结算成功和可以接收下一任务。
2. **失败阶段与恢复单位。** 先解决 Provider 的真实请求失败落在迭代器内、内核既有重试只看直接返回错误的接缝。若以后支持部分输出恢复，应先说明哪些内容已提交、失败尝试如何展示、下一轮是重发还是续写，以及工具结果如何保留。不要直接照搬 Codex/OpenCode 的有状态服务协议。
3. **请求预算口径。** 用实际模型请求案例核对 system、schema、工具结果与输出预留，评估估算接缝的必要性。无需把厂商知识库放进内核；也不先锁定公开接口形状。

代价：第 2、3 项可能改变冻结契约，须先修改 DESIGN.md 并独立设计；第 1 项主要是宿主控制协议，不能仅改终端文案。公开控制器、durable inbox、后台压缩、自动重启接管和插话队列都不是本次自动获准的开发事项。

### 暂不做

不新增通用 hook 总线、工作流引擎或自动恢复守护进程；不为了模仿 Pi 拆 package；不把实际应用的所有策略塞回 agent.New 的选项。持久输入、跨进程接管、部分输出续写只有在产品承诺明确后才分别设计。

## 5. 应先验证的边界，而非先增加的抽象

| 场景 | 需要确认的本地行为 |
|---|---|
| 已通知宿主但输入尚未提交就退出 | 接收确认与 durable 接受的区别，不凭 metadata 猜任务正文已存 |
| 429/连接错误在惰性迭代器内出现 | 真实 Provider 路径的错误分类、重试次数与尝试信息 |
| 已显示文本后断流 | 失败尝试与已提交历史不混淆；不假装 Done，不透明拼接重复内容 |
| 工具已改文件但结果尚未提交时崩溃 | 恢复报告未知，不凭缺少结果盲目重执行；磁盘恢复另算 |
| 审批中、执行中、自然完成时停止 | 不同窗口下的 join、原 token、结算和后继任务身份 |
| 压缩中取消或重启 | 摘要结果与历史版本对应；失败不丢新输入，不安装过期结果 |

这些是后续设计和验证清单，不代表本轮已经做过故障注入，也不代表每一行都需要新增 API 或已有 bug。

## 6. 相比上一轮，哪些判断改变了

| 上一轮判断 | 本轮结论 |
|---|---|
| 主要看 SDK 就能决定架构优化方向 | 撤回；SDK 降为补充，主证据必须是应用完整任务链 |
| 只需消除 Provider 重试接缝 | 扩大为控制层确认点、恢复单位和请求预算三条轴；不能只修一个循环就声称架构完善 |
| 尽量只允许一层重试 | 修正为不同范围可有不同恢复策略，预算和重放单位必须清楚 |
| 控制器需求要等第二个消费者再说 | 会话控制职责现在就必须清楚，且本地已部分实现；新增公共包仍需复用证据 |
| 预算接口缺少需求证据 | 有实际应用需求信号，值得设计评估；仍不足以直接决定 API 或引入 tokenizer |
| 存储放循环外就难以可靠介入 | 收窄；Pi 说明等待型协议可以参与执行次序，但 go-agent 当前观察接口不能直接替代内核持久协议 |
| 没有输出就可以重试 | 仍撤回；零 chunk 不是远端无副作用证明 |

## 7. 复核与证据限制

两条项目调查已完成，主调研者直接交叉检查了 Codex 的输入/重试/交接/历史重建、OpenCode 的恢复/输出门控/压缩/撤销，以及 Pi/Aider 的应用链。

一条独立只读评审审查了本地契约与主结论推理，未发现根本阻断，要求收紧三点，均已采纳：不能把 Pi 的 listener 协议当作本项目可直接外置 Store 的证据；Attempt 的区别已有实现，不能重复立项；Usage 校准只作研究方向，不作精度承诺。该评审没有重新独立抓取外部源码，也没有核查 harness 测试覆盖；外部事实的交叉核查由主调研者完成。因此不能把本报告描述为四项目的独立运行验收或未来方案已获批准。

未验证：外部程序的真实 API、终端交互、平台沙箱、数据库故障、进程强杀与性能；Codex 远端压缩协议；OpenCode 未读传输实现与所有权限应答路径；Pi 写入的完整掉电行为；Aider 全部编辑格式。没有以局部未见能力断言全仓不支持，也没有声称任何样本实现了外部副作用 exactly-once。

本地只修订此研究报告，不修改实现或 DESIGN.md；纯文档不新增钉住措辞的测试，也不重跑此前已通过的构建来充当外部研究证据。

## 8. 固定源码台账

下列链接均指向上表固定修订。范围是阅读和判断所依赖的关键节点，不是全文件每行都经过运行验证。

- [C1] codex-rs/core/src/codex_thread.rs（417-451 输入路由；474-519 恢复）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/codex_thread.rs>
- [C2] codex-rs/core/src/session/turn.rs（173-242 输入/压缩；1729-1869 重试循环；2654-3120 流与工具）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/session/turn.rs>
- [C3] codex-rs/tui/src/app_server_session.rs（1469-1530 提交/打断）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/tui/src/app_server_session.rs>
- [C4] codex-rs/core/src/tools/orchestrator.rs（125-345 审批和沙箱选择）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/tools/orchestrator.rs>
- [C5] codex-rs/core/src/session/turn_suspension.rs（13-111 flush、取消、交接）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/session/turn_suspension.rs>
- [C6] codex-rs/core/src/responses_retry.rs（57-176 重试/重连预算）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/responses_retry.rs>
- [C7] codex-rs/core/src/session/rollout_reconstruction.rs（208-300 历史重建）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/session/rollout_reconstruction.rs>
- [C8] codex-rs/core/src/thread_manager.rs（1345-1371 从历史恢复线程）：<https://github.com/openai/codex/blob/d4bdafd378a8e328a67a4cd3557b5fe634af378c/codex-rs/core/src/thread_manager.rs>
- [O1] packages/core/src/session/session.ts（155-185 准备、受理、唤醒）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/session.ts>
- [O2] packages/core/src/session/runner/llm.ts（56-193 drain；283-291 续写；334-387 残留状态）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/runner/llm.ts>
- [O3] packages/core/src/session/runner/step.ts（73-260 采样与工具收敛）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/runner/step.ts>
- [O4] packages/core/src/session/execution/restart.ts（46-138 恢复及 owner 前提）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/execution/restart.ts>
- [O5] packages/core/src/session/runner/publish-llm-event.ts（436-505 outputStarted 含推理和工具）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/runner/publish-llm-event.ts>
- [O6] packages/core/src/session/compaction.ts（272-339 压缩；1067-1095 估算）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/compaction.ts>
- [O7] packages/core/src/session/revert.ts（22-81 文件与会话边界）：<https://github.com/anomalyco/opencode/blob/02671e66454260d77fe04d63cf5eb94e76921dee/packages/core/src/session/revert.ts>
- [P1] packages/coding-agent/src/core/sdk.ts（191-210 历史恢复；403-438 装配）：<https://github.com/earendil-works/pi/blob/36be9c612e64082a4f0d4d0a94706c38ecbc20a1/packages/coding-agent/src/core/sdk.ts>
- [P2] packages/coding-agent/src/core/agent-session.ts（1103-1198 保存；1873-2016 控制；2092-2175 输入；2498-2514 停止；3769-3861 重试）：<https://github.com/earendil-works/pi/blob/36be9c612e64082a4f0d4d0a94706c38ecbc20a1/packages/coding-agent/src/core/agent-session.ts>
- [P3] packages/coding-agent/src/modes/interactive/interactive-mode.ts（3321-3339 输入与排队）：<https://github.com/earendil-works/pi/blob/36be9c612e64082a4f0d4d0a94706c38ecbc20a1/packages/coding-agent/src/modes/interactive/interactive-mode.ts>
- [P4] packages/coding-agent/src/core/session-manager.ts（576-582 上下文；1172-1213 写入）：<https://github.com/earendil-works/pi/blob/36be9c612e64082a4f0d4d0a94706c38ecbc20a1/packages/coding-agent/src/core/session-manager.ts>
- [P5] packages/coding-agent/src/core/tools/bash.ts（99-163 进程取消和退出）：<https://github.com/earendil-works/pi/blob/36be9c612e64082a4f0d4d0a94706c38ecbc20a1/packages/coding-agent/src/core/tools/bash.ts>
- [P6] packages/agent/src/agent.ts（267-278 listener 语义；617-620 等待）：<https://github.com/earendil-works/pi/blob/36be9c612e64082a4f0d4d0a94706c38ecbc20a1/packages/agent/src/agent.ts>
- [A1] aider/coders/base_coder.py（519-523 恢复；876-944 输入；1419-1623 重试/编辑；2191-2238 确认）：<https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/coders/base_coder.py>
- [A2] aider/coders/editblock_coder.py（38-86 编辑与写入）：<https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/coders/editblock_coder.py>
- [A3] aider/commands.py（553-638 撤销前检查）：<https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/commands.py>
- [A4] aider/io.py（1117-1136 历史写入）：<https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/io.py>
- [A5] aider/history.py（7-96 计数与摘要）：<https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/history.py>

SDK 补充而非主证据：OpenAI Agents SDK 的 replay-safety 规则，以及 Pydantic AI 对存储/持久执行的区别，仅用于术语校验。其文档不是上述应用行为的证据。
