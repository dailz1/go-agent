package toolsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/dailz1/go-agent/tool"
)

var _ tool.Tool = (*forwardTool)(nil)

// normalizeNumberLiteral renders a json.Number literal as normalized
// scientific notation ("d.ddde±X"): leading zeros stripped, fractional
// trailing zeros stripped, decimal point folded into the exponent. Two
// literals of the same value normalize identically ("0.5" == "5e-1",
// "9007199254740993.0" == "9007199254740993"), purely by string
// manipulation — no numeric parsing, so no rounding and no unbounded
// expansion. Unparseable exponents return the literal unchanged.
func normalizeNumberLiteral(lit string) string {
	mant := lit
	exp := 0
	if i := strings.IndexAny(lit, "eE"); i >= 0 {
		mant = lit[:i]
		parsed, err := strconv.Atoi(lit[i+1:])
		if err != nil {
			return lit
		}
		exp = parsed
	}
	neg := strings.HasPrefix(mant, "-")
	mant = strings.TrimPrefix(mant, "-")
	dot := strings.Index(mant, ".")
	intPart, fracPart := mant, ""
	if dot >= 0 {
		intPart, fracPart = mant[:dot], mant[dot+1:]
	}
	digits := intPart + fracPart
	p := len(intPart)
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "0"
	}
	significant := strings.TrimRight(trimmed, "0")
	if significant == "" {
		return "0"
	}
	trailing := len(trimmed) - len(significant)
	sciExp := new(big.Int).SetInt64(int64(p - len(digits) + exp + trailing))
	out := significant + "e" + sciExp.String()
	if neg {
		out = "-" + out
	}
	return out
}

// ratFromNumber parses a normalized json.Number literal into an exact
// big.Rat for limit integrality checks. Call normalizeNumberLiteral first:
// the structural caps here (literal width, exponent magnitude) reject
// pathological spellings that could expand unboundedly during parsing, and
// valid limits live in [1,100] so anything wider cannot be in range.
func ratFromNumber(lit string) (*big.Rat, bool) {
	if len(lit) > 64 {
		return nil, false
	}
	if i := strings.IndexAny(lit, "eE"); i >= 0 {
		exp, err := strconv.Atoi(lit[i+1:])
		if err != nil || exp > 40 || exp < -40 {
			return nil, false
		}
	}
	rat, ok := new(big.Rat).SetString(lit)
	return rat, ok
}

var _ tool.Tool = (*forwardTool)(nil)

// forwardTool is one loaded business tool as advertised to the model: a thin
// wrapper that forwards Execute to the live source tool after verifying the
// metadata it was loaded under is still current. frozenInfo is the isolated
// deep copy taken from source.List at load time and served by Info;
// infoJSON is its normalized encoding, compared against the live source
// metadata with object-key order and number-form insensitivity so
// semantically equal schemas never read as stale.
type forwardTool struct {
	source     *tool.Registry
	name       string
	searchName string
	frozenInfo tool.ToolInfo
	infoJSON   []byte
}

func (f *forwardTool) Info() tool.ToolInfo {
	// Callers must not mutate the returned value: it shares nested schema
	// state with the frozen snapshot (the same read-only Info() convention
	// every tool.Tool implementation in this repository follows).
	return f.frozenInfo
}

func (f *forwardTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	current, ok := f.source.Get(f.name)
	if !ok {
		return tool.NewErrorResult("tool %q is no longer available; call %s with task keywords to search again", f.name, f.searchName), nil
	}
	now, err := json.Marshal(current.Info())
	if err != nil || !bytes.Equal(normalizeJSON(now), f.infoJSON) {
		return tool.NewErrorResult("tool %q changed since it was loaded (catalog_stale); call %s with task keywords to search again", f.name, f.searchName), nil
	}
	return current.Execute(ctx, args)
}

// normalizeJSON re-encodes raw JSON into a canonical form: object keys are
// sorted and numbers are rendered from their exact value (int64 when
// integral, otherwise 64-bit float), so semantically equal schemas compare
// byte-equal despite member order or spelling differences (1 vs 1.0).
// Unparseable input is returned unchanged; the comparison then treats it as
// changed, matching the stale contract.
func normalizeJSON(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(canonicalValue(v))
	if err != nil {
		return raw
	}
	return out
}

// canonicalValue maps numbers to their exact canonical value: integral
// numbers stay JSON numbers rendered from the exact big.Rat numerator
// (preserving large-integer precision), non-integral numbers become exact
// ratio tokens ("R:num/den") so equal values compare equal regardless of
// spelling; recurses into containers.
func canonicalValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for key, val := range t {
			m[key] = canonicalValue(val)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonicalValue(val)
		}
		return out
	case json.Number:
		return json.Number(normalizeNumberLiteral(t.String()))
	default:
		return v
	}
}
