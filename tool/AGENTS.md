# tool

Tool contract: `Tool` interface, JSON-Schema-like `ParameterSchema`, soft-error `ToolResult`, thread-safe `Registry`. Earned its file: score 8, distinct domain - the project's two-tier error convention is defined here and referenced from ~20 places.

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Tool interface | tool.go:14 - `Info() ToolInfo` / `Execute(ctx, json.RawMessage) (*ToolResult, error)` |
| Soft vs hard failures | tool.go - tool-level failures return `*ToolResult` with `Status: ResultError` and are fed back to the LLM; Go errors are reserved for system failures that abort the loop |
| Result constructors | `NewTextResult` / `NewDataResult` / `NewErrorResult`; `NewParameterSchema` |
| Registry | registry.go - RWMutex-guarded, sorted `List`, schema validation on `Register`, `MustRegister` panics |
| Tool metadata | ToolInfo (tool.go:33): name, description, ParameterSchema, RequiresApproval flag (gated by agent's ApprovalFunc) |
| Schema builders | NewParameterSchema + Param / ParamEnum (tool.go) |
| Result status | ResultStatus: `ResultSuccess` / `ResultError`; NewErrorResult builds the soft-failure result |
| Lookup | Registry.Get by name; List returns a sorted copy |
| Result shape | `ToolResult{Content, Data, Status}` + `IsError()`; Data is JSON-marshaled at construction |
| Cancellation | Execute receives ctx; the agent checks ctx before each execution but cannot abort a running tool - honor ctx.Done() inside |
| Contract examples | tool_test.go - JSON round-trip, status semantics, schema-validation cases |

## CONVENTIONS (differs from parent)
- `NewDataResult` requires JSON-marshalable data (marshal errors surface at construction).
- Registry validation rejects: nil tools, empty names, duplicate names, `required` parameters missing from `properties`, empty property types.
- Raw-JSON tool args are passed unvalidated beyond the schema; tools own their arg parsing.
- Enum constraints are expressed with `ParamEnum`; property types must be non-empty.
- Registry is safe for concurrent Register/Get/List; thread-safety of a tool's own Execute state is the tool author's responsibility.

## ANTI-PATTERNS (THIS PACKAGE)
- Do not panic inside `Execute` for expected failures - recover happens upstream, but soft `ToolResult` errors are the contract for tool-level failures.
- Do not bypass `Registry` validation; registration-time schema errors are intentional fail-fast.
- Do not assume a failed Execute is retried - a returned Go error aborts the whole agent loop (retry applies to provider calls only).
