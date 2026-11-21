package source

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"time"
)

const BodyLimit = 16 << 20

// Fetch downloads both sources in order under a single ten-second deadline.
// Each response is trimmed before the next complete body is allocated.
func Fetch(ctx context.Context, client *http.Client, now func() time.Time) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var local http.Client
	if client != nil {
		local = *client
	}
	local.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// A shared cookie jar could attach credentials to public metadata requests.
	local.Jar = nil
	s := &Snapshot{SchemaVersion: 2, Windows: make(map[string]map[string]int)}
	for _, item := range []struct{ id, url string }{{V1ID, V1URL}, {V2ID, V2URL}} {
		windows, info, err := fetchOne(ctx, &local, item.url, now)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", item.id, err)
		}
		info.ID = item.id
		s.Sources = append(s.Sources, info)
		if s.VerifiedAt.IsZero() || info.VerifiedAt.Before(s.VerifiedAt) {
			s.VerifiedAt = info.VerifiedAt
		}
		for provider, models := range windows {
			s.Windows[provider] = models
		}
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("validate fetched snapshot: %w", err)
	}
	return s, nil
}

func fetchOne(
	ctx context.Context,
	client *http.Client,
	url string,
	now func() time.Time,
) (map[string]map[string]int, Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, Info{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, Info{}, fmt.Errorf("request metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Info{}, fmt.Errorf("metadata http status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, BodyLimit+1))
	if err != nil {
		return nil, Info{}, fmt.Errorf("read metadata: %w", err)
	}
	if len(data) > BodyLimit {
		return nil, Info{}, fmt.Errorf("metadata exceeds %d bytes", BodyLimit)
	}
	windows, skipped, err := Decode(data, url == V2URL)
	if err != nil {
		return nil, Info{}, fmt.Errorf("decode metadata: %w", err)
	}
	info := Info{
		URL: url, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
		ETag: resp.Header.Get("ETag"), VerifiedAt: now().UTC(), SkippedModels: skipped,
	}
	return windows, info, nil
}
