# tool/

## OVERVIEW
Tool seam: Tool interface + soft-error ToolResult, thread-safe Registry, recursive JSON Schema model with a semantic-fidelity codec (the schema never validates arguments — it carries them faithfully to the model).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Tool contract | tool.go:14 — `Info()` + `Execute()`; `ToolInfo` :33, `ToolResult` :93 |
| Soft vs hard errors | tool.go:25-35 — `ToolResult{Status: ResultError}` is fed back to the LLM; a Go error aborts the loop; `NewErrorResult` :101 |
| Registry | registry.go:10 — `NewRegistry` :15, `Register` :21 (validates schema, errors on duplicate), `Get`/`List` :52/:59 (sorted), `MustRegister` :73 (panics) |
| Parameter schema | tool.go:49 — recursive model; `Property` :66 (`Items *Property`, unexported presence flags :55,:73) |
| Schema codec | schema_unmarshal.go:9 / schema_marshal.go:10 — semantic-fidelity round-trip, ordered keyword carriers |
| Schema validation | schema_validate.go — composition cycles fail closed (:59,:157) |

## CONVENTIONS (differs from parent)
- Registry is `sync.RWMutex` (registry.go:11) — safe to mutate while an agent runs.
- Schema codec preserves typed recursive fields + ordered `SchemaKeyword` carriers; JSON null normalization is null-first; marshal/unmarshal share an active-stack cycle detector (encoding/json cannot protect custom MarshalJSON).
- External construction semantics: pointer non-nil = present, Ref non-empty = present, empty Ref = absent (validated uniformly for all externally constructible values).

## ANTI-PATTERNS (THIS PACKAGE)
- Do not report tool failures as Go errors — soft `ToolResult{Status: ResultError}` only; Go errors abort the agent loop.
- Do not bypass Registry validation by constructing schemas ad hoc — use the codec; cycles must fail closed, never hang.

## NOTES
- Schema validation runs at `Registry.Register` time; nothing in-repo validates call arguments against the schema — the model's args are passed through verbatim.
- Keyword carriers keep encounter order for keywords outside the typed subset; order is part of fidelity.
- `NewErrorResult` (tool.go:101) is the canonical soft-failure constructor — never hand-roll Status strings.
- `ToolResult` (tool.go:93) carries Content plus optional structured Data; Data marshals inside the codec's cycle-checked path.
- Validation happens at Register time only; call arguments are never schema-validated in-repo.
