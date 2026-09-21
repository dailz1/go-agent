# Harness M1 设计契约

> 日期：2026-09-21。Owner 已裁决 D1–D3；本文约束 M1 实施。
> 当前交付为 Stage E：Stage D 持久 controller 之上，交互终端界面（聊天/审批/授权/会话/恢复面板、
> worker/UI 隔离桥、README 键位表）已实现并有结构与消息流测试；
> 真终端人工验收（PTY、resize、中文粘贴）属 Stage F。

## 1. 定位与依赖边界

`harness/` 是同仓库、同 Go module 的应用卫星：消费 `agent`、`llm`、
`store`、`tool` 的公开接口，提供本机交互式编码助手。内核不反向依赖
harness，不因应用入口增加 UI、文件快照或终端策略。没有稳定的
`harness.New` 公共库 API；应用代码放在 `harness/internal/`。

M1 冻结内核 API 与实现。`docs/DESIGN.md` 仍是内核契约，本文只定义消费者
行为；根设计文档仅登记卫星定位。`scripts/kernel-closure-guard.sh` 继续验证
四个核心包的完整编译闭包为 stdlib-only，MCP SDK 的版本和 importer 边界不放宽。
module 总数允许增长，不再把全仓 14 个 module 当作零依赖内核的替代判断。

UI 使用 Charm 的 Bubble Tea v2、Bubbles v2 与 Lip Gloss v2；稳定 v2 的模块
路径是 `charm.land/.../v2`。三者分别承担终端循环、输入/视窗组件、布局。
只有 `internal/tui` 导入这些库；配置、controller 和工具不得暴露 `tea.Msg`
或 `tea.Model`。不引入 Markdown renderer、表单或动画框架。
实际版本、module 增量、许可证与编译闭包见 [README.md](README.md)。

## 2. 冻结范围与 M2 对照

| 能力 | M1 契约 | M2 / 非目标 |
|---|---|---|
| 聊天 | 正常运行流式正文；推理可见、默认折叠 | GUI、图片观察 |
| 文件工具 | read/glob/grep/edit/write；单文件有前置条件的修改 | 通用 patch、多文件事务 |
| shell | Linux、本机前台进程组、stdin EOF、有界输出 | PTY、持久 shell、后台 jobs、逃逸进程隔离 |
| 审批 | 默认逐次；精确文件集 edit/write grant，最多 20 次或 30 分钟 | shell 前缀授权、目录通配授权 |
| 免审批 | 仅显式 `--no-approval`，常驻标识 | 环境、配置、项目文本或运行时开启 |
| 会话 | 持久索引、只打开、显式继续或放弃、取消后原会话改向 | 输入队列、后台唤醒、多写者 |
| 恢复文件 | 每次 edit/write 的前后像，保住原有未提交修改 | shell 修改、全工作区快照、对话时间旅行 |
| 模型 | 启动选择 openai/openai_responses/glm，已有会话配置冻结 | 会话中途换模型、自动跨协议 fallback |
| 扩展 | 内置工具与项目规则 | MCP、agenttool、插件、skills、自动记忆 |

D1 锁定 Linux-first。D2 锁定精确文件集合及 20 次/30 分钟先到先失效；
shell 永远不在 grant 内。D3 接受非流式恢复与历史缓存缺口，见 §7。
取消不是撤销：按键前或同时已进入执行的写入可能完成。

## 3. 分层与一次任务走查

```text
cmd/go-agent          启动时读 flags/env，组装应用，管理退出码
internal/config      启动配置；后续 provider 组装
internal/app         单 controller、run 生命周期、审批与 worker/UI 接口
internal/session     workspace 锁、meta、展示缓存、outputs
internal/tools       六工具、路径约束、shell executor
internal/snapshot    edit/write 前后像、恢复协议
internal/prompt      静态规则来源、动态工作区事实
internal/tui         唯一终端所有者；Bubble Tea、Bubbles、Lip Gloss
```

用户启动后，应用校验配置，取得 workspace 独占进程锁，显示规则来源、
模型与权限模式。新任务先持久保存随机 ThreadID 和 workspace 元数据，
再构造 run ctx、审批 closure、退出 token 回调并调用 `RunThreadStream`。
read/grep 返回真实文件内容与适用规则；edit 显示具体 diff，批准后先保存
快照再写盘。shell 即使处于文件授权模式也单独审批，输出附件落盘。

