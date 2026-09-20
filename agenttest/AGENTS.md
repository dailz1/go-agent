# agenttest/

## OVERVIEW
Public deterministic test doubles for downstream consumers (the kernel's own in-package tests do NOT use this package): scripted provider, recording/replay of real provider exchanges, strict wire decoding.

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Scripted provider | scripted_provider.go:77 — `ScriptedProvider`, `NewScriptedProvider` :85, `Verify` :190; strict request matching |
| Record real provider | record.go:34 — `Recorder` (`NewRecorder` :42); wraps a real Provider, Name = "agenttest_recorder(<inner>)" :41 |
| Replay recordings | replay.go:17 — `Replayer` (`NewReplayer` :19); order-strict incl. stream position |
| Tool doubles | tool.go — ToolFunc/ToolCall recording |
| Wire decode | wire_decode.go:15-38 — hand-rolled strict decoder; version gate accepts 1|recordingVersion |

## CONVENTIONS (differs from parent)
- Recording grammar is VALUE-form only: pointer-form chunks are `UnsupportedChunkError`.
- Record/replay is order-strict — exchanges and stream positions must match exactly (record.go:89).
- Sentinels come as typed wrappers with `Unwrap` to the sentinel (record.go:17-29).

## ANTI-PATTERNS (THIS PACKAGE)
- Do not use `agent.MockProvider` outside package agent — its constructors sit over unexported types (agent/mock_provider.go:24-38); this package is the public seam.
- Do not loosen the wire decoder (struct-tag JSON decode of recordings) — token-strictness is the pinned contract (docs/DESIGN.md:67).

## NOTES
- Kernel in-package tests never import this package (white-box style with local helpers) — parity is asserted by agenttest's own agent_parity_test.go instead.
- Typical loop: record real exchanges once, commit the recording, replay in tests for deterministic runs; `Verify` fails on any request drift.
- Clone helpers (clone.go, clone_chunk_pointer.go, clone_pointer.go) deep-copy messages/chunks — the parity suite's shared fixtures.
- tool.go provides ToolFunc/ToolCall doubles with call recording; combine with ScriptedProvider for full deterministic runs.
- Recordings are plain bytes — commit them as fixtures and replay in CI-safe tests with zero network.
- Canonical end-to-end wiring: examples/testdouble (ScriptedProvider + ToolFunc, zero network).
