# go-agent harness

同仓库的交互式编码助手应用卫星，消费 go-agent 公开 API。
**当前 Stage E：交互终端界面已交付——Bubble Tea 聊天面（多行/中文输入、滚动回看、七事件渲染、
折叠推理、压缩通知、缓存/实时分区）、审批面（逐次批准/拒绝、精确文件集 grant 管理）、
会话 UX（新建/列表/打开、非流式恢复、取消后同会话改向、快照恢复入口）与 worker/UI 隔离桥。
Stage D 的持久 controller 与崩溃窗口矩阵不变；真终端人工验收（PTY resize、中文多行粘贴、拒绝/取消/重启实测）属 Stage F。**
M1 目标与边界见 [DESIGN.md](DESIGN.md)，不应将契约中的目标能力当成已交付功能。

## 运行

要求 Go 1.26.1，首发验收 Linux。在仓库根目录：

```sh
go run ./harness/cmd/go-agent --help
go run ./harness/cmd/go-agent --provider openai --model YOUR_MODEL
```

TTY 中显示 provider、workspace、规则来源和审批模式后进入聊天界面；键位见下节，
`q` 是普通字符不再是退出键。
stdin 或 stdout 非 TTY 时只打印帮助、退出 0，不尝试打开 `/dev/tty`，
也不执行模型；这不是无人审批的管道模式。帮助无需模型或 API key。
用 `go build -o /tmp/go-agent ./harness/cmd/go-agent` 构建当前骨架。
包含 harness 的版本发布后，安装路径为
`go install github.com/dailz1/go-agent/harness/cmd/go-agent@<version>`。

## 快速上手（首个会话）

1. **构建与确认入口**：`go build -o /tmp/go-agent ./harness/cmd/go-agent`，
   `/tmp/go-agent --help` 无需 key 即可打印参数面。
2. **最小配置面**：交互启动必须显式给 `--model`；`--provider` 选
   `openai` / `openai_responses` / `glm`，key 只从环境变量读取（`--api-key-env` 指定名字，
   默认按 provider）。OpenAI 兼容端点用 `--provider openai --base-url <endpoint>` 搭配，例如：

   ```sh
   /tmp/go-agent --provider openai --base-url https://api.deepseek.com/v1 \
     --model deepseek-chat --api-key-env DEEPSEEK_API_KEY --workspace /path/to/proj
   ```

   启动页显示 provider、workspace、规则来源与审批模式；`--data-dir` 可把私有数据区
   指到别处（默认 XDG 数据目录）。完整参数表见下文「启动配置」。
3. **创建与对话**：在聊天面输入任务后 `enter` 提交——首个输入自动创建会话并持久化
   ThreadID；`ctrl+j` 换行，可直接粘贴中文多行文本。正文逐段流式出现，推理默认折叠
   （`ctrl+r` 展开）。
4. **审批**：模型请求 edit/write/shell（或敏感 read）时弹出审批面板，展示完整路径、
   diff 或完整命令；`y`/`enter` 批准，`n`/`esc` 拒绝（软错误回传模型，它可据此调整）。
   批准只对这一次请求生效；也可 `ctrl+g` 为一组精确文件建立有限 grant，shell 永不继承。
5. **停止与改向**：任务运行中按 `esc`（或 `/stop`）：先失效待批请求、取消运行、
   join 后用原退出 token 结算；结算完成后同会话直接接受新输入（改向，ThreadID 不变，
   旧任务模型调用数为 0）。
6. **重启与恢复会话**：退出后再次启动，`/sessions` 列出持久会话；打开仅渲染缓存视图，
   不调用模型。上次有中断任务时，先选 `/continue`（非流式续跑）或 `/abandon`
   （结算后改向）才继续。
7. **文件恢复**：`/changes` 列出本会话可恢复的 edit/write 变更，`/restore <id>`
   展示 diff 并确认后把该文件恢复到写入前字节（新建文件恢复后删除；用户原有未提交
   修改保存在前像中）。shell 造成的修改不在恢复范围。

## 终端界面与键位

聊天界面由 Bubble Tea v2 驱动：输入区支持多行（ctrl+j 换行）与粘贴（含中文），
滚动回看区按 DESIGN §7 规则渲染七种事件——工具卡片在结果事件到达前只标记“已请求”，
推理默认折叠（ctrl+r 展开），压缩通知显示策略与前后 runes，缓存打开的历史
带来源横幅与“缓存落后”缺口说明，恢复中的任务明确标注非流式、不伪造增量。
审批/授权/会话/恢复各是独立面板，面板打开时按键不会漏进聊天输入。
应用内 `?`（输入为空时）或 `/help` 显示同样的键位说明。

