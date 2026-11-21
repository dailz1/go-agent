// Package modelmeta supplies immutable models.dev context-window catalogs.
// Use a catalog's Guard result as the Agent's provider to enforce expiry.
package modelmeta

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

// DefaultMaxAge is the default catalog lifetime, not an upstream guarantee.
const DefaultMaxAge time.Duration = 30 * 24 * time.Hour

var (
	ErrStale           = errors.New("modelmeta: stale catalog")
	ErrUnknownProvider = errors.New("modelmeta: unknown provider")
	ErrInvalidArgument = errors.New("modelmeta: invalid argument")
)

// Catalog is immutable after construction and supports concurrent queries.
// Its zero value has no usable data.
type Catalog struct {
	info    SnapshotInfo
	windows map[string]map[string]int
}

// SnapshotInfo describes the catalog and the age shared by all its providers.
type SnapshotInfo struct {
	SchemaVersion int
	Sources       []SourceInfo
	VerifiedAt    time.Time
	ProviderCount int
	ModelCount    int
	SkippedModels int
}

// SourceInfo describes the complete original response, before trimming.
type SourceInfo struct {
	ID            string
	URL           string
	SHA256        string
	ETag          string
	VerifiedAt    time.Time
	SkippedModels int
}

func catalog(s *source.Snapshot) *Catalog {
	info := SnapshotInfo{
		SchemaVersion: s.SchemaVersion, VerifiedAt: s.VerifiedAt,
		ProviderCount: len(s.Windows), Sources: make([]SourceInfo, 0, len(s.Sources)),
	}
	for _, item := range s.Sources {
		info.Sources = append(info.Sources, SourceInfo(item))
		info.SkippedModels += item.SkippedModels
	}
	for _, models := range s.Windows {
		info.ModelCount += len(models)
	}
	return &Catalog{info: info, windows: s.Windows}
}

// Info returns a copy, including a separate Sources slice.
// A nil catalog returns the zero SnapshotInfo.
func (c *Catalog) Info() SnapshotInfo {
	if c == nil {
		return SnapshotInfo{}
	}
	info := c.info
	info.Sources = slices.Clone(info.Sources)
	return info
}

// Windows exports a fresh map for the exact source provider ID.
// Invalid arguments, expiry and unknown providers always return a nil map.
func (c *Catalog) Windows(provider string, at time.Time, maxAge time.Duration) (map[string]int, error) {
	if c == nil || c.info.VerifiedAt.IsZero() || len(c.windows) == 0 {
		return nil, fmt.Errorf("windows catalog: %w", ErrInvalidArgument)
	}
	if at.IsZero() || at.Before(c.info.VerifiedAt) || maxAge < 0 {
		return nil, fmt.Errorf("windows time or age: %w", ErrInvalidArgument)
	}
	if maxAge == 0 {
		maxAge = DefaultMaxAge
	}
	if at.Sub(c.info.VerifiedAt) >= maxAge {
		return nil, ErrStale
	}
	models, ok := c.windows[provider]
	if !ok {
		return nil, fmt.Errorf("windows provider %q: %w", provider, ErrUnknownProvider)
	}
	return maps.Clone(models), nil
}
