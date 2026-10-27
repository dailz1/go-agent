# Skills

`github.com/dailz1/go-agent/skills` is a stdlib-only satellite package in the
root module (it imports only the `tool` seam and the standard library). It
loads a skills directory — one subdirectory per skill, each containing a
`SKILL.md` — into an immutable `Set` and exposes a single distributor tool:
the tool's Description carries the catalog (name + description per skill,
name-sorted, rune-budgeted); `Execute({"name": ...})` returns that skill's
body as plain `ToolResult.Content`.

Contract:

- Scan: root must be an existing directory. Only immediate subdirectories
  are scanned, in sorted order; a subdirectory without `SKILL.md` is skipped
  and reported as an Issue; non-directory entries are ignored; a root-level
  `SKILL.md` is not a skill; no recursion.
- Frontmatter: a flat `key: value` subset between `---` fences. Recognized
  keys: `name` (optional — defaults to the directory name; must match
  `^[a-z0-9]+(-[a-z0-9]+)*$` and be ≤64 chars) and `description` (required;
  whitespace collapsed; truncated at 1024 runes with an ellipsis and an
  Issue). Unknown keys are ignored. Duplicate keys, block/folded scalars
  (`|`/`>`), malformed lines, or a missing closing fence invalidate the
  skill. Bodies are the verbatim remainder, trimmed; empty bodies are
  invalid. Files are capped at 1 MiB.
- Invalid skills never fail Load: they are skipped and recorded in
  `Set.Issues()`. Load fails only on configuration errors: unreadable root,
  invalid option values, or a catalog over budget.
- Catalog: the assembled Description (fixed header plus one `<skill>` entry
  per skill, name-sorted) must fit `WithCatalogLimit` runes (default 2048 —
  roughly 10–12 typical entries of name plus a ~120-rune description, fewer
  if descriptions run toward the 1024-rune per-entry cap; raise it via
  `WithCatalogLimit` for larger libraries); overflow is a Load error —
  prune skills or raise the limit. Duplicate effective names: the first
  subdirectory in sorted order wins; the shadowed skill is skipped and
  reported.
- Distributor: default tool name "skill" (`WithToolName`, validated against
  `^[A-Za-z0-9_-]{1,64}$`). Parameters: required string `name`. Malformed
  arguments and unknown names are soft `ToolResult` errors fed back to the
  model — never Go errors. A hit returns the body verbatim in Content; the
  Data field is always nil. `RequiresApproval` is false: Execute is a pure
  in-memory read; wrap the tool if you need gating.
- Kernel guardrails apply as-is: oversized bodies are truncated by the
  agent's tool-result guard (max(128, window×30%) runes, head+tail). Set a
  real context window and keep bodies short; put long reference material in
  additional files inside the skill directory and let the model read them
  through the host's file tools. Layer-3 reference reads are reachable only
  when the skills directory sits inside the host's file-tool root (in
  harness terms: the workspace root — its read/glob/grep tools are
  os.Root-confined); outside it, catalog and body delivery still work, but
  every additional-file read fails. Skill names never enter the Registry,
  so no namespace prefix is needed or provided.
- Mutation: a `Set` is immutable and loaded once per host process; edited
  skill files take effect after a host restart (no watcher in v1). To swap
  a catalog in a live shared registry, `Load` again and
  `registry.Replace(set.Tool())`: the new catalog broadcasts with the next
  run's snapshot (next round top with `agent.WithDynamicTools()`), while
  execution resolves per call against the live registry — a model holding
  the old catalog may still name entries the new Set dropped, which the
  distributor answers with a soft not-found. Registering an empty Set's
  tool is pointless; hosts skip registration when `Skills()` is empty.

Frozen v1 non-goals: allowed-tools permission trimming, slash-command
expansion, file watchers / hot reload / a skills-side Refresh API,
marketplaces, recursive or multi-root scanning, loading bundled non-SKILL.md
files, full YAML support (block scalars, lists, nesting), and an approval
option.