| 键 | 作用 |
|---|---|
| `enter` | 提交输入；首个输入自动创建会话 |
| `ctrl+j` | 输入区换行（多行输入，粘贴可用） |
| `esc` | 关闭面板；任务运行中＝停止（cancel → join → settle，结算后原会话接收新输入）；空闲＝退出 |
| `ctrl+c` | 任务运行中＝停止并在结算完成后退出；停止窗口内再按＝强制退出；空闲＝退出 |
| `ctrl+r` | 展开/折叠推理区 |
| `ctrl+g` | 授权面板（精确文件集，20 次/30 分钟） |
| `pgup` / `pgdown` | 回看翻页；`ctrl+u` / `ctrl+d` 半页 |
| `?` | 帮助（仅输入为空时） |

| 命令 | 作用 |
|---|---|
| `/new` | 新会话 |
| `/sessions` | 列出持久会话；打开＝只读，不调模型 |
| `/resume <id>` | 打开指定会话；中断任务先选继续或放弃 |
| `/stop` | 停止：cancel → join → 原 token 结算（运行中可用） |
| `/continue` | 继续中断任务（非流式恢复，可取消） |
| `/abandon` | 放弃旧任务并结算，随后接收新输入 |
| `/retry` | 重试被阻塞的结算 |
| `/changes` | 列出可恢复的 edit/write 变更 |
| `/restore <id>` | 确认后恢复一次文件变更 |
| `/grant` | 授权面板 |
| `/help` | 帮助 |

审批面板：`y`/`enter` 批准、`n`/`esc` 拒绝（软拒绝回传模型），完整 diff/命令可滚动；
运行结束后未答复的请求自动失效，迟到按键不生效。
授权面板：`enter` 添加路径、`ctrl+x` 移除所选、`ctrl+a` 应用、`ctrl+r` 撤销。

## 启动配置

| 参数 | 环境默认 | 含义 |
|---|---|---|
| `--provider` | `GO_AGENT_PROVIDER`，否则 `openai` | `openai`、`openai_responses`、`glm` |
| `--model` | `GO_AGENT_MODEL` | 模型名；交互启动必须指定 |
| `--base-url` | `GO_AGENT_BASE_URL` | endpoint；空值预留 adapter 默认 |
| `--api-key-env` | `GO_AGENT_API_KEY_ENV` | key 所在环境变量的名字；默认 OPENAI_API_KEY，glm 为 GLM_API_KEY |
| `--no-approval` | 无；环境变量不能开启 | 显式免审批选择；状态页常驻标识 |
| `--config` | `GO_AGENT_CONFIG` | 用户 JSON；默认用户配置目录的 `go-agent/config.json` |
| `--workspace` | `GO_AGENT_WORKSPACE`，否则 `.` | 规范化的工作区根 |
| `--user-rules` | `GO_AGENT_USER_RULES` | 显式用户指令文件；不扫描父目录或 home 的规则 |
| `--data-dir` | `GO_AGENT_DATA_DIR` | 私有数据区；默认 XDG 数据目录下 `go-agent` |
| `--excludes` | `GO_AGENT_EXCLUDES` | 逗号分隔的搜索排除目录名 |
| `--max-iterations` | `GO_AGENT_MAX_ITERATIONS`，否则 30 | 每次执行轮数上限 |
| `--context-budget` | `GO_AGENT_CONTEXT_BUDGET`，否则 8192 | 启发式上下文预算 |
| `--max-output-tokens` | `GO_AGENT_MAX_OUTPUT_TOKENS`，否则 4096 | 输出 token 预算 |
| `--run-timeout` | `GO_AGENT_RUN_TIMEOUT`，否则 `15m` | 每次执行时间预算 |
| `--snapshot-quota` | `GO_AGENT_SNAPSHOT_QUOTA`，否则 256 | 每会话快照额度（MiB）；满额拒绝新 edit/write |
| `--help` / `-h` | 无 | 打印帮助，退出 0 |

优先级为命令行 > 非秘密环境 > 用户 JSON > 默认值。JSON 字段使用 snake_case，
拒绝未知字段、key 值和 `no_approval`；不读取仓库配置。TTY 启动读取指定变量的
key 并构造 adapter，不访问网络、不写会话。key 不进入 Config、规则或启动显示，
也没有 `--api-key` 参数。endpoint 拒绝 userinfo、query 和 fragment，避免意外泄密。
GLM 含点 key 的 JWT 判定会显示提示。执行预算由后续 controller 消费。
模型/endpoint 只启动配置；M1 不支持会话中途更换模型。
未知参数、位置参数、非法 provider/endpoint/环境变量名返回使用错误（退出 2）。

## 文件工具、shell 与副作用 gate

