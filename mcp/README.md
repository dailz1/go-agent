# MCP bridge

`github.com/dailz1/go-agent/mcp` is a `mcpbridge` package in the root module. The official MCP SDK is compiled only into binaries that import this package. It is a client-only, tools-only bridge: callers create an official MCP transport, connect into a fresh unpublished `tool.Registry`, and publish that registry only after `Connect` succeeds.

`Config.Namespace` and every final `${namespace}__${remote}` name must match `^[A-Za-z0-9_-]{1,64}$`; namespaces cannot contain `__`. Discovery is static: no refresh or unregister occurs, and a removed remote tool produces a hard protocol error. Multi-round-trip handling is disabled, so input-required results are one soft `v1 不支持 MCP input-required continuation` result with no retry.

Approval defaults to required for every bridge tool. `WithApprovalRequired(false)` relaxes only tools whose annotations explicitly say read-only and do not say destructive; all other tools remain approval-gated. Input schemas are projected from the SDK-decoded map through `ParameterSchema.UnmarshalJSON`; full vocabulary is typed-or-carried in name-sorted form, while duplicate keys, encounter order, and precise numeric tokens are outside the SDK decode boundary. Provider acceptance is independent from schema representability.

**SDK v1.7.0 discovery boundary:** `ClientSession.ListTools` logs and silently excludes tools with an invalid `x-mcp-header`: the annotation is on a non-string/integer/boolean property, has a non-string/empty/invalid HTTP field-name value, or duplicates a case-insensitive header value at any nesting. The bridge cannot observe or reject those rows. Server authors must fix the annotation; a future SDK option is the only v1 escape.

Text content joins with newlines. Image, audio, resource-link, and embedded-resource content gets an escaped typed Content placeholder and an indexed JSON Data envelope. `WithResultDataLimit` defaults to 1 MiB, rejects non-positive values at Connect, and checks the final cumulative encoded envelope before accepting each entry. Data is available only to live `ToolResultEvent` consumers; history and Store retain Content only. `OutputSchema`, top-level tool Title/Icons, content annotations, resource-link icons, all `_meta`, and progress notifications are intentionally dropped.

Before `Close`, callers must cancel every Agent call or stream using the bridge and join each in-flight Execute via its exact completion signal. Close concurrent with Execute is unsupported and may wait indefinitely for an uncooperative remote tool.

Frozen v1 non-goals: agent/server mode; resources, prompts, sampling, roots, logging, and elicitation UI; OAuth policy, configuration, automatic installation, or network discovery; dynamic refresh/unregister; bridge retry/redial, per-tool timeouts, shared sessions; and root README/examples.
