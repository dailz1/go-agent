# pkg/agent

Agent core: streaming-first tool-calling loop over an `llm.Provider`, with history compaction, tool-result truncation, approval gating, and retry/fallback. Earned its file: score 8 (largest, most complex package; core domain).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Understand the loop | `runStreamInternal` (agent.go:304) - the single execution path; `Run` folds it, non-streaming providers are synthesized into chunks (`synthesizeChunks`) |
| Retry / streaming fallback | `chatWithRetryAndFallback` (agent.go:553); falls back to `Chat` on `llm.ErrStreamingNotSupported` |
| Multi-tool stream reassembly | accumulator.go - `toolCallAccum` keyed by stream `Index` |
| Compaction strategies | compact.go - standard, sliding-window, drop-oldest-tool-groups, summarization, plus `NewCompactorChain` |
| Tool-result truncation | truncate.go - head+tail with omission marker; panics on invalid rune limits by contract ("caller bug") |
| Events a stream consumer sees | event.go - 7 sealed `AgentEvent` variants (TextDelta, ThinkingDelta, ToolCall, ToolResult, Retry, Done, Compaction) + `RetryInfo` |
| Provider mocks | mock_provider.go - non-test file; base `NewMockProvider` takes unexported types, streaming/retryable variants take public types |
| Loop outcome | RunResult (agent.go:191): Message, History, ToolCalls, Truncated, Retries, Usage, TotalUsage |
| Retry behavior | `WithRetryConfig` / `AgentRetryConfig`: exponential backoff + full jitter honoring `APIError.RetryAfter`; `MaxRetries: -1` disables; defaults 3 / 500ms / 120s |
| Compaction trigger | `DefaultContextWindowTokens = 8_192`, `DefaultCompactionThresholdPercent = 80` (agent.go:130-134) |

## CONVENTIONS (differs from parent)
- Compaction failure is non-fatal: logged at Warn, the run continues with the original history.
- Unknown provider chunk types are silently ignored (forward compat); an unknown `AgentEvent` aborts with an error in `foldRunStream`.
- `ctx` is checked (`select`/`default`) before each LLM round and each tool execution.
- Sentinels `ErrInvalidHistory`, `ErrCompactionBudgetExceeded` (compact.go) - compare with `errors.Is`.
- Tool execution recovers panics and feeds a soft error result to the LLM.
- History is deep-copied via `deepCopyMessages`; do not mutate caller-owned slices.

## ANTI-PATTERNS (THIS PACKAGE)
- Do not retry mid-stream SSE errors here (or anywhere) - only pre-stream failures retry.
- Do not grow `runStreamInternal` into parallel paths for Run vs RunStream - everything folds through it.
- Do not add channels to public APIs; streaming is `iter.Seq2`.
