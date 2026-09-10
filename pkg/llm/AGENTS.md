# pkg/llm

Shared kernel: `Provider` interface, `Message`/`ContentBlock` model, `Chunk` streaming, HTTP+SSE plumbing, retry, `APIError` - plus two adapter subpackages. Earned its file: score 11 (largest inbound-ref count after tool; adapter duplication rule lives here).

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Provider contract | provider.go:19 - `Name`/`Chat`/`ChatStream`; sentinel `ErrStreamingNotSupported`; `llm.Option` + exported `ApplyOptions` |
| Message model | message.go - sealed `ContentBlock` (Text/Reasoning/Image/ToolUse/ToolResult); custom `UnmarshalJSON` accepts string, array, or null content |
| Stream chunks | chunk.go - 5 sealed variants: TextDelta, ReasoningDelta, ToolCallStart, ToolCallArgs, Done |
| HTTP boundary / APIError | http.go (`DoJSONRequest`/`DoStreamRequest`, 10MB body cap, Cleanup-func contract), errors.go (`StatusCode`/`RetryAfter`/`Retryable()`) |
| SSE parsing | sse.go - lazy iterator, 1MB line cap |
| Retry classification | retry.go - only 429/5xx/network retry; `sleepFor` package-var test seam |
| GLM adapter (glm/) | JWT-vs-bearer auth sniffed from key content, thinking mode, `tool_stream`, eager HTTP request |
| Auth internals (glm/) | auth.go - HS256 JWT, ~5-min early-expiry token cache (sync.Map), `timeNow` clock seam |
| OpenAI adapter (openai/) | lazy request inside iterator, `stream_options.include_usage`, fails stream without a DoneChunk, round-trips ReasoningContent |
| Request knobs | `llm.Option` set: WithModel / WithMaxTokens / WithTemperature / WithStop; applied via exported `ApplyOptions` (provider.go) |
| Message constructors | SystemMessage / UserMessage / AssistantMessage / AssistantToolCallMessage / ToolResultMessage (message.go) |
| Token accounting | Usage (usage.go): Total / Add / IsZero / String |
| Shared truncate | `llm.Truncate` (util.go) - rune-count helper reused by pkg/agent |

## CONVENTIONS (differs from parent)
- **Adapter duplication is intentional.** `convert*`, `firstNonEmpty`, `ptrToString`, `encodeBase64`, `parseStreamPayload` exist near-verbatim in BOTH glm/ and openai/. Any change to shared conversion semantics must be applied to both; do not extract a shared helper package.
- Option-function normalization is the exception, not the rule: constructors normalize (except glm's `WithTopP`, which clamps 0.01-1.0 in place).
- Reasoning differs per adapter: OpenAI round-trips `ReasoningContent`; GLM silently strips ReasoningBlocks outbound. GLM truncates `stop` to 1 entry.
- Stream timing differs: GLM issues the HTTP request eagerly before returning the iterator; OpenAI lazily inside it - this changes which errors count as retryable pre-stream failures.

## ANTI-PATTERNS (THIS PACKAGE)
- Do not implement `Provider`/`ContentBlock`/`Chunk` outside this package - sealed via unexported marker methods.
- Do not add a third adapter by forking one and "improving" shared helpers in only one place - keep the pair in sync.
