package toolsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dailz1/go-agent/tool"
)

var _ tool.Tool = (*searchTool)(nil)

// searchTool is the always-advertised entry tool. Execute ranks the source
// catalog with BM25 and publishes the top matches into exposed; it never
// executes a business tool itself.
type searchTool struct {
	s *Search
}

func searchInfo(name string, maxLimit int) tool.ToolInfo {
	no := false
	parameters := tool.NewParameterSchema()
	parameters.Properties["query"] = tool.Param("string",
		"Task keywords; use English words or CJK phrases.")
	parameters.Properties["limit"] = tool.Property{
		Type:        "integer",
		Description: "Maximum matches to load; omit for the configured default.",
		Keywords: []tool.SchemaKeyword{
			{Name: "minimum", Value: json.RawMessage("1")},
			{Name: "maximum", Value: json.RawMessage(strconv.Itoa(maxLimit))},
		},
	}
	parameters.Properties["mode"] = tool.ParamEnum("Retrieval mode; omit to use bm25.", "bm25")
	parameters.Required = []string{"query"}
	parameters.AdditionalProperties = &no
	return tool.ToolInfo{
		Name: name,
		Description: "Search deferred tools by task keywords. " +
			"Matching tools become available with full schemas next round. " +
			"Use those schemas before calling a loaded tool. " +
			"Result text may be truncated; loaded tools still appear in " +
			"the next round's advertised schemas. Use those schemas, " +
			"or search again with a smaller limit for a shorter summary. " +
			"If a tool from earlier history reports not found, stale, or an " +
			"unexpected approval error, search again here. This search does not " +
			"execute business tools.",
		Parameters:       parameters,
		RequiresApproval: false,
	}
}

