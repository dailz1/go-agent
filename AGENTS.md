# PROJECT KNOWLEDGE BASE

**Generated:** 2026-09-10
**Commit:** 6b546f0b03
**Branch:** main

## OVERVIEW
go-agent is a zero-dependency Go library for building LLM agents: a streaming-first tool-calling loop (`pkg/agent`) over pluggable provider adapters (`pkg/llm` with `glm`/`openai`) and a thread-safe tool registry (`pkg/tool`). No `package main` - library only. Layering: `agent -> llm -> tool`; adapters import `llm`+`tool` and are consumer-selected (nothing in-repo imports `glm`; `openai` only from the e2e test).

## STRUCTURE
```
./
├── docs/DESIGN.md  # design contract (Chinese): interfaces change here FIRST; stdlib-only rule
├── pkg/agent/      # agent loop, stream fold, compaction, truncation, events, provider mocks
├── pkg/llm/        # Provider iface, Message/Chunk model, HTTP+SSE, retry, APIError
│   ├── glm/        # GLM adapter: JWT/bearer auth sniffing, eager stream request
│   └── openai/     # OpenAI-compatible adapter: lazy stream, include_usage
└── pkg/tool/       # Tool iface, soft-error ToolResult, RWMutex Registry
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Understand the loop | pkg/agent `runStreamInternal` (agent.go:304) | single execution path; `Run` folds the stream |
| Add a provider adapter | pkg/llm/glm or pkg/llm/openai | copy an adapter; update BOTH (see pkg/llm/AGENTS.md) |
| Change Message/Chunk/Event types | docs/DESIGN.md, then pkg/llm | sealed interfaces; DESIGN.md is doc of record |
| Add a tool | pkg/tool/tool.go | implement `Tool`, register via `Registry` |
| Tune context management | pkg/agent compact.go + truncate.go | compaction at 80% of window; tool results ~30% |
| Real-API integration test | pkg/agent/agent_e2e_test.go | `//go:build e2e`; needs OPENAI_API_KEY |
| Consumer entry sequence | tool.NewRegistry -> provider.NewProvider -> agent.New -> Agent.Run/RunStream | no cmd/, no examples |

## CODE MAP
| Symbol | Type | Location | Refs | Role |
|--------|------|----------|------|------|
| llm.Provider | interface | pkg/llm/provider.go:19 | agent + both adapters | Name/Chat/ChatStream; ErrStreamingNotSupported sentinel |
| llm.Message / ContentBlock | struct / sealed iface | pkg/llm/message.go | highest traffic | custom UnmarshalJSON (string/array/null) |
| llm.Chunk | sealed iface | pkg/llm/chunk.go | heavy in pkg/agent | 5 stream variants |
| agent.Agent | struct | pkg/agent/agent.go:100 | consumer entry | loop, approval, retry, compaction wiring |
| agent.New | func | pkg/agent/agent.go:211 | | central normalization of all options |
| agent.Compactor | interface | pkg/agent/compact.go:43 | | 4 strategies + chain |
| tool.Tool / tool.Registry | iface / struct | pkg/tool/tool.go:14, registry.go | ~20 | soft-error contract; thread-safe |
| llm.APIError | struct | pkg/llm/errors.go | retry path | StatusCode/RetryAfter/Retryable(); leaf, no Unwrap |

## CONVENTIONS
- Errors: `fmt.Errorf("context: %w", err)` with lowercase context prefix; sentinels compared with `errors.Is`; `*llm.APIError` extracted with `errors.As`. APIError is a leaf (no Unwrap).
- Functional options at 3 levels (`agent.Option`, `llm.Option`, per-provider `ProviderOption`); value normalization lives in constructors (`agent.New`, `NewProvider`), not option functions - sole exception: glm's `WithTopP` clamps in place; zero-vs-unset options use explicit set-flags (`retryCfgSet`).
- Sealed interfaces via unexported marker methods; handle BOTH value and pointer block forms (see `deepCopyMessages`).
- Streaming is `iter.Seq2[T, error]`; no channels in public APIs; early-break safe via `Cleanup` (from `sync.OnceFunc`).
- Tests: stdlib `testing` only (no testify), table-driven, `t.Parallel` widespread but not universal, white-box in-package; hand-written mocks. Test-heavy: ~12.9k test LOC vs ~4.5k source LOC.
- Test seams are package vars: `sleepFor` (pkg/llm/retry.go), `timeNow` (pkg/llm/glm/auth.go).

## ANTI-PATTERNS (THIS PROJECT)
- Adding ANY third-party dependency - violates the stdlib-only design contract (`go list -m all` must print a single line).
- Retrying mid-stream SSE errors - only pre-stream connection failures retry (`chatWithRetryAndFallback`).
- Reporting tool failures as Go errors - soft failures are `ToolResult{Status: ResultError}` fed back to the LLM; Go errors abort the loop.
- Editing conversion helpers in one adapter only - glm/ and openai/ duplicate them intentionally; change both.
- Reading env vars or dialing the network from library code - no `init()` functions exist; env access only in the e2e test.

## UNIQUE STYLES
- Suffix taxonomy: `*Block` (content), `*Chunk` (stream delta), `*Event` (agent), `*Result` (tool), `*Config` - with parallel constructor verbs.
- docs/DESIGN.md (Chinese) is authoritative for interfaces; change the doc first, code second.

## COMMANDS
```bash
go build ./... && go vet ./...
go test ./...                 # unit only, no network
go test -tags e2e ./...       # real OpenAI API; OPENAI_API_KEY required; OPENAI_MODEL / OPENAI_BASE_URL optional
go test -cover ./...
go list -m all                # stdlib-only guard: must print one line
gofmt -l .
```

## NOTES
- Provenance: this repo is a standalone snapshot of holmes-go's core `pkg/` subtree (extracted into its own module); DESIGN.md and code comments still carry that lineage.
- go.mod requires Go 1.26.1 (`iter.Seq2`, `math/rand/v2`, `sync.OnceFunc`); no go.sum.
- Defaults: context window 8192 tokens unless `WithContextWindowTokens`; compaction triggers at 80%; tool results truncated to ~30% of window (min 128 runes, head+tail kept).
- GLM auth sniffing: an API key containing `.` selects JWT mode - a bearer key containing a dot will misauthenticate.
- `estimateRunes` JSON-marshals the whole history per call (heuristic, known O(n^2)-ish on long histories; acknowledged in DESIGN.md).
- Bounded I/O: response bodies capped at 10MB, SSE lines at 1MB, default HTTP client timeout 120s.
- Approval fails closed: a `RequiresApproval` tool with no callback set is rejected and the rejection is fed to the LLM.
- `mock_provider.go` ships exported mocks in a non-test file; the base `NewMockProvider` takes unexported types (unusable outside `pkg/agent`), while the streaming/retryable constructors take public types. A public test-double package is a DESIGN.md roadmap item.
