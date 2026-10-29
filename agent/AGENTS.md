# pkg/agent → agent/

## OVERVIEW
Core autonomous loop: chat → tool calls → results → chat, streaming `AgentEvent`s, with compaction, truncation, approval, retry/fallback, durable threads, runtime control plane (steering queue, per-thread config hot-update, FinishRound), and cross-provider hot-swap. Largest, most complex package (~5.1k source vs ~17.4k test LOC).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Understand the loop | `runStreamInternal` agent.go:388 — single execution path; Run :335 folds it, RunStream :384; `RunStreamWithHistory` :879 (resume-style entry) |
| Provider hot-swap | config.go (`ConfigChange{Model,Thinking,Provider}` :37, `SetThreadConfig` :59, sentinels `ErrUnknownProvider`/`ErrInvalidConfigChange`) + `WithProviders` agent.go:227; resolution at round top after refreshConfig (agent.go:417-433), fail-closed on unknown name; pair switch enforced write+decode side (persist_dto.go `decodeConfigChanged` → ErrIncompatibleLog) |
| Retry / provider call seam | provider_call.go:13 — `chatWithRetryAndFallback(ctx, history, tools, request, opts, yield, retries, prov)`; ONLY provider touchpoints (ChatStream :38, fallback Chat :46) |
| Steering queue | steering.go — `EnqueueInputWithID` :70 (durable FIFO, idempotency key, admission bound); drained post-compaction each round |
| FinishRound hook | finishround.go — per-round scheduling decision (Default/Continue/End) before terminal state |
| Round context | round_context.go — `RoundContextProvider` :19, called per round with `RoundContextRequest{Round,ThreadID}` |
| Stream reassembly | accumulator.go — `toolCallAccum` keyed by stream Index |
| maxIter terminal skip (C10) | truncation branch agent.go:471-517; skip helper max_iter.go:8 (`iterationLimitResults`); Store side `truncateRun` persist.go:210 |
| Events a consumer sees | event.go — 7 sealed variants; TC-first semantics :36-44; DoneEvent :97 (History field :102 is the authoritative snapshot) |
| Compaction strategies | compact.go — standard, sliding-window, drop-oldest-tool-groups, summarization, `NewCompactorChain`; `Compactor.Compact(ctx, history, budget, provider llm.Provider, opts []llm.Option)` compact.go:50 — summarization follows the thread's round provider + effective options (`NewSummarizationCompactor()` :484 takes no provider) |
| Tool-result truncation | truncate.go:17 — head+tail with omission marker; panics on invalid rune limits ("caller bug") |
| Durable threads | persist_api.go:14/32/63/105 (RunOption/WithThread/runSequence/ResumeThread); settle.go:58 (`SettleThread`); persist.go (declareRound/commit/absorbTail); persist_dto.go (record codecs, `configState` folding — provider+model pair enforced); persist_replay.go |
| Cross-provider degradation | adapters own it at outbound conversion (DESIGN.md frozen table): glm strips all reasoning blocks; openai passes CC `ReasoningBlock`, skips `ReasoningItemBlock`; openairesponses/codex replay native rs_ items, drop foreign; codex hard-errors on zero-arg/foreign tool_use — pinned in each adapter's `cross_provider_degrade_test.go` + agent/provider_hotswap_test.go |
| Provider mocks | mock_provider.go — exported constructors over UNEXPORTED types; external packages use agenttest instead |
| Options | agent.go — constructor options normalized in New :278 (maxIter<1→10, window<=0→8192, concurrency<1→1); `WithProviders` :227 appends alternates |

## CONVENTIONS (differs from parent)
- TC-first: all ToolCallEvents of a round yield before any executes; the announcement batch has NO context check (splitting strands a partially-announced assistant) — agent.go:526-543.
- maxIter terminal round: every requested call is announced then given a deterministic skipped `ToolResultEvent` (IsError, "iteration limit"); zero execution, zero approval; Store closes the batch first, break cannot reopen (event.go:41-44).
- Durable-before-visible: declareRound before announce/execute/approval; compaction checkpoint before CompactionEvent; terminal state before DoneEvent.
- Fold contract: initial input + fold(events) = DoneEvent.History; DoneEvent.History is authoritative (ReasoningItemChunk emits no event); stream ending without Done = error.
- Compaction failure is non-fatal: Warn log, run continues with original history. Unknown provider chunks silently ignored; unknown AgentEvent aborts the fold.
- ctx checked before each LLM round and each tool execution; history deep-copied (`deepCopyMessages`), never mutate caller slices.
- Sentinels: `ErrInvalidHistory`, `ErrCompactionBudgetExceeded`, `ErrUnknownProvider`, `ErrInvalidConfigChange`; persistence: `ErrRunIncomplete` (choose ResumeThread or explicit SettleThread with the original token), `ErrNothingToResume`, `ErrNothingToSettle`; config: `ErrEmptyConfig` (all three fields nil).
- Config hot-update: one channel `SetThreadConfig` → `config_changed` record; provider+model switch atomically per record (provider without model rejected write+decode side); takes effect next round top, never mid-request; unswitched threads byte-identical (Provider field omitempty).

## ANTI-PATTERNS (THIS PACKAGE)
- Do not retry mid-stream SSE errors anywhere — only pre-stream failures retry.
- Do not grow `runStreamInternal` into parallel Run-vs-RunStream paths — everything folds through it.
- Do not add channels to public APIs; streaming is `iter.Seq2`.
- Do not emit events for ReasoningItemChunk (index-ordered assembly only; consumers read DoneEvent.History).
