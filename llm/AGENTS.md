# llm/

## OVERVIEW
Provider-agnostic LLM seam: Provider iface, Message/ContentBlock/Chunk sealed unions, shared HTTP+SSE+retry plumbing; three adapter subpackages (glm/, openai/, openairesponses/).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Provider contract | provider.go:19 — Name/Chat/ChatStream; `ErrStreamingNotSupported` sentinel; lazy `iter.Seq2` (:23-33) |
| Message model | message.go:22 — Role + sealed []ContentBlock :27 (Text/Reasoning/Image/ToolUse/ReasoningItem/ToolResult); OpenAI string-content compatible; constructors :93-:126 |
| Stream chunks | chunk.go:20 — 6 sealed variants (TextDelta..DoneChunk) |
| Retry + errors | retry.go, errors.go:9 — `APIError` leaf (no Unwrap); `Retryable()` = 429/5xx :21 |
| HTTP/SSE plumbing | http.go (10MB body cap :20), sse.go (1MB line cap :29) |

## CONVENTIONS (differs from parent)
- Adapters: `openai.NewProvider` adapter.go:50, `glm.NewProvider` :104 (of llm/glm/adapter.go), `openairesponses.NewProvider` :56 — all satisfy Provider.
- glm: API key containing `.` selects self-signed JWT auth (adapter.go:105-108) — bearer key with a dot WILL misauthenticate; `WithTopP` is the only option that clamps in place (:77-86); `timeNow` seam (auth.go:13-14).
- openai: lazy-stream contract — request errors surface only when ranging begins (adapter.go:158-165); `StreamOptions.IncludeUsage` (:174-175).
- openairesponses: typed items, store:false + full item-history replay per call, reasoning via encrypted_content; v1 exclusions documented in adapter.go:1-16 (no built-in tools, text.format, previous_response_id, WS).
- Bounded I/O + 120s default client timeout in all three adapters.

## ANTI-PATTERNS (THIS PACKAGE)
- Conversion helpers (`convert*`, `firstNonEmpty`, `ptrToString`, `encodeBase64`, `parseStreamPayload`) are duplicated near-verbatim in glm/ and openai/ BY DESIGN — change both, never extract a shared helper.
- Do not pre-range a ChatStream in wrappers — lazy contract breaks (docs/DESIGN.md:61).
- Do not implement the sealed interfaces outside their defining package.
- Do not wrap/retry mid-stream SSE errors.

## NOTES
- Message constructors live at message.go:93-:126 (Text/System/User/etc.) — build histories with these, not struct literals.
- The e2e file llm/openairesponses/e2e_test.go is `//go:build e2e`-gated and invisible to plain `go test ./...`.
- Every adapter pins a 120s default HTTP client timeout; overrides flow through llm.Option.
