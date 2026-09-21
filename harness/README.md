# go-agent harness

同仓库的交互式编码助手应用卫星，消费 go-agent 公开 API。
**当前 Stage B：启动配置、固定项目规则和文件工具已实现；终端仍是状态页，不能聊天或恢复会话。**
应用没有接入审批与快照依赖，因此 edit/write 始终拒绝执行，包括 `--no-approval`。
M1 目标与边界见 [DESIGN.md](DESIGN.md)，不应将契约中的目标能力当成已交付功能。

## 运行

要求 Go 1.26.1，首发验收 Linux。在仓库根目录：

```sh
go run ./harness/cmd/go-agent --help
go run ./harness/cmd/go-agent --provider openai --model YOUR_MODEL
```

TTY 中显示 provider、workspace、规则来源和写入禁用状态，按 `q`、`Esc` 或 `Ctrl+C` 退出。
stdin 或 stdout 非 TTY 时只打印帮助、退出 0，不尝试打开 `/dev/tty`，
也不执行模型；这不是无人审批的管道模式。帮助无需模型或 API key。
用 `go build -o /tmp/go-agent ./harness/cmd/go-agent` 构建当前骨架。
包含 harness 的版本发布后，安装路径为
`go install github.com/dailz1/go-agent/harness/cmd/go-agent@<version>`。

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
| `--help` / `-h` | 无 | 打印帮助，退出 0 |

优先级为命令行 > 非秘密环境 > 用户 JSON > 默认值。JSON 字段使用 snake_case，
拒绝未知字段、key 值和 `no_approval`；不读取仓库配置。TTY 启动读取指定变量的
key 并构造 adapter，不访问网络、不写会话。key 不进入 Config、规则或启动显示，
也没有 `--api-key` 参数。endpoint 拒绝 userinfo、query 和 fragment，避免意外泄密。
GLM 含点 key 的 JWT 判定会显示提示。执行预算由后续 controller 消费。
模型/endpoint 只启动配置；M1 不支持会话中途更换模型。
未知参数、位置参数、非法 provider/endpoint/环境变量名返回使用错误（退出 2）。

## 文件工具与 Stage C 接线

已注册 `read/glob/grep/edit/write` 五个文件工具；提案中的第六个工具是 Stage C
的 shell，不在此阶段注册。观察是 `ToolResult.Content` 中的 JSON：
read 含行号、完整内容 SHA-256、截断标记和适用规则；搜索含排序路径或
path/line 命中、规则路径提示、截断原因与下一页 offset。
分页统一从 1 开始，默认 200、最多 1000 项。read 的单文件读取上限选为
2 MiB，行文本预算 32 KiB；grep 每行最多 1024 bytes，超大上下文会省略并标记。
需要完整长行时应直接检查文件；分页不会重建被裁剪的同一行。
搜索默认排除 `.git,node_modules,vendor,build,dist,target` 与敏感文件；
不是 `.gitignore` 解释器。默认扫描 100,000 路径、64 MiB、10 秒，触顶明确标记不完整。

`workspace` 通过 `os.Root` 约束访问，拒绝链接、特殊文件、越界路径；
写入另拒绝多硬链接、`.git` 和私有数据区。新文件不自动创建父目录。
根 AGENTS.md 与用户指令固定在 `prompt.Snapshot`（来源/hash/组装版本）；
嵌套 AGENTS.md 由 read 返回，写前必须已读当前版本。根规则变更要求新会话。
快照保存到会话元数据属于 Stage D，不会把内存固定误称为持久保存。

Stage C 提供 `tools.WriteGate.Commit(ctx, workspace.Change, apply)`：
确认当前 run 的有效批准，持久保存 before/after 与 prepared 快照，才可同步调用
`apply` 一次；成功后记录 applied。`apply` 再检查规则、文件存在性/字节/权限与 ctx，
然后同目录临时文件 fsync、原子替换及目录 fsync。拒绝返回 `tools.ErrDenied`，
持久性失败保留 Go error。工具元数据仍要求内核审批；Stage C 应关联同一批准凭据，
而不是把两个独立审批当成一个。应用目前不提供 WriteGate，因此没有放开副作用。
敏感 read 的 `ReadApproval`、opaque shell 输出的 `OutputReader` 同样留给 Stage C；
未接入时明确拒绝，不把输出 ID 当宿主路径。

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
`internal/app` 声明 controller/worker/UI 接缝；`internal/tui` 独占终端。
`internal/tools` 实现文件工具；`workspace` 负责路径与文件访问，`prompt` 固定规则。
`internal/session`、`snapshot` 暂为后续阶段的包边界。
Bubble Tea 类型不穿过 UI 边界；当前没有后台模型 worker。

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
