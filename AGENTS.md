# PROJECT KNOWLEDGE BASE

**Generated:** 2026-09-20
**Commit:** f064c63
**Branch:** main

## OVERVIEW
Embeddable Go agent runtime kernel ("net/http of Go agents"): streaming-first tool-calling loop (agent/) over pluggable providers (llm/), durable threads (store/), tool registry + recursive JSON Schema (tool/). Satellites: agenttest/ (public test doubles), mcp/ (MCP client bridge), agenttool/ (agent-as-tool), examples/. Module `github.com/dailz1/go-agent`, Go 1.26.1.

## STRUCTURE
```
./
├── docs/DESIGN.md        # design contract (Chinese) — interfaces change here FIRST
├── agent/                # loop, events, compaction, truncation, persistence, C10 terminal skip
├── llm/                  # Provider iface, Message/Chunk, HTTP+SSE, retry; adapters glm/ openai/ openairesponses/
├── store/                # thread persistence: append-only log, checkpoints, JSONL + memory backends
├── tool/                 # Tool iface, soft-error ToolResult, Registry, recursive ParameterSchema
├── agenttest/            # public deterministic test doubles (scripted provider, record/replay)
├── agenttool/            # wraps a child Agent as a Tool
├── mcp/                  # MCP client bridge — package name is mcpbridge; only SDK importer
├── examples/             # 8 runnable mains (quickstart, agentcli, resume, parallel, ...)
└── scripts/              # CI guards: kernel-closure-guard.sh, check-readme-contract.sh
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Understand the loop | `runStreamInternal` agent/agent.go:347 | single execution path; `Run` :282 folds it, `RunStream` :335 |
| Add a provider adapter | llm/glm or llm/openai | copy one; update BOTH (duplication is by design) |
| Change Message/Chunk/Event types | docs/DESIGN.md, then llm/ + agent/ | docs-first; sealed interfaces |
| Add a tool | tool/tool.go:14 + tool/registry.go:10 | Register validates schema |
| Persist / resume threads | agent/persist_api.go:19,40,55 + store/ | RunThread/ResumeThread/RunThreadStream |
| Tune context | agent/compact.go, agent/truncate.go | compaction at 80% of window; tool results ~30% |
| Test doubles | agenttest/ | scripted provider, record/replay — NOT agent.MockProvider (unexported types) |
| MCP bridge | mcp/bridge.go:24 + mcp/README.md | package `mcpbridge`; tools-line only |
| Agent-as-tool | agenttool/agenttool.go:42 | stateless child Run per call |
| Real-API integration test | agent/agent_e2e_test.go | `//go:build e2e`; needs OPENAI_API_KEY |

## CODE MAP
| Symbol | Type | Location | Role |
|--------|------|----------|------|
| agent.Agent / agent.New | type/func | agent/agent.go:110 / :243 | consumer entry; options normalized in New |
| runStreamInternal | method | agent/agent.go:347 | the loop; Run/RunStream both fold it |
| AgentEvent (7 sealed variants) | iface | agent/event.go:15 | TC-first announcements; DoneEvent :97 authoritative |
| iterationLimitResults | func | agent/max_iter.go:8 | C10: maxIter terminal skip results |
| RunThread/ResumeThread | methods | agent/persist_api.go:19/:40 | durable threads over store.Store |
| store.Store / store.Record | iface/type | store/store.go:104 / :74 | append-only log; checkpoints are accelerators |
| tool.Tool / Registry | iface/type | tool/tool.go:14 / tool/registry.go:10 | soft-error contract; RWMutex registry |
| tool.ParameterSchema | type | tool/tool.go:49 | recursive JSON Schema, semantic-fidelity codec |
| llm.Provider / Message / Chunk | ifaces | llm/provider.go:19 / llm/message.go:22 / llm/chunk.go:20 | provider seam; sealed unions |
| llm.APIError | type | llm/errors.go:9 | leaf error; Retryable() drives retry |
| mcpbridge.Connect | func | mcp/bridge.go:24 | remote MCP tools → Registry |
| agenttest.ScriptedProvider | type | agenttest/scripted_provider.go:77 | deterministic public test double |

## CONVENTIONS
- Errors: `fmt.Errorf("lowercase context: %w", err)`; sentinels via `errors.Is`; `*llm.APIError` via `errors.As` — leaf, no Unwrap.
- Functional options at 3 levels; value normalization lives in constructors (`agent.New`), sole exception glm `WithTopP` clamps in place.
- Sealed interfaces via unexported marker methods; type switches must handle value AND pointer forms.
- Streaming is `iter.Seq2[T, error]`; no channels in public APIs; early-break safe.
- Tests: stdlib `testing` only, table-driven, white-box in-package; test seams are package vars (`sleepFor`, `timeNow`).
- Docs-first: docs/DESIGN.md (Chinese) is the interface contract of record.

## ANTI-PATTERNS (THIS PROJECT)
- ANY third-party import inside kernel closure (agent, llm, store, tool) — guarded by scripts/kernel-closure-guard.sh; `go list -m all` = 14 lines expected (SDK lives only in mcp/).
- Retrying mid-stream SSE errors — only pre-stream failures retry (chatWithRetryAndFallback).
- Reporting tool failures as Go errors — soft failures are `ToolResult{Status: ResultError}` fed back to the LLM.
- Deduplicating glm/openai conversion helpers — duplicated by design; change both.
- `pkg/` directory — packages were promoted to module root; do not reintroduce.
- `init()` functions or env reads in library code — env only in e2e test and examples.
- Approval fail-closed: a RequiresApproval tool with no callback is rejected, never executed.

## UNIQUE STYLES
- Suffix taxonomy: `*Block` (content) `*Chunk` (stream delta) `*Event` (agent) `*Result` (tool) `*Config`, with parallel constructor verbs.
- Dependency shape is the product: satellites depend on kernel; kernel depends on nothing third-party.
- Bilingual mirrored READMEs (README.md / README.zh-CN.md) enforced by scripts/check-readme-contract.sh.

## COMMANDS
```bash
go build ./... && go vet ./...
go test -count=1 ./...                      # unit only, no network (16 packages)
go test -race -shuffle=on -count=1 ./agent ./store
gofmt -l .                                  # must print nothing
bash scripts/kernel-closure-guard.sh        # kernel closure + SDK pin v1.7.0
bash scripts/check-readme-contract.sh       # bilingual README contract
go list -m all                              # expect 14 lines (module + SDK + transitives)
go test -tags e2e ./...                     # real API; OPENAI_API_KEY required
```

## NOTES
- Provenance: standalone extraction of holmes-go's core pkg/ subtree; pkg/ itself was promoted to root (2026-09-20, `1b1932b`).
- DoneEvent.History is the authoritative history snapshot; ReasoningItemChunk emits NO event (read reasoning from DoneEvent.History).
- GLM auth sniffing: an API key containing `.` selects JWT mode — a bearer key with a dot will misauthenticate.
- Bounded I/O: response bodies 10MB, SSE lines 1MB, HTTP timeout 120s, MCP result envelope 1 MiB.
- `dir mcp` holds package `mcpbridge` — import path ends /mcp, symbol name differs.
- Subdirectory AGENTS.md: agent/, llm/, store/, tool/, agenttest/, mcp/. Anchors were grep-verified at f064c63; re-verify after structural refactors (they drift).
