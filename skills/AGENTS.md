# skills/ (package `skills`)

## OVERVIEW
Skill-directory loader satellite: scans a one-level skill directory, parses SKILL.md files with an agentskills.io-aligned YAML-subset scanner (stdlib only, no yaml dependency), and produces one immutable `Set` per `Load`. `Set.Tool()` returns a distributor Tool whose Description carries the name-sorted catalog and whose `Execute` returns a named skill body verbatim. Imports only stdlib + `github.com/dailz1/go-agent/tool`; kernel diff zero. Binding contract: skills/README.md.

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Load / options / Set | skills.go — `Load(root, opts...)`, `WithToolName` (default `skill`, regex `^[A-Za-z0-9_-]{1,64}$`), `WithCatalogLimit` (default 2048 runes) |
| One-level scan + bounded read | scan.go — 1 MiB `io.LimitReader` cap; directory symlinks NOT followed (lstat-based `IsDir`) |
| Frontmatter subset | parse.go — indent-aware line scanner: recognized keys (name, description) accept plain scalars with folded continuations, same-line quoted scalars with escapes, and block scalars (`|`/`>`); unknown keys skip with their whole indented block; duplicate recognized keys, `|`/`>` gone wrong, malformed/blank-tolerant lines, missing fence, empty body rules per README; BOM strip + CRLF normalize; description required, whitespace-collapsed, 1024-rune truncate-with-issue; explicit name ≠ directory loads with a warning Issue |
| Catalog assembly | catalog.go — header + name-sorted entries; overflow of the total limit is a Load HARD error (counts + limit + remedy in message) |
| Distributor Tool | distributor.go — body verbatim, `Data` nil, unknown name/malformed args → soft `NewErrorResult` (zero Go error), `RequiresApproval=false` fixed |

## CONVENTIONS (differs from parent)
- Failure split: hard errors only at Load boundary (unreadable root, invalid options, catalog over limit); everything per-skill is skip-with-collect `Issues()` — Load writes no shared state, unlike mcp Connect's fail-closed-rollback.
- Collision rule: `os.ReadDir` order (name-sorted); first subdir wins, loser recorded as an Issue.
- Set is immutable after Load; `Skills()`/`Issues()` return sorted clones. No unregister/refresh API — refresh = re-`Load` + host `registry.Replace` (P2-4).

## ANTI-PATTERNS (THIS PACKAGE)
- Do not add a yaml dependency — the recognized-key subset is two fields by design; the escalation trigger to introduce one is written in skills/README.md (parsed unknown-field content).
- Do not return Go errors from `Tool.Execute` — soft `ToolResult` only.
- Do not swallow the catalog-limit hard error with silent truncation — a silently dropped entry is a permanently undiscoverable skill.

## NOTES
- Non-goals frozen in skills/README.md: allowed-tools permission trimming, slash-command expansion, watcher/Refresh, marketplace, nested scan, bundled-file loading, parsing unknown fields' nested content (skipped whole), approval option.
- Catalog Description tokens are invisible to the kernel's truncation/budget accounting (history-rune only) — keep catalogs small; default fits ~10-12 entries.
- Host precondition: place SkillsDir inside the harness workspace root when layer-3 reference files must be readable by the host's file tools (os.Root confinement); the package itself is root-agnostic.
- Refresh semantics are two-layer: broadcast = run-start snapshot (or per-iteration under `WithDynamicTools`), execution = per-call live `registry.Get`; mid-run Replace soft-errors old-catalog names via the distributor.
