package toolsearch

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/dailz1/go-agent/tool"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// isCJK reports whether r belongs to the CJK scripts the tokenizer handles
// (Han, Hiragana, Katakana, Hangul).
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// tokenize splits text into BM25 tokens. Latin runs are lowercased and split
// on whitespace only (punctuation, underscores and camelCase stay attached);
// contiguous CJK characters become adjacent-character bigrams, split at the
// boundary to non-CJK text; a lone CJK character yields no token.
func tokenize(text string) []string {
	var tokens []string
	var word strings.Builder
	var cjk []rune
	flushWord := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	flushCJK := func() {
		for i := 0; i+1 < len(cjk); i++ {
			tokens = append(tokens, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		if isCJK(r) {
			flushWord()
			cjk = append(cjk, r)
			continue
		}
		flushCJK()
		if unicode.IsSpace(r) {
			flushWord()
			continue
		}
		word.WriteRune(unicode.ToLower(r))
	}
	flushCJK()
	flushWord()
	return tokens
}

// rankedTool is one positive-score candidate: the frozen identity for
// publication plus the display description for the search result.
type rankedTool struct {
	info        tool.ToolInfo
	name        string
	description string
	score       float64
}

// rankTools scores the source catalog against the query tokens with BM25
// and returns the positive matches ordered by score descending, ties broken
// by ascending tool name. Token accumulation runs over a fixed query order,
// so scores are deterministic across calls.
func rankTools(infos []tool.ToolInfo, query []string) []rankedTool {
	if len(infos) == 0 || len(query) == 0 {
		return nil
	}
	unique := make([]string, 0, len(query))
	seenQuery := make(map[string]struct{}, len(query))
	for _, q := range query {
		if _, dup := seenQuery[q]; dup {
			continue
		}
		seenQuery[q] = struct{}{}
		unique = append(unique, q)
	}

	docs := make([][]string, len(infos))
	counts := make([]map[string]int, len(infos))
	total := 0
	df := make(map[string]int, len(unique))
	for i, info := range infos {
		docs[i] = tokenize(info.Name + "\n" + info.Description)
		total += len(docs[i])
		m := make(map[string]int, len(docs[i]))
		for _, tok := range docs[i] {
			m[tok]++
		}
		counts[i] = m
		for _, q := range unique {
			if m[q] > 0 {
				df[q]++
			}
		}
	}
	avgdl := float64(total) / float64(len(infos))
	if avgdl == 0 {
		return nil
	}

	out := make([]rankedTool, 0, len(infos))
	for i, info := range infos {
		dl := float64(len(docs[i]))
		var score float64
		for _, q := range unique {
			tf := float64(counts[i][q])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(infos))-float64(df[q])+0.5)/(float64(df[q])+0.5))
			score += idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
		if score <= 0 {
			continue
		}
		out = append(out, rankedTool{
			info:        info,
			name:        info.Name,
			description: info.Description,
			score:       score,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].name < out[j].name
	})
	return out
}
