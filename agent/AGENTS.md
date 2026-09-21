# pkg/agent → agent/

## OVERVIEW
Core autonomous loop: chat → tool calls → results → chat, streaming `AgentEvent`s, with compaction, truncation, approval, retry/fallback, and durable threads. Earned its file: largest, most complex package (~3.4k source vs ~11.5k test LOC).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Understand the loop | `runStreamInternal` agent.go:347 — single execution path; Run :282 folds it (`foldRunStream` :298), RunStream :335 |
| Retry / stream fallback | `chatWithRetryAndFallback` agent.go:641; falls back to Chat on `llm.ErrStreamingNotSupported` |
| Stream reassembly | accumulator.go — `toolCallAccum` keyed by stream Index |
| maxIter terminal skip (C10) | truncation branch agent.go:471-517; skip helper max_iter.go:8 (`iterationLimitResults`); Store side `truncateRun` persist.go:210 |
| Events a consumer sees | event.go — 7 sealed variants; TC-first semantics :36-44; DoneEvent :97 (History field :102 is the authoritative snapshot) |
| Compaction strategies | compact.go — standard, sliding-window, drop-oldest-tool-groups, summarization, `NewCompactorChain` (`Compactor` iface compact.go:43) |
| Tool-result truncation | truncate.go:17 — head+tail with omission marker; panics on invalid rune limits ("caller bug") |
| Durable threads | persist_api.go:19/40/55 (RunThread/ResumeThread/RunThreadStream); persist.go (declareRound/commit/truncateRun); persist_replay.go |
| Provider mocks | mock_provider.go — exported constructors over UNEXPORTED types; external packages use agenttest instead |
| Options | agent.go:152-214 — normalized in New :243 (maxIter<1→10, window<=0→8192, concurrency<1→1); defaults :142/:146 |

## CONVENTIONS (differs from parent)
- TC-first: all ToolCallEvents of a round yield before any executes; the announcement batch has NO context check (splitting strands a partially-announced assistant) — agent.go:526-543.
- maxIter terminal round: every requested call is announced then given a deterministic skipped `ToolResultEvent` (IsError, "iteration limit"); zero execution, zero approval; Store closes the batch first, break cannot reopen (event.go:41-44).
- Durable-before-visible: declareRound before announce/execute/approval; compaction checkpoint before CompactionEvent; terminal state before DoneEvent.
- Fold contract: initial input + fold(events) = DoneEvent.History; DoneEvent.History is authoritative (ReasoningItemChunk emits no event); stream ending without Done = error.
- Compaction failure is non-fatal: Warn log, run continues with original history. Unknown provider chunks silently ignored; unknown AgentEvent aborts the fold.
- ctx checked before each LLM round and each tool execution; history deep-copied (`deepCopyMessages`), never mutate caller slices.
- Sentinels: `ErrInvalidHistory`, `ErrCompactionBudgetExceeded`; persistence: `ErrRunIncomplete` (choose ResumeThread or explicit SettleThread with the original token), `ErrNothingToResume`, `ErrNothingToSettle`.

## ANTI-PATTERNS (THIS PACKAGE)
- Do not retry mid-stream SSE errors anywhere — only pre-stream failures retry.
- Do not grow `runStreamInternal` into parallel Run-vs-RunStream paths — everything folds through it.
- Do not add channels to public APIs; streaming is `iter.Seq2`.
- Do not emit events for ReasoningItemChunk (index-ordered assembly only; consumers read DoneEvent.History).