controller 拥有执行状态；每个活动 run 只有一条 worker 消费链。网络、
磁盘、审批等待、进程 join 不在 TUI Update 内发生。UI 只处理内存状态与
输入；worker 经有界、可取消的队列传递普通宿主消息，文本增量可按帧合并，
工具/审批/结束消息不可丢弃。不为每个 delta 启动 goroutine。
TUI 独占 stdin/stdout，工具不打印终端，日志写文件。UI 退出或断开必须
取消并 join worker，不能仅停止消费 iterator 就宣称已停止。

Stage A 只声明 worker/UI 接缝，运行静态终端，拒绝任务执行。B 接通配置、
规则和文件工具；C 接通审批、shell、快照；D 接通持久 controller；
E 实现交互视图；F 做整体实测。占位包不注册可执行工具，不把 TODO 当成功结果。

## 4. 工具与副作用

| 工具 | 输入/结果边界 |
|---|---|
| read | workspace 相对路径、1-based offset/limit；行号、hash、截断标记；opaque output ID 可分页读保留的 shell 输出，不可转任意宿主路径 |
| glob | 分段 `*`、`?`、`[]` 和整段 `**`；字典序、分页，不跟随目录链接 |
| grep | Go regexp、glob、大小写、上下文、分页；path:line 与不完整说明，不承诺 PCRE |
| edit | 单路径、expected hash、old/new text；old 精确出现一次，写前再次核验 |
| write | 单路径、完整文本；新文件须不存在，覆盖须 expected hash；保留权限，不增加执行位 |
| shell | `/bin/sh -c`、workspace 内 cwd、timeout；结果区分退出码、取消、超时、输出上限，含输出 ID |

文件访问使用受根约束的 API，不用 `os.Chdir` 或仅靠字符串前缀。
拒绝绝对/越界路径、符号链接、特殊设备；写入拒绝多硬链接文件。
`.git` 和 harness 数据区不可写。`.env` 等凭据不自动读取，明确读取须
本次批准，免审批模式除外。搜索默认排除 `.git`、`node_modules`、`vendor`
及构建输出，可在启动配置调整，不宣称完整 `.gitignore` 兼容。
扫描上限为 100,000 路径、64 MiB 文本、10 秒；任何上限触发都标记结果不完整。
单文件修改限 2 MiB UTF-8 文本，不支持删除、重命名或二进制编辑。

shell 是用户权限的本机执行，不是沙箱；批准的命令仍可访问网络、仓库外
文件及凭据。默认 120 秒，最高 600 秒；run 默认 15 分钟。去掉注入的 provider
key 后继承环境。取消/超时发送 TERM，2 秒未退出则 KILL；等待主进程、
同组后代和输出采集结束，正常退出遗留的同组后台子进程也清理。
无法确认清理则禁止新运行；不保证 daemon/setsid/nohup 或整机崩溃后的清理，
重启不凭旧 PID 盲杀。

输出边落盘边保留 head/tail，inline 预算取 8 KiB 与内核结果预算留足引用后
余量的较小者，退出事实和输出 ID 放首部。每命令原始输出限 64 MiB，
触顶清理后软失败，仅保留额度内原文。I/O 失败属于基础设施错误。
正常命令失败、参数错误、拒绝、过期 hash 是软结果；父 ctx 取消与持久性/
资源收敛错误保留 Go error 和原因链。UI 过滤外来 ANSI/OSC 控制码，
原始附件不可直接作为终端控制流渲染。

## 5. 审批与取消改向

普通 read/glob/grep 自动放行；edit/write/shell 和敏感读取经过 gate。
审批必须展示完整参数、路径/diff 或 command/cwd/timeout，可滚动，
不按字符裁剪批准依据。没有 callback、UI 已关闭或 ctx 已取消一律拒绝。

Ctrl+G 创建或撤销精确文件集 grant，绑定 ThreadID、workspace、当前进程；
切换会话、重启、取消、改向即撤销。最多 20 次 edit/write 或 30 分钟，
不覆盖 shell、敏感文件、项目指令文件、越界路径。新建授权不能顺带批准
另一个待批请求。授权、自动放行与拒绝记宿主审计元数据，不扩充内核事件。

