# go-agent

[简体中文](README.zh-CN.md)

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

## Optional tool and approval observation

The quickstart includes a simple echo tool. Ask explicitly for it, but treat model tool selection as best-effort manual observation. For deterministic allow/reject behavior, run:

```sh
go run ./examples/approval
go test ./examples/approval -count=1
```

Tools with `RequiresApproval` are rejected when no approval callback is installed. Approval is not sandboxing: do not copy a shell or filesystem tool into production without an appropriate policy boundary.

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

The project is pre-v0.1; breaking changes may occur before a separately planned stable release. There are no release, CI, license, or download badges because this repository does not currently publish those assets. Contributions should preserve the contracts in [docs/DESIGN.md](docs/DESIGN.md) and keep tests deterministic.
