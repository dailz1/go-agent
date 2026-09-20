# mcp/ (package `mcpbridge`)

## OVERVIEW
Client-only MCP bridge: discovers remote MCP tools over a caller-provided transport and registers them into a `tool.Registry` with full tools-line field coverage. The ONLY package allowed to import the MCP SDK (guard-enforced); the binding contract is mcp/README.md (namespace regex, approval defaults, data limits, frozen v1 non-goals).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Bridge entry | bridge.go:24 — `Connect(ctx, registry, transport, cfg)`; `Bridge` :16, `remoteTool` :155 |
| Options | options.go — `WithApprovalRequired` defaults TRUE (:21-23,:33, fail-closed), `WithResultDataLimit` default 1 MiB |
| Result projection | result.go:52-77 — per-entry Data budget; over-limit entries become `"[mcp:<type> omitted: ...]"` marker text, not errors |
| Schema projection | schema.go:66 — remote schemas decoded via SDK map[string]any, re-projected through tool.ParameterSchema.UnmarshalJSON |

## CONVENTIONS (differs from parent)
- Package name is `mcpbridge`, SDK is aliased `sdk` (bridge.go:1,10-11) — import path ends in /mcp, symbol differs.
- Connect preconditions: registry fresh / unpublished / quiescent; `Close` concurrent with `Execute` is unsupported (bridge.go:11-18).
- Namespace regex `^[A-Za-z0-9_-]{1,64}$` without `__` (bridge.go:26-28).
- Soft/hard split: mapper failures (result shaping) are soft; protocol/transport errors are hard (bridge.go:164-190). No retries in the bridge.

## ANTI-PATTERNS (THIS PACKAGE)
- No other package may import the MCP SDK — scripts/kernel-closure-guard.sh enforces the importer allowlist (incl. tests).
- Do not add retries; the bridge is zero-retry by contract.
- Do not promise discovery completeness: SDK v1.7.0 `ListTools` silently excludes tools with invalid `x-mcp-header` annotations (mcp/README.md:9); map-decode loses duplicate keys/order/numeric fidelity upstream.

## NOTES
- Dropped by design, documented in mcp/README.md: `_meta`, `OutputSchema`, `Icons`, content-level `annotations`, `progress` events surface only as opt-in callbacks.
- Discovery is static (one `ListTools` at Connect); removed remote tools are a hard protocol error — no refresh.
- MultiRoundTrip is forced Disabled; SDK v1.7.0 pinned byte-exact by scripts/kernel-closure-guard.sh.
| In-process test wiring | stdio_example_test.go — stdio transport example without network |
- `WithApprovalRequired(false)` is the only softening of the approval baseline — restricted to read-only, non-destructive remote tools (mcp/README.md).