每次 run 的审批 closure 捕获该 run ctx；请求携带宿主 generation、递增
request ID、工具名、不可变参数副本和一次性 reply。公开 callback 没有 call ID，
不从文本猜它。UI 与 controller 都核对身份，消费批准前再查 ctx/策略；
工具 Execute 开头和最终写入/启动进程前再次查 ctx。

Ctrl+C 或 `/stop` 先失效待批请求和 grant，再 cancel、join，最后用原退出
token 调 `SettleThread`。`WithRunExitFn` 只保存 token，不在 callback 内重入；
必须等 iterator/Resume 和所有 owned work 返回后，另建 10 秒有界 ctx 结算。
写入确认不明只可重试同一 token；不得用 `SettlementTarget` 刷新旧取消的
RunID/head。冲突/收尾失败保持“未结算”，禁止新任务，等待新的显式恢复决策。
自然完成保留完成事实，不因 `ErrNothingToSettle` 取消后继任务。

结算成功才接受原 ThreadID 的新输入；结算和新任务是两次持久提交，
中间崩溃不代表新草稿已接受。忙时可编辑草稿，Enter 不排队。
provider/磁盘错误、run timeout 保留 incomplete，不自动放弃。

## 6. 会话、规则与快照

会话位于用户数据目录，按规范 workspace 路径分区：
`sessions/<id>/meta.json`、共享 `store/`、展示缓存、`outputs/`、`snapshots/`。
目录 0700、文件 0600；首个模型调用前 meta 必须 fsync。扫描 meta 得到列表，
不要求 Store 枚举线程。同一 workspace 只允许一个进程和一个 JSONL Store；
锁冲突报错，不绕锁。数据不自动 GC；离线归档时 outputs/snapshots 一并处理。

`/sessions` 列表；`/new` 新建；`/resume <id>` 或启动 `--resume <id>` 只打开。
打开读展示缓存并用 `SettlementTarget` 检查 incomplete，不调用 Resume 探测。
中断时用户选继续才调用 `ResumeThread`；选放弃才以新的恢复决策取得 target
并结算。切换、新建、restore 必须等活动执行/清理结束，不能切走遗忘旧任务。

新线程冻结内置 prompt、用户级指令、根 AGENTS.md 的路径/hash/组装版本；
根规则变更提示新建会话，不热换已持久 system。嵌套规则随工具路径提示，
read 返回适用规则；edit/write 前核验已读取当前版本，新文件可先查父目录规则。
不自动加载父目录/home 的其他规则、skills 或 memory。
静态规则默认限 8 KiB，超限报错。`WithRoundContextProvider` 每次返回完整、
有界的 cwd、branch/dirty、权限、恢复事实、规则版本，不作为消息队列。

`/changes` 列工具修改；`/restore <id>` 由用户确认。恢复占用 controller 的
独占操作槽，模型不能请求恢复。每次实际 edit/write 一个 change ID，
绑定 workspace、ThreadID、generation、工具次序；同路径按逆序恢复最新未撤销项。

写入协议：核验当前文件 → before/after 字节、存在性、权限和 prepared manifest
落盘/fsync → 再核验 → 同目录临时文件写入/fsync/rename → 父目录 fsync →
标记 applied。快照不可用就拒绝写入；no-op 不生成可恢复项。
prepared 崩溃恢复按当前文件等于 before、等于 after、其他冲突区分，不猜成功。
restore 同样有 prepared/restored 状态并幂等。新文件恢复只删除匹配 after 的文件，
不递归删除目录。

恢复要求当前字节、存在性、权限匹配 after；冲突只提供前像/差异，不强制覆盖。
before 保留用户已有未提交修改。M1 要求实际修改/恢复期间无外部并发写者，
工作区锁不锁用户编辑器，check-to-rename 不是跨进程事务。
shell 不生成可恢复记录。恢复不回退对话；下一次环境快照传递恢复事实，
旧 read hash 失效，要求重读。每会话快照默认 256 MiB、可启动调整，
额度满拒绝新修改，不淘汰承诺保留的前像。不恢复 ACL/xattr/mtime 全部元数据。

## 7. 事件与 D3 展示降级

正常流处理七种 AgentEvent 的 value/pointer 形态。Text/Thinking 增量是当前
尝试的观察；ToolCall 只是请求，不等于已执行；ToolResult 返回不等于该轮 durable
commit。Retry 仅展示真实重试；Compaction 显示策略、前后 runes、移除组数并
以 History 更新视图；Done.History 是权威快照，替换对齐，不重复追加最终正文。
error 或无 Done 结束标记不完整，不合成 Done。

