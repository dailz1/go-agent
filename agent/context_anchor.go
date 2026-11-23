package agent

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"math/bits"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type contextMeasurement struct {
	provider           string
	model              string
	fingerprint        []byte // nil clears an old anchor and cannot establish one.
	fingerprintVersion int
	prefixMessages     int
	inputTokens        int64
	outputTokens       int64
	captureConfigSeq   int64
}

// usageAnchor belongs to one run. The owner must bump *generation on every
// history replacement, including edits that preserve the message count.
// The prefix and already counted suffix must otherwise remain unchanged.
type usageAnchor struct {
	contextMeasurement
	durable            bool
	suffixRunes        int
	countedMessages    int
	jsonMode           bool
	generation         *uint64
	boundaryGeneration uint64
}

func newUsageAnchor(m contextMeasurement, history []llm.Message, generation *uint64) *usageAnchor {
	if m.fingerprint == nil || m.fingerprintVersion != 1 || generation == nil {
		return nil
	}
	if m.prefixMessages < 1 || m.prefixMessages != len(history) || !knownBlockTypes(history) {
		return nil
	}
	if _, _, ok := eligibleUsage(&llm.Usage{InputTokens: m.inputTokens, OutputTokens: m.outputTokens}); !ok {
		return nil
	}
	return &usageAnchor{
		contextMeasurement: m,
		countedMessages:    len(history),
		jsonMode:           knownHistoryJSON(history),
		generation:         generation,
		boundaryGeneration: *generation,
	}
}

func eligibleUsage(u *llm.Usage) (input, output int64, ok bool) {
	if u == nil || u.InputTokens <= 0 || u.OutputTokens < 0 {
		return 0, 0, false
	}
	if u.CachedTokens < 0 || u.ReasoningTokens < 0 || u.CachedTokens > u.InputTokens {
		return 0, 0, false
	}
	if u.OutputTokens > math.MaxInt64-u.InputTokens {
		return 0, 0, false
	}
	return u.InputTokens, u.OutputTokens, true
}

// contextFingerprintV1 hashes ALL options fields deliberately conservatively:
// any options change invalidates the anchor, even a sampling-only change.
func contextFingerprintV1(opts llm.Options, tools []tool.ToolInfo) []byte {
	encoded, err := json.Marshal(struct {
		Options llm.Options
		Tools   []tool.ToolInfo
	}{Options: opts, Tools: tools})
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(append([]byte("go-agent-context-v1\x00"), encoded...))
	return sum[:]
}

func estimateContextTokens(history []llm.Message, a *usageAnchor) uint64 {
	fallback := func() uint64 {
		return uint64(estimateRunes(history))
	}
	if a == nil || a.generation == nil || a.fingerprint == nil {
		return fallback()
	}
	if *a.generation != a.boundaryGeneration || len(history) < a.countedMessages {
		return fallback()
	}
	appended := history[a.countedMessages:]
	if a.durable {
		appended = measurementHistory(appended)
	}
	if !knownBlockTypes(appended) {
		// Custom encoders need not remain deterministic between calls.
		a.generation = nil
		return fallback()
	}
	if len(appended) > 0 {
		size, jsonMode := estimateRunesMode(appended)
		if a.jsonMode && !jsonMode {
			a.generation = nil
			return fallback()
		}
		if !a.jsonMode {
			size = fallbackRunes(appended)
		}
		delta := dropDelta(size, a.jsonMode)
		maxInt := int(^uint(0) >> 1)
		a.suffixRunes += min(delta, maxInt-a.suffixRunes)
		a.countedMessages = len(history)
	}
	// Eligible usage fits int64; the nonnegative rune suffix also fits
	// int64, so their exact sum fits uint64 on every supported platform.
	return uint64(a.inputTokens+a.outputTokens) + uint64(a.suffixRunes)
}

// appendRunesDelta measures an append, not a history replacement. Known JSON
// prefixes are checked for malformed raw tool arguments without re-encoding
// them. Custom encoders and transitions to fallback require a full recount.
func appendRunesDelta(prev, appended []llm.Message) int {
	if len(appended) == 0 {
		return 0
	}
	if !knownBlockTypes(prev) || !knownBlockTypes(appended) {
		joined := make([]llm.Message, 0, len(prev)+len(appended))
		joined = append(joined, prev...)
		joined = append(joined, appended...)
		return estimateRunes(joined) - estimateRunes(prev)
	}
	if !knownHistoryJSON(prev) {
		return fallbackRunes(appended)
	}
	size, jsonMode := estimateRunesMode(appended)
	if !jsonMode {
		return fallbackRunes(prev) + size - estimateRunes(prev)
	}
	if len(prev) == 0 {
		// nil encodes as null; a non-nil empty slice encodes as [].
		return size - estimateRunes(prev)
	}
	return dropDelta(size, true)
}

// All known blocks encode deterministically; only raw tool arguments can fail.
func knownHistoryJSON(history []llm.Message) bool {
	for _, message := range history {
		for _, block := range message.Content {
			var input json.RawMessage
			switch typed := block.(type) {
			case *llm.ToolUseBlock:
				if typed != nil {
					input = typed.Input
				}
			}
			if input != nil && !json.Valid(input) {
				return false
			}
		}
	}
	return true
}

// compactionTargetRunes translates an overflowing token estimate to a rune
// target. Preconditions: R >= 1, E >= 1, B >= 0. A nonpositive result means
// there is no positive rune target. E <= B leaves the unanchored target alone.
func compactionTargetRunes(B, R int64, E uint64) int64 {
	if E <= uint64(B) {
		return B
	}
	// B < E makes the quotient strictly less than R, so Div64 cannot
	// overflow and the result fits int64 even when the product does not.
	hi, lo := bits.Mul64(uint64(R), uint64(B))
	q, _ := bits.Div64(hi, lo, E)
	return min(B, R-1, int64(q))
}