type searchArgs struct {
	Query string `json:"query"`
	Limit *int   `json:"limit,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

type match struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	NewlyLoaded bool   `json:"newly_loaded"`
}

type searchOutput struct {
	Matches []match `json:"matches"`
	Message string  `json:"message"`
}

func (t *searchTool) Info() tool.ToolInfo {
	return searchInfo(t.s.cfg.toolName, t.s.cfg.limit)
}

// marshalInfo encodes a frozen ToolInfo into its normalized comparison
// form. It runs only for candidates selected for publication, so a search's
// serialization cost stays proportional to the load limit.
func marshalInfo(info tool.ToolInfo) ([]byte, error) {
	b, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	return normalizeJSON(b), nil
}

func (t *searchTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	s := t.s
	query, limit, perr := parseSearchArgs(args, s.cfg.limit)
	if perr != nil {
		return tool.NewErrorResult("%s", perr.Error()), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if muWaitHook != nil {
		muWaitHook()
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	infos := s.source.List()
	if runes := catalogRunes(infos, int64(s.cfg.catalogLimit)); runes > int64(s.cfg.catalogLimit) {
		s.mu.Unlock()
		return tool.NewErrorResult("toolsearch catalog uses %d runes over the %d limit; shrink the deferred pool or raise WithCatalogLimit", runes, s.cfg.catalogLimit), nil
	}
	tokens := tokenize(query)
	if len(tokens) == 0 {
		s.mu.Unlock()
		return tool.NewErrorResult("query produced no search tokens; use English words or at least two adjacent CJK characters"), nil
	}
	ranked := rankTools(infos, tokens)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	var added []tool.Tool
	var removed []string
	var newNames, refreshedNames []string
	pending := make(map[string]*forwardTool)
	newSet := make(map[string]struct{})
	for _, r := range ranked {
		current, registered := s.exposed.Get(r.name)
		wrapper, wasOwned := s.owned[r.name]
		if wasOwned && (!registered || current != tool.Tool(wrapper)) {
			s.mu.Unlock()
			return tool.NewErrorResult("toolsearch cannot load %q: the loaded name was taken over or removed outside this instance", r.name), nil
		}
		if registered && !wasOwned {
			s.mu.Unlock()
			return tool.NewErrorResult("toolsearch cannot load %q: the name is registered outside this instance", r.name), nil
		}
		if registered {
			infoJSON, merr := marshalInfo(r.info)
			if merr != nil {
				s.mu.Unlock()
				return tool.NewErrorResult("toolsearch cannot load %q: %v", r.name, merr), nil
			}
			if !bytes.Equal(infoJSON, wrapper.infoJSON) {
				fresh := &forwardTool{source: s.source, name: r.name, searchName: s.cfg.toolName, frozenInfo: r.info, infoJSON: infoJSON}
				removed = append(removed, r.name)
				added = append(added, fresh)
				refreshedNames = append(refreshedNames, r.name)
				pending[r.name] = fresh
			}
			continue
		}
		if len(s.owned)+len(newNames) >= s.cfg.maxLoaded {
			s.mu.Unlock()
			return tool.NewErrorResult("toolsearch would exceed MaxLoaded (%d); raise the limit or start a fresh exposed registry", s.cfg.maxLoaded), nil
		}
		infoJSON, merr := marshalInfo(r.info)
		if merr != nil {
			s.mu.Unlock()
			return tool.NewErrorResult("toolsearch cannot load %q: %v", r.name, merr), nil
		}
		newNames = append(newNames, r.name)
		newSet[r.name] = struct{}{}
		fresh := &forwardTool{source: s.source, name: r.name, searchName: s.cfg.toolName, frozenInfo: r.info, infoJSON: infoJSON}
		added = append(added, fresh)
		pending[r.name] = fresh
	}

	if len(added) > 0 {
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if err := s.exposed.Apply(removed, added); err != nil {
			s.mu.Unlock()
			return tool.NewErrorResult("toolsearch publish failed: %v", err), nil
		}
		for name, wrapper := range pending {
			s.owned[name] = wrapper
		}
	}
	total := len(s.owned)
	s.mu.Unlock()

	if len(added) > 0 {
		s.cfg.logger.Info("toolsearch loaded",
			slog.Any("tool_names", newNames),
			slog.Any("refreshed_names", refreshedNames),
			slog.Int("loaded_total", total))
	}

	return searchResult(ranked, newSet, len(infos) == 0), nil
}

// searchResult renders the ranked candidates as the model-visible summary.
// The loaded-set authority is the exposed registry: even if the kernel later
// truncates this text, the matches survive as next round's schemas.
func searchResult(ranked []rankedTool, newSet map[string]struct{}, emptyPool bool) *tool.ToolResult {
	out := searchOutput{Matches: []match{}}
	switch {
	case emptyPool:
		out.Message = "No deferred tools are available."
	case len(ranked) == 0:
		out.Message = "No matching tools; try different task keywords."
	default:
		out.Message = "Tools are available next round; use their advertised schemas."
	}
	for _, r := range ranked {
		_, fresh := newSet[r.name]
		out.Matches = append(out.Matches, match{
			Name:        r.name,
			Description: foldDescription(r.description),
			NewlyLoaded: fresh,
		})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return tool.NewErrorResult("toolsearch failed to encode its summary: %v", err)
	}
	return tool.NewTextResult(string(encoded))
}

// foldDescription collapses whitespace runs and truncates to
// maxDescriptionRunes with an ellipsis; scoring always used the full text.
func foldDescription(description string) string {
	folded := strings.Join(strings.Fields(description), " ")
	if utf8.RuneCountInString(folded) <= maxDescriptionRunes {
		return folded
	}
	runes := []rune(folded)
	return string(runes[:maxDescriptionRunes-1]) + "…"
}

// parseSearchArgs validates the model's call: top-level object with only
// known fields, a non-empty query of at most maxQueryRunes, an optional
// limit within [1,maxLimit], and an empty or bm25 mode.
func parseSearchArgs(args json.RawMessage, maxLimit int) (string, int, error) {
	dec := json.NewDecoder(bytes.NewReader(args))
	if tok, err := dec.Token(); err != nil {
		return "", 0, fmt.Errorf("invalid arguments: expected a JSON object with query, limit and mode fields: %v", err)
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return "", 0, fmt.Errorf("invalid arguments: expected a JSON object with query, limit and mode fields")
	}
	seen := make(map[string]struct{}, 3)
	fields := make(map[string]json.RawMessage, 3)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", 0, fmt.Errorf("invalid arguments: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", 0, fmt.Errorf("invalid arguments: object keys must be strings")
		}
		if _, dup := seen[key]; dup {
			return "", 0, fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", 0, fmt.Errorf("invalid arguments: %v", err)
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return "", 0, fmt.Errorf("invalid arguments: %v", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", 0, fmt.Errorf("invalid arguments: unexpected trailing content")
	}
	for key := range fields {
		switch key {
		case "query", "limit", "mode":
		default:
			return "", 0, fmt.Errorf("unknown field %q", key)
		}
	}
	raw, ok := fields["query"]
	if !ok {
		return "", 0, fmt.Errorf("query is required")
	}
	if isNull(raw) {
		return "", 0, fmt.Errorf("query must not be null")
	}
	var query string
	if err := json.Unmarshal(raw, &query); err != nil {
		return "", 0, fmt.Errorf("query must be a string: %v", err)
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", 0, fmt.Errorf("query must be a non-empty string")
	}
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return "", 0, fmt.Errorf("query exceeds %d runes", maxQueryRunes)
	}
	limit := maxLimit
	if raw, ok := fields["limit"]; ok {
		if isNull(raw) {
			return "", 0, fmt.Errorf("limit must not be null")
		}
		// JSON Schema treats 1.0 and 1e0 as integers; validate the exact
		// decimal value (no float rounding) before applying the bounds.
		var v any
		decNum := json.NewDecoder(bytes.NewReader(raw))
		decNum.UseNumber()
		if err := decNum.Decode(&v); err != nil {
			return "", 0, fmt.Errorf("limit must be an integer: %v", err)
		}
		num, ok := v.(json.Number)
		if !ok {
			return "", 0, fmt.Errorf("limit must be a number")
		}
		rat, ok := ratFromNumber(normalizeNumberLiteral(num.String()))
		if !ok || !rat.IsInt() {
			return "", 0, fmt.Errorf("limit must be an integer")
		}
		n := rat.Num()
		if !n.IsInt64() || n.Int64() < 1 || n.Int64() > int64(maxLimit) {
			return "", 0, fmt.Errorf("limit must be between 1 and %d", maxLimit)
		}
		limit = int(n.Int64())
	}
	if raw, ok := fields["mode"]; ok {
		if isNull(raw) {
			return "", 0, fmt.Errorf("mode must not be null")
		}
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			return "", 0, fmt.Errorf("mode must be a string: %v", err)
		}
		if mode != "bm25" {
			return "", 0, fmt.Errorf("unsupported mode %q; only bm25 is available", mode)
		}
	}
	return query, limit, nil
}

// isNull reports whether the raw JSON value is a bare null.
func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}
