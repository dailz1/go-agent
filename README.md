# go-agent

[简体中文](README.zh-CN.md)

[![ci](https://github.com/dailz1/go-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/dailz1/go-agent/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/tag/dailz1/go-agent?label=release)](https://github.com/dailz1/go-agent/releases)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An embeddable Go agent runtime, not an orchestration framework. It provides Agents, Providers, Tools, Events, and Stores as small composable building blocks.

## 10-minute streaming quickstart

This is the checkout-based quickstart. It calls a real OpenAI-compatible API and may incur provider charges.

```sh
git clone https://github.com/dailz1/go-agent.git
cd go-agent
export OPENAI_API_KEY='...'
# OPENAI_BASE_URL and OPENAI_MODEL are optional.
go run ./examples/quickstart
```

You should see streaming text on stdout followed by a terminal status line. `OPENAI_API_KEY` is required; `OPENAI_BASE_URL` and `OPENAI_MODEL` select a compatible endpoint and model. Ask for a normal response, or explicitly request the demo tool. A model choosing not to call that tool is not a failure.

## Install as a consumer

Use this separately from the checkout quickstart, from your own Go module:

```sh
go get github.com/dailz1/go-agent@latest
```

The module path is `github.com/dailz1/go-agent` and this repository requires Go 1.26.1. Copy the small building blocks from the examples into your own `main.go`; repository-relative `./examples/quickstart` is not a consumer-module command.

## Core concepts

- **Agent** runs the model/tool loop and can stream events or return a completed result.
- **Provider** is the model boundary. Bring an OpenAI-compatible provider or a deterministic test double.
- **Tool** exposes a JSON schema and returns either a model-visible soft error or a Go hard error.
- **Event** exposes text deltas, tool calls/results, retries, compaction, and the terminal result without hook chains.
- **Store** persists explicit threads for `RunThread` and `ResumeThread`; `Run` gains generated thread IDs when a Store is configured.

## Stop and redirect a persistent thread

Cancellation alone leaves a run incomplete and resumable. To abandon it, cancel its context, wait for the call/iterator and its owned tools to exit, then call `SettleThread(ctx, token)` with a fresh bounded context. Save the original `SettlementToken{ThreadID, RunID, ExpectedHead}` from `RunInterruptedError.Settlement`, or configure `WithRunExitFn` for early-break and anonymous streams. The synchronous exit callback runs **before ownership release**: only save the value there; do not reenter thread operations. Synchronize callbacks when running concurrently.

Settlement writes one `run_cancelled` record, pairs uncommitted tool calls with outcome-unknown results, and never reruns tools, emits Done, or undoes external effects. Retry uncertain settlement writes with the **same token**. A stale token cannot cancel a successor. `SettlementTarget` is a read-only inspection for a **new explicit recovery decision** after a crash, not a way to refresh an old cancellation request.

After settlement, `ResumeThread` returns `Cancelled=true` with detached canonical `History`, zero `Message` and execution statistics, and no execution or writes. Normal Done still returns `ErrNothingToResume`. Submit the next input through `RunThread` or `RunThreadStream`; stopping and accepting that input are separate durable confirmations, not an atomic redirect. Parent settlement is non-recursive: the host collects child tokens and settles children before the parent.

Upgrade notes: consumers must check `RunResult.Cancelled` on successful Resume; `RunInterruptedError` now carries `Settlement`. Custom Stores must support Schema 2. New cancellation records and checkpoints use Schema 2 (checkpoints also carry `codec_version:2`); old logs remain readable without migration, but old binaries cannot read the new format. The seven-event set and nonpersistent history entry points are unchanged.

## Optional tool and approval observation

The quickstart includes a simple echo tool. Ask explicitly for it, but treat model tool selection as best-effort manual observation. For deterministic allow/reject behavior, run:

```sh
go run ./examples/approval
go test ./examples/approval -count=1
```

Tools with `RequiresApproval` are rejected when no approval callback is installed. Approval is not sandboxing: do not copy a shell or filesystem tool into production without an appropriate policy boundary.

## Coding assistant harness

The [harness application](harness/README.md) is a same-module satellite consuming the public kernel API. **Stage A only:** the terminal skeleton starts and exits; chat, tools, and durable sessions are not connected yet. Run `go run ./harness/cmd/go-agent --help`; non-TTY startup prints help and exits successfully. In a terminal, pass `--model YOUR_MODEL`, then press `q`, `Esc`, or `Ctrl+C` to exit.

Its [M1 contract](harness/DESIGN.md) covers Linux-first streaming chat, code/shell tools, approval, persistent sessions, cancel-and-redirect, and edit/write snapshots. MCP, agenttool, model switching, and workspace-wide snapshots are deferred. UI dependencies stay outside the stdlib-only kernel closure. `examples/agentcli` remains a demonstration; the application entry point is `harness/cmd/go-agent`.

## Examples

The gallery is ordered as a learning path. All tests are deterministic and make no network calls; entries marked **manual** require their stated external dependency.

| Need | Example | Coverage / run |
| --- | --- | --- |
| Start with streaming and one tool | [quickstart](examples/quickstart) | **manual**: set an API key, then `go run ./examples/quickstart` |
| Understand approval gates | [approval](examples/approval) | deterministic: `go test ./examples/approval -count=1` |
| Build an interactive, durable CLI | [agentcli](examples/agentcli) | **manual**: set an API key; supports `--resume` |
| Recover a persistent thread | [resume](examples/resume) | deterministic: `go test ./examples/resume -count=1` |
| Execute one response's tools concurrently | [parallel](examples/parallel) | deterministic: `go test ./examples/parallel -count=1` |
| Wrap providers or tools | [middleware](examples/middleware) | deterministic: `go test ./examples/middleware -count=1` |
| Test an agent without a network | [testdouble](examples/testdouble) | deterministic: `go test ./examples/testdouble -count=1` |
| Delegate to a child agent | [agenttool](examples/agenttool) | deterministic: `go test ./examples/agenttool -count=1` |
| Bridge MCP tools | [MCP bridge guide](mcp/README.md) | **manual/reference**: see its transport and lifecycle requirements |

Run the compile and behavior gate for the full gallery with `go test ./examples/... -count=1`.

## Agent as a tool

`agenttool.New` adapts a prebuilt child to a normal parent registry. The parent decides when to delegate and whether the outer call needs approval; the child keeps its own provider, registry, and options.

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

Each returned adapter serializes its own calls and invokes one stateless child `Run`. Separate adapters around the same child do not interlock. Child events, sessions, Store thread IDs, and history are not forwarded; malformed input, a truncated child run, and textless child output are soft tool results, while child `Run` and context errors remain hard errors.

## MCP bridge

The `mcpbridge` package is a client-only tools bridge. Create an official SDK transport, connect it into a fresh unpublished registry, and publish that registry only after success:

```go
registry := tool.NewRegistry()
bridge, err := mcpbridge.Connect(ctx, registry, transport, mcpbridge.Config{
    Namespace: "docs",
})
if err != nil { return err }
defer bridge.Close()
```

Read the [MCP bridge guide](mcp/README.md) before using it. In particular, cancel and join every in-flight agent call before `Close`; close concurrent with tool execution is unsupported.

## Production checklist

- Set the actual model, context-window size, and `WithMaxIter` explicitly.
- Choose Store thread ownership and retain thread IDs where recovery matters.
- Instrument provider errors, usage, retries, and events with your metrics/logging policy.
- Gate side-effecting tools with approval and honor context cancellation in every tool.
- Use `agenttest` scripts and tool doubles for deterministic tests.
- Review the public contracts in [docs/DESIGN.md](docs/DESIGN.md).

## Limitations

This runtime deliberately does not provide routing graphs, agent pools, shared child sessions, nested child-event forwarding, or automatic delegation. `agenttool` invokes one child `Run` per call.

The MCP bridge is client-only and tools-only. It has static discovery (no refresh or unregister), no retry/redial, per-tool timeout, shared session, server mode, resources, prompts, sampling, roots, logging, elicitation UI, OAuth policy, automatic installation, or network discovery. Input-required continuation is a soft unsupported result; non-text remote content is represented as placeholders plus live-event Data, while history and Store retain text only. Its full schema/result/approval boundaries are documented in [mcp/README.md](mcp/README.md).

## Project status and contributing

The project is released as [v0.1.0](https://github.com/dailz1/go-agent/releases/tag/v0.1.0) under the [MIT license](LICENSE). CI runs the verification battery and the kernel closure guard on every push to `main`. The API may still evolve before a separately planned v1.0; contributions should preserve the contracts in [docs/DESIGN.md](docs/DESIGN.md) and keep tests deterministic.
