package source

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

const (
	V1ID  = "models-dev-v1"
	V2ID  = "models-dev-v2-codex"
	V1URL = "https://models.dev/api.json"
	V2URL = "https://models.dev/experimental/v2.0/api.json?include=openai-codex&type=chat"
)

// Info records a complete upstream response, including discarded fields.
type Info struct {
	ID            string    `json:"id"`
	URL           string    `json:"url"`
	SHA256        string    `json:"sha256"`
	ETag          string    `json:"etag"`
	VerifiedAt    time.Time `json:"verified_at"`
	SkippedModels int       `json:"skipped_models"`
}

// Snapshot is the versioned on-disk representation shared by both entry points.
type Snapshot struct {
	SchemaVersion int                       `json:"schema_version"`
	Sources       []Info                    `json:"sources"`
	VerifiedAt    time.Time                 `json:"verified_at"`
	Windows       map[string]map[string]int `json:"windows"`
}

// ParseSnapshot checks the complete format without consulting the clock.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	if err := CheckJSON(data); err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("validate snapshot: %w", err)
	}
	return &s, nil
}

// Validate enforces format 2, both provenance records and the necessary Codex data.
func (s *Snapshot) Validate() error {
	if s.SchemaVersion != 2 || len(s.Sources) != 2 {
		return fmt.Errorf("snapshot requires format 2 and two sources")
	}
	seen := make(map[string]bool)
	var earliest time.Time
	for _, info := range s.Sources {
		url := ""
		switch info.ID {
		case V1ID:
			url = V1URL
		case V2ID:
			url = V2URL
		default:
			return fmt.Errorf("unknown source %q", info.ID)
		}
		if seen[info.ID] || info.URL != url || info.VerifiedAt.IsZero() || info.SkippedModels < 0 {
			return fmt.Errorf("invalid source %q", info.ID)
		}
		digest, err := hex.DecodeString(info.SHA256)
		if err != nil {
			return fmt.Errorf("source %q digest: %w", info.ID, err)
		}
		if len(digest) != 32 {
			return fmt.Errorf("source %q requires sha256", info.ID)
		}
		seen[info.ID] = true
		if earliest.IsZero() || info.VerifiedAt.Before(earliest) {
			earliest = info.VerifiedAt
		}
	}
	if !s.VerifiedAt.Equal(earliest) {
		return fmt.Errorf("snapshot time differs from earliest source")
	}
	if len(s.Windows[Codex]) == 0 {
		return fmt.Errorf("snapshot requires codex windows")
	}
	for provider, models := range s.Windows {
		if provider == "" || models == nil {
			return fmt.Errorf("invalid snapshot provider %q", provider)
		}
		for model, n := range models {
			if model == "" || n <= 0 || int64(n) > 2147483647 {
				return fmt.Errorf("invalid snapshot window %q/%q", provider, model)
			}
		}
	}
	return nil
}

// Canonical emits compact JSON with every object key sorted and one newline.
// Source records are ordered by ID; model and provider keys remain exact.
func Canonical(s *Snapshot) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	copy := *s
	copy.Sources = slices.Clone(s.Sources)
	slices.SortFunc(copy.Sources, func(a, b Info) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	data, err := json.Marshal(copy)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	// Struct declaration order is not canonical JSON key order. Round-trip
	// through JSON values so encoding/json sorts keys at every depth.
	var value any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode canonical value: %w", err)
	}
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode canonical value: %w", err)
	}
	return result.Bytes(), nil
}