六个工具（read/glob/grep/edit/write/shell）由 `app.Startup.BeginRun(ctx, thread, snapshots, outputs)`
按次运行组装：它 `Approvals.Begin` 创建携带独立 token 的 run，把每个工具包上运行身份，
再把 `app.Gate` 作为 `tools.WriteGate` 接入 edit/write、把 `RunApproval` 接入敏感 read 与 shell。
缺少会话快照/输出存储时 `BeginRun` 直接拒绝，`--no-approval` 不降低此门槛。
观察是 `ToolResult.Content` 中的 JSON：read 含行号、完整内容 SHA-256、截断标记和适用规则；
搜索含排序路径或 path/line 命中、规则路径提示、截断原因与下一页 offset。
分页统一从 1 开始，默认 200、最多 1000 项；read 的单文件读取上限 2 MiB，行文本预算 32 KiB。
搜索默认排除 `.git,node_modules,vendor,build,dist,target` 与敏感文件；不是 `.gitignore` 解释器。
默认扫描 100,000 路径、64 MiB、10 秒，触顶明确标记不完整。

### 审批三模式

- 默认逐次：edit/write/shell 与敏感 read 每次都发审批请求，携带 run token、递增请求 ID、
  工具名与不可变参数副本；批准只对这一请求生效，重复/迟到/错 run 的回复一律拒绝。
- 精确文件集 grant（`Approvals.Grant`）：仅覆盖选定 edit/write 路径，最多 20 次或 30 分钟；
  新路径重新询问；shell、敏感文件、项目指令文件、越界路径永不入 grant。切换会话/取消/改向即撤销。
- `--no-approval`：仅显式命令行，运行时不可切换进入；它跳过询问但不跳过快照。

内核审批回调与工具内检查共享同一张一次性凭据：`WithApprovalFn` 传入 `RunApproval.ApprovalFn`，
批准后的 permit 只能被同一参数的工具调用消费一次，改一个字符都要重新问。
取消 run 后迟到的批准被拒绝且零执行（有测试钉住）；进入执行前的最后一刻取消同样拒绝。

### shell 执行器

`tools.NewShell` 以独立进程组启动 `/bin/sh -c`，stdin 为 EOF，cwd 限制在 workspace 内，
启动前去掉 provider key 所在环境变量。默认 120 秒、最高 600 秒；取消/超时先 TERM、
2 秒后 KILL，并等主进程、同组后代（含正常退出后遗留的后台子进程）与输出采集全部退出。
清理无法确认时执行器锁死，拒绝后续命令。输出边落盘边保留 head/tail（inline 预算 8 KiB），
原始输出上限 64 MiB，触顶清理后返回软失败；输出按 opaque ID 用 read 分页回查。
本机用户权限执行，不是沙箱；不承诺清理主动逃逸进程组的进程。

### 快照与恢复

`snapshot.Store` 按会话打开（绑定 workspace 与 ThreadID，0700 目录、0600 文件）：
每次 edit/write 先把 before/after 字节、存在性、权限与 prepared 状态落盘 fsync，
再由 `Workspace.Replace` 同目录临时文件原子替换，最后标记 applied。
prepared 崩溃后不猜成功：当前文件等于 before 视为未应用，等于 after 视为可能已应用，
其余为冲突；`Approvals.Restore`（/restore 命令路径，要求无活动 run）按逆序恢复同路径最新未撤销项，
恢复前核验当前字节/存在性/权限匹配 after，冲突时拒绝并提供前像，不提供强制覆盖。
持久化故障经故障注入测试：prepared 阶段任何写入失败都不触发 apply，apply 后的失败保留证据并可重开核验。
**明确边界：shell 造成的文件修改不在恢复范围内**——shell 从不生成可恢复记录；
若 shell 恰好改了同一文件，通常表现为恢复时的 after 冲突，由人工处置。
恢复不回退对话；额度满拒绝新修改，不淘汰已承诺的前像。

## M1 边界

- 流式聊天；read/glob/grep/edit/write 和有界前台 shell。
- 默认逐次审批。精确 edit/write 文件集合可授权最多 20 次或 30 分钟；
  shell 不继承授权。免审批只能显式启动指定。
- 持久会话；停止需 cancel、join、原 token `SettleThread` 后才能在同会话改向。
- edit/write 前后像可恢复；shell 修改不覆盖，恢复文件不回退对话。
- 推理默认折叠；中断恢复非流式，历史缓存落后会明确说明缺口，不补造事件。
- Linux-first。本机 shell 是用户权限执行，不是沙箱，也不保证主动逃逸进程清理。
- MCP、agenttool、模型热切换、全工作区快照推迟到 M2。

## 布局与验证

