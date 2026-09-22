// Package llm defines provider-independent messages, streaming chunks, and errors.
//
// The codex subpackage uses subscription OAuth through an explicitly injected
// codex/auth.Source. Refresh owns authentication and persistence; Token is only a
// read-only snapshot. Interactive login and file storage belong to codexauth.
//
// Codex streams lazily, including authentication, and Chat folds that same stream.
// Unlike public OpenAI Responses, it merges all system instructions and omits
// output-cap fields even for positive MaxTokens. Its structured warning discloses
// that the configured cap is not a server-enforced limit. Iterator errors do not
// enter the agent's outer pre-stream retry loop.
//
// APIError.NonRetryable preserves explicit quota classification while leaving
// legacy zero-value retry behavior intact. Adding the field affects unkeyed
// APIError literals. The agenttest recording v3 format preserves this field and
// Codex error chains; its reader retains strict v1/v2 compatibility.
package llm
