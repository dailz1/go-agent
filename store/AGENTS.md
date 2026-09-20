# store/

## OVERVIEW
Thread persistence for the agent loop: append-only log is the source of truth; checkpoints are rebuildable accelerators. Two backends: JSONL on disk (crash-safe) and in-memory.

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Store contract | store.go:104 — `Store` iface: Append/Latest/History/Delete; `Append` takes expected seq (optimistic concurrency) |
| Record envelope | store.go:74 — versioned `{Seq, Kind, Schema, ID, RecordedAt, Payload}`; Checkpoint :86, ThreadState :91 |
| Shared append logic | planAppend store.go:171 + prepareBatch store.go:144 — both backends route through these so semantics cannot diverge |
| JSONL backend | jsonl.go:46 — `NewJSONL` :69; every acknowledged append is fsynced (:28); torn final line repaired by truncation, interior corruption → `ErrCorruptLog` (store.go:56-59) |
| Memory backend | memory.go:12 — `NewMemory` :18 |
| Test seam | `timeNow` package var (store.go:245) |

## CONVENTIONS (differs from parent)
- Threads are implicit: IDs are caller-owned, never enumerated by the store (store.go:11-13).
- Append is idempotent for ambiguous retries: the applied prefix must match the full caller content (ID/Kind/Schema/Payload), else `ErrRevisionConflict`.
- One live JSONL store per physical directory (jsonl.go:15,38,65-66).
- Replay contract lives in agent/persist_replay.go (kernel side); this package only stores/returns records.

## ANTI-PATTERNS (THIS PACKAGE)
- Do not treat checkpoints as truth — they are rebuildable accelerators; the log is the source of truth (store.go:8-10).
- Do not add backend-specific append semantics — extend planAppend/prepareBatch so both backends stay identical.
- Do not fsync-skip or batch-acknowledge in new backends without preserving the acknowledged-append durability guarantee.

## NOTES
- Sentinels: `ErrRevisionConflict` (optimistic-concurrency mismatch), `ErrCorruptLog` (interior corruption), `ErrUnknownThread` on missing History reads.
- Records carry `Schema` per kind so consumers can gate on version before decoding payloads.
- Agent-side append protocol (declareRound/commit/checkpoint ordering) is defined in agent/persist.go — this package never composes rounds itself.