Chat ReasoningBlock 与 thinking 可显示；Responses ReasoningItemChunk 没有
AgentEvent，只从 Done/RunResult.History 读取 ReasoningItemBlock.Summary。
没有 summary 就说明不可读，不展示 EncryptedContent、不编造推理；
流式摘要与最终快照对齐替换。推理默认折叠。宿主工具开始、审批、清理消息
不实现 AgentEvent，不混入内核日志。

| 缺口 | M1 接受的行为 | 独立 kernel 升级申请 |
|---|---|---|
| K1：无公开流式恢复入口 | ResumeThread 显示“恢复中（非流式）”、真实工具/审批与取消入口；结束后一次显示 History；不伪造正文、thinking、retry、compaction 增量 | ResumeThreadStream；约 2–4 日内核 + 1 日接线 |
| K2：无任意线程只读完整 History 投影 | 成功/压缩时在 worker 放行前采集公开 History 与 Store head，公开 JSON codec 原子保存/fsync；打开比较 Latest.Head，缺失/落后明确标记缺口 | 只读 History API；约 2–3 日内核 + 1–2 日 UI |

缓存永远不作为模型执行历史，不复制内核私有 replay/codec。取消结算后可用
已保证只读的 cancelled ResumeThread 刷新视图，普通打开不可冒用 Resume。
缓存保存失败不能报告“界面记录已保存”；内核终态和缓存并非事务。
缓存落后可显示已保存用户输入/临时观察，但标为宿主记录。压缩后的有效上下文
不是未删减档案。

M1 沿用默认标准压缩链与 80% rune 估算阈值，不默认多发 LLM 摘要请求，
不宣称精确 token 百分比或账单。provider overflow 保留 incomplete，用户决定
缩减/放弃/新会话，不无限重试，不提供 durable `/compact`。

## 8. 启动配置与阶段验收

目标优先级：flags > `GO_AGENT_*` 非秘密环境 > 用户 JSON > 内置默认；
只在启动读取，不读仓库配置。provider 默认 openai，model 必须显式配置，
base URL 默认沿用 adapter。API key 仅从指定环境变量取（默认
OPENAI_API_KEY / GLM_API_KEY），无 key 值 flag、不写盘。`--no-approval`
只接受命令行。GLM 含点 key 的 JWT 判定沿用现有 adapter，须明确诊断。
已有线程使用原模型/协议，冲突提示新建，不通过重启偷换模型；key 可轮换。
诊断净化 headers、URL userinfo/query、provider error body；源码缓存不宣称脱敏。

默认 30 轮、15 分钟 run、4096 输出 tokens、8192 保守上下文预算；
`--context-budget` 是用户设置的启发式预算，不是模型自动识别窗口。
恢复视为新一次执行预算，不声称跨重启累计费用封顶。
Stage B 已接通启动 JSON/env/flags、provider 构造、workspace 根封装与静态规则。
Stage C 在其上交付：`app.BeginRun` 组装六工具（read/glob/grep/edit/write/shell），
审批三模式（逐次默认、精确文件集 grant 20 次/30 分钟、显式 `--no-approval`）、
运行身份绑定（迟到批准拒绝、零执行）、`app.Gate` 把同一张批准凭据交给
`snapshot.Store` 的 prepared→applied 协议，shell 输出落盘并按 opaque ID 分页。
`--snapshot-quota` 调整每会话快照额度（默认 256 MiB）。
缺少快照/输出存储时 `BeginRun` 拒绝开始，`--no-approval` 不降低此门槛。
取消/超时的进程组清理经真实孙进程测试；清理未确认时执行器拒绝新命令。
Stage D 的持久 controller 消费这些部件；TTY 启动仍不启动模型。
非 TTY 打印帮助并退出 0；不是免审批管道模式。

测试使用 stdlib testing；同步信号后触发取消，timeout 只作失败上界，
不靠 sleep/轮询。Stage A 覆盖帮助、参数解析和骨架启动退出；后续阶段再
覆盖路径/快照/审批/取消/重启，不能用 Stage A 通过宣称 M1 已完成。
阶段门包括全仓 build/vet/test、harness tests、gofmt 空输出、两个守卫、
实际 `--help` 与非 TTY 启动、终端退出及依赖图实测。
