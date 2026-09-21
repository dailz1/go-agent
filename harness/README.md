# go-agent harness

同仓库的交互式编码助手应用卫星，消费 go-agent 公开 API。
**当前仅 Stage A 骨架：能启动、查看帮助和退出，不能聊天、执行工具或恢复会话。**
M1 目标与边界见 [DESIGN.md](DESIGN.md)，不应将契约中的目标能力当成已交付功能。

## 运行

要求 Go 1.26.1，首发验收 Linux。在仓库根目录：

```sh
go run ./harness/cmd/go-agent --help
go run ./harness/cmd/go-agent --provider openai --model YOUR_MODEL
```

TTY 中显示 Stage A 状态页，按 `q`、`Esc` 或 `Ctrl+C` 退出。
stdin 或 stdout 非 TTY 时只打印帮助、退出 0，不尝试打开 `/dev/tty`，
也不执行模型；这不是无人审批的管道模式。帮助无需模型或 API key。
用 `go build -o /tmp/go-agent ./harness/cmd/go-agent` 构建当前骨架。
包含 harness 的版本发布后，安装路径为
`go install github.com/dailz1/go-agent/harness/cmd/go-agent@<version>`。

## Stage A 启动表面

| 参数 | 环境默认 | 含义 |
|---|---|---|
| `--provider` | `GO_AGENT_PROVIDER`，否则 `openai` | `openai`、`openai_responses`、`glm` |
| `--model` | `GO_AGENT_MODEL` | 模型名；交互启动必须指定 |
| `--base-url` | `GO_AGENT_BASE_URL` | endpoint；空值预留 adapter 默认 |
| `--api-key-env` | `GO_AGENT_API_KEY_ENV` | key 所在环境变量的名字；默认 OPENAI_API_KEY，glm 为 GLM_API_KEY |
| `--no-approval` | 无；环境变量不能开启 | 显式免审批选择；状态页常驻标识 |
| `--help` / `-h` | 无 | 打印帮助，退出 0 |

命令行覆盖环境。Stage A 不读取 key 值，不将其放入配置结构或显示；
没有 `--api-key` 参数。当前不建立 provider、不访问网络、不写会话数据，
不加载用户 JSON。完整配置优先级、资源预算、`--resume` 与规则接线在 B–D 实现。
模型/endpoint 只启动配置；M1 不支持会话中途更换模型。
未知参数、位置参数、非法 provider/endpoint/环境变量名返回使用错误（退出 2）。

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
`internal/session`、`tools`、`snapshot`、`prompt` 暂为后续阶段的包边界。
Bubble Tea 类型不穿过 UI 边界；当前没有后台模型 worker 或可执行工具。

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