`cmd/go-agent` 组装入口；`internal/config` 启动解析；
`internal/app` 声明 controller/worker/UI 接缝，拥有审批管理器、run 身份、`Gate` 与 `BeginRun` 组装；
`internal/tui` 独占终端。`internal/tools` 实现六工具与 shell 执行器；`workspace` 负责路径与文件访问，
`prompt` 固定规则，`snapshot` 持有前后像与恢复协议。`internal/session` 管理持久索引与展示缓存。
Bubble Tea 类型只在 `internal/tui`；controller 的每次 run 由 worker 消费流，
UI 经 bridge 的有界队列收发消息（事件可合并、审批不丢、intent 串行），Update 内不做阻塞调用。

```sh
go build ./...
go vet ./...
go test -count=1 ./harness/...
go test -count=1 ./...
gofmt -l .
bash scripts/kernel-closure-guard.sh
bash scripts/check-readme-contract.sh
go list -m all
go run ./harness/cmd/go-agent --help
```

根双语守卫只覆盖 `README.md` 与 `README.zh-CN.md`；本目录仅提供中文 README，
未额外创建英文镜像。`examples/agentcli` 仍是演示，正式应用方向使用本入口。

## UI 依赖

锁定 Charm 官方稳定 v2：Bubble Tea `charm.land/bubbletea/v2 v2.0.9`，
Bubbles `charm.land/bubbles/v2 v2.2.1`，Lip Gloss `charm.land/lipgloss/v2 v2.0.6`。
它们分别提供终端循环、视窗组件和布局；终端检测使用同一依赖链的 `x/term`。
2026-09-21 在 Linux 的 Stage A 实测：

| 口径 | 基线 / 最终结果 |
|---|---|
| `go list -m all`，含主 module | 14 → **40 行，新增 26 个 module** |
| 新增直接依赖 | 三个 UI 库及 `github.com/charmbracelet/x/term v0.2.2` |
| 已有 module 被 MVS 升级 | `golang.org/x/sys` v0.41.0 → v0.47.0；`golang.org/x/sync` v0.20.0 → v0.22.0 |
| Linux 应用编译闭包中的第三方 module | **18 个**（含原有 x/sys、x/sync），不是完整 module 图的 39 个外部 module |
| 内核四包第三方编译闭包 | **0**；MCP SDK 仍直接锁定 v1.7.0 |

新增 module 的最终选择如下；其中部分只在上游其他组件或测试图中，
不能把 module 图全部称为当前二进制的运行依赖：

```text
charm.land/bubbles/v2 v2.2.1
charm.land/bubbletea/v2 v2.0.9
charm.land/lipgloss/v2 v2.0.6
github.com/MakeNowJust/heredoc v1.0.0
github.com/atotto/clipboard v0.1.4
github.com/aymanbagabas/go-udiff v0.4.1
github.com/bits-and-blooms/bitset v1.24.6
github.com/charmbracelet/colorprofile v0.4.3
github.com/charmbracelet/harmonica v0.2.0
github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886
github.com/charmbracelet/x/ansi v0.11.8
github.com/charmbracelet/x/exp/golden v0.0.0-20250806222409-83e3a29d542f
github.com/charmbracelet/x/term v0.2.2
github.com/charmbracelet/x/termios v0.1.1
github.com/charmbracelet/x/windows v0.2.2
github.com/clipperhouse/displaywidth v0.11.0
github.com/clipperhouse/stringish v0.1.1
github.com/clipperhouse/uax29/v2 v2.7.0
github.com/dustin/go-humanize v1.0.1
github.com/lucasb-eyer/go-colorful v1.4.1
github.com/mattn/go-runewidth v0.0.27
github.com/muesli/cancelreader v0.2.2
github.com/rivo/uniseg v0.4.7
github.com/sahilm/fuzzy v0.1.3
github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e
golang.org/x/exp v0.0.0-20231006140011-7918f672742d
```

当前 Linux 编译闭包为三个 Charm v2 库、colorprofile、ultraviolet、
x/ansi、x/term、x/termios、x/windows、displaywidth、uax29/v2、
go-colorful、go-runewidth、cancelreader、uniseg、terminfo、x/sync、x/sys。
已逐一核对这些锁定 module 的根 LICENSE（uniseg 为 LICENSE.txt）：
前 16 个为 MIT，x/sync 与 x/sys 为 BSD-3-Clause。此处是模块级许可证盘点，
发布分发仍须保留上游版权/许可声明，不替代仓库内附带数据的通知清单。

可在仓库根目录复核 Linux 编译闭包：

```sh
go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}' \
  ./harness/cmd/go-agent | sort -u
```

三个直接 UI 库锁稳定 tag；传递依赖 ultraviolet、golden 等仍含上游选择的
pseudo-version，不宣称整张依赖图全部使用稳定 tag。此次 x/sys/x/sync 升级后，
全仓 build/vet/test 与内核闭包守卫均通过。
