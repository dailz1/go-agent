# go-agent

[English](README.md)

[![ci](https://github.com/dailz1/go-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/dailz1/go-agent/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/tag/dailz1/go-agent?label=release)](https://github.com/dailz1/go-agent/releases)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

一个可嵌入的 Go agent runtime，不是 orchestration framework。它以小而可组合的构件提供 Agent、Provider、Tool、Event 和 Store。

## 10 分钟流式快速开始

这是基于 checkout 的快速开始。它会调用真实的 OpenAI-compatible API，可能产生供应商费用。

```sh
git clone https://github.com/dailz1/go-agent.git
cd go-agent
export OPENAI_API_KEY='...'
# OPENAI_BASE_URL and OPENAI_MODEL are optional.
go run ./examples/quickstart
```

应能在 stdout 看到流式文本，最后看到终态状态行。必须设置 `OPENAI_API_KEY`；`OPENAI_BASE_URL` 和 `OPENAI_MODEL` 用于选择兼容端点和模型。可以请求普通回答，或明确要求使用 demo tool。模型没有选择该工具不代表失败。

## 作为消费者安装

此流程和 checkout 快速开始分开；请在自己的 Go module 中执行：

```sh
go get github.com/dailz1/go-agent@latest
```

module path 是 `github.com/dailz1/go-agent`，本仓库要求 Go 1.26.1。将示例中的小构件复制到自己的 `main.go`；仓库相对路径 `./examples/quickstart` 不是消费者 module 的命令。

## 核心概念

- **Agent** 执行模型/工具循环，可流式输出事件或返回完成结果。
- **Provider** 是模型边界。可接入 OpenAI-compatible provider 或确定性的 test double。
- **Tool** 暴露 JSON schema，并返回模型可见的 soft error 或 Go hard error。
- **Event** 暴露文本增量、工具调用/结果、重试、压缩和终态结果，不引入 hook chain。
- **Store** 为 `RunThread` 和 `ResumeThread` 持久化显式 thread；配置 Store 后，`Run` 会生成 thread ID。

## 停止并改向持久线程

仅取消 context 会保留 incomplete、可恢复的 run。要放弃它，先取消 context，等待调用/iterator 及其 owned tools 退出，再用新的有界 context 调 `SettleThread(ctx, token)`。从 `RunInterruptedError.Settlement` 保存原 `SettlementToken{ThreadID, RunID, ExpectedHead}`；early-break 和匿名 stream 应配置 `WithRunExitFn`。同步退出回调在 **ownership release 之前**运行：仅保存值，不重入线程操作；并发运行时由宿主同步回调。

结算只写一条 `run_cancelled`，把未提交工具调用配成 outcome-unknown 结果，不重跑工具、不产生 Done、不撤销外部副作用。写入结果不确定时使用**同一 token**重试；过期 token 不能取消后继 run。`SettlementTarget` 是崩溃后供**新的显式恢复决策**使用的只读检查，不可用来刷新旧取消请求。

结算后，`ResumeThread` 返回 `Cancelled=true`、独立深拷贝的 canonical `History`、零值 `Message` 与执行统计，不执行、不写日志。正常 Done 仍返回 `ErrNothingToResume`。通过 `RunThread` 或 `RunThreadStream` 提交下一输入；停止与接受输入分别持久确认，不是原子改向。父结算非递归：宿主收集子 token，先结算子再结算父。

升级说明：调用者须检查成功 Resume 的 `RunResult.Cancelled`；`RunInterruptedError` 新增 `Settlement`。自定义 Store 必须支持 Schema 2。新取消记录和 checkpoint 使用 Schema 2（checkpoint 另带 `codec_version:2`）；旧日志无需迁移仍可读取，但旧二进制不能读取新格式。七种事件与非持久 history 入口不变。

## 可选的工具与审批观察

quickstart 包含一个简单的 echo tool。请明确要求使用它，但将模型的工具选择视为 best-effort 的手工观察。要获得确定性的 allow/reject 行为，请运行：

```sh
go run ./examples/approval
go test ./examples/approval -count=1
```

未安装 approval callback 时，带 `RequiresApproval` 的工具会被拒绝。审批不是 sandbox：没有合适的策略边界时，不要把 shell 或文件系统工具复制到生产环境。

## 示例

gallery 按学习路径排序。所有测试均为确定性且不访问网络；标记为 **manual** 的项目需要其说明的外部依赖。

| 需求 | 示例 | 覆盖 / 运行方式 |
| --- | --- | --- |
| 从流式和一个工具开始 | [quickstart](examples/quickstart) | **manual**：设置 API key 后运行 `go run ./examples/quickstart` |
| 理解审批 gate | [approval](examples/approval) | 确定性：`go test ./examples/approval -count=1` |
| 构建交互式、持久化 CLI | [agentcli](examples/agentcli) | **manual**：设置 API key；支持 `--resume` |
| 恢复持久化 thread | [resume](examples/resume) | 确定性：`go test ./examples/resume -count=1` |
| 并发执行同一响应中的工具 | [parallel](examples/parallel) | 确定性：`go test ./examples/parallel -count=1` |
| 包装 provider 或 tool | [middleware](examples/middleware) | 确定性：`go test ./examples/middleware -count=1` |
| 无网络测试 agent | [testdouble](examples/testdouble) | 确定性：`go test ./examples/testdouble -count=1` |
| 委派给 child agent | [agenttool](examples/agenttool) | 确定性：`go test ./examples/agenttool -count=1` |
| bridge MCP tools | [MCP bridge 指南](mcp/README.md) | **manual/reference**：参见其中的 transport 和 lifecycle 要求 |

完整 gallery 的编译和行为 gate：`go test ./examples/... -count=1`。

## 将 Agent 作为工具

`agenttool.New` 将预先构造的 child 适配为普通的 parent registry 工具。parent 决定何时委派以及 outer call 是否需要审批；child 保持自己的 provider、registry 和 options。

```go
childRegistry := tool.NewRegistry()
child := agent.New(childProvider, childRegistry, agent.WithMaxIter(2))
researcher := agenttool.New(child, agenttool.Config{
    Name: "researcher",
    Description: "Summarizes bounded research tasks.",
    RequiresApproval: true,
})
parentRegistry := tool.NewRegistry()
parentRegistry.MustRegister(researcher)
parent := agent.New(parentProvider, parentRegistry, agent.WithApprovalFn(approve))
result, err := parent.Run(ctx, "delegate a short research task")
_ = result
_ = err
```

每个返回的 adapter 只串行化自己的调用，并为每次调用执行一次无状态的 child `Run`。围绕同一 child 分别创建的 adapter 不会互锁。child event、session、Store thread ID 和 history 都不会转发；错误输入、child 截断和无文本输出是 soft tool result，child `Run` 与 context error 仍是 hard error。

## MCP bridge

`mcpbridge` package 是 client-only、tools-only 的 bridge。创建官方 SDK transport，将其连接到一个新建且未发布的 registry，并只在连接成功后发布该 registry：

```go
registry := tool.NewRegistry()
bridge, err := mcpbridge.Connect(ctx, registry, transport, mcpbridge.Config{
    Namespace: "docs",
})
if err != nil { return err }
defer bridge.Close()
```

使用前请阅读 [MCP bridge 指南](mcp/README.md)。特别是，在 `Close` 前必须 cancel 并 join 所有进行中的 agent 调用；工具执行期间并发 Close 不受支持。

## 生产检查表

- 显式设置实际模型、context window 大小和 `WithMaxIter`。
- 选择 Store thread 所有权；需要恢复时保留 thread ID。
- 用自己的 metrics/logging policy 记录 provider error、usage、retry 和 event。
- 用 approval gate 保护有副作用的工具，并让每个工具遵守 context cancellation。
- 使用 `agenttest` script 和 tool double 编写确定性测试。
- 审阅 [docs/DESIGN.md](docs/DESIGN.md) 中的公开契约。

## 限制

本 runtime 有意不提供 routing graph、agent pool、共享 child session、嵌套 child-event 转发或自动委派。`agenttool` 每次调用只执行一次 child `Run`。

MCP bridge 是 client-only、tools-only。它使用静态发现（没有 refresh 或 unregister），不提供 retry/redial、per-tool timeout、shared session、server mode、resources、prompts、sampling、roots、logging、elicitation UI、OAuth policy、automatic installation 或 network discovery。input-required continuation 会得到 soft unsupported result；非文本远端内容会表示为 placeholder 加 live-event Data，而 history 和 Store 只保留文本。完整的 schema/result/approval 边界见 [mcp/README.md](mcp/README.md)。

## 项目状态与贡献

项目已发布 [v0.1.0](https://github.com/dailz1/go-agent/releases/tag/v0.1.0)，采用 [MIT 许可证](LICENSE)。CI 在每次 push 到 `main` 时自动运行验证电池与内核闭包守卫。在独立规划的 v1.0 之前 API 仍可能演进；贡献应保持 [docs/DESIGN.md](docs/DESIGN.md) 的契约，并确保测试为确定性的。
