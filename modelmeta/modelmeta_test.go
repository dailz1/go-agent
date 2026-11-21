package modelmeta

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

var fixedAt = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func testCatalog() *Catalog {
	return catalog(&source.Snapshot{
		SchemaVersion: 2, VerifiedAt: fixedAt,
		Sources: []source.Info{
			{ID: source.V1ID, URL: source.V1URL, VerifiedAt: fixedAt, SkippedModels: 2},
			{ID: source.V2ID, URL: source.V2URL, VerifiedAt: fixedAt.Add(time.Second), SkippedModels: 1},
		},
		Windows: map[string]map[string]int{
			"openai":  {"m": 1000, "same": 3000},
			"zhipuai": {"m": 1000}, "zai": {"m": 1000},
			source.Codex: {"m": 2000, "same": 4000},
		},
	})
}

func TestWindowsAgeAndArguments(t *testing.T) {
	c := testCatalog()
	for _, tc := range []struct {
		name, provider string
		at             time.Time
		age            time.Duration
		wantErr        error
	}{
		{"fresh", "openai", fixedAt, time.Hour, nil},
		{"before expiry", "openai", fixedAt.Add(time.Hour - 1), time.Hour, nil},
		{"at expiry", "openai", fixedAt.Add(time.Hour), time.Hour, ErrStale},
		{"after expiry", "openai", fixedAt.Add(time.Hour + 1), time.Hour, ErrStale},
		{"default fresh", "openai", fixedAt.Add(DefaultMaxAge - 1), 0, nil},
		{"default expires", "openai", fixedAt.Add(DefaultMaxAge), 0, ErrStale},
		{"negative age", "openai", fixedAt, -1, ErrInvalidArgument},
		{"zero time", "openai", time.Time{}, 0, ErrInvalidArgument},
		{"before verification", "openai", fixedAt.Add(-1), 0, ErrInvalidArgument},
		{"unknown", "missing", fixedAt, 0, ErrUnknownProvider},
		{"empty provider", "", fixedAt, 0, ErrUnknownProvider},
		{"case exact", "OpenAI", fixedAt, 0, ErrUnknownProvider},
		{"codex same age", source.Codex, fixedAt.Add(time.Hour), time.Hour, ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Windows(tc.provider, tc.at, tc.age)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v; want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if got != nil {
					t.Fatal("error returned partial map")
				}
			} else if !reflect.DeepEqual(got, map[string]int{"m": 1000, "same": 3000}) {
				t.Fatalf("windows = %v", got)
			}
		})
	}
	for _, c := range []*Catalog{nil, {}} {
		if windows, err := c.Windows("openai", fixedAt, 0); windows != nil || !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid catalog = %v, %v", windows, err)
		}
	}
}

func TestCatalogCopiesAndConcurrentReads(t *testing.T) {
	c := testCatalog()
	info := c.Info()
	if info.SchemaVersion != 2 || info.ProviderCount != 4 || info.ModelCount != 6 || info.SkippedModels != 3 {
		t.Fatalf("info = %+v", info)
	}
	info.Sources[0].ID = "changed"
	if c.Info().Sources[0].ID != source.V1ID {
		t.Fatal("sources slice shared")
	}
	windows, err := c.Windows("openai", fixedAt, 0)
	if err != nil {
		t.Fatal(err)
	}
	windows["m"] = 99
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				got, err := c.Windows("openai", fixedAt, 0)
				if err != nil || got["m"] != 1000 {
					t.Errorf("windows = %v, %v", got, err)
				}
				got["m"] = -1
				c.Info().Sources[0].ID = "other"
			}
		})
	}
	wg.Wait()
}
