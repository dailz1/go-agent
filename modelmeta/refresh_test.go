package modelmeta

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

const v1Body = `{"openai":{"models":{"m":{"limit":{"context":1000}},"bad":{}}},"openai-codex":{"models":{"m":{"limit":{"context":9}}}}}`
const v2Body = `{"openai":{"models":{"m":{"limit":{"context":999}}}},"openai-codex":{"models":{"m":{"limit":{"context":2000}}}}}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func setClock(t *testing.T, now func() time.Time) {
	t.Helper()
	old := timeNow
	timeNow = now
	t.Cleanup(func() { timeNow = old })
}

func TestFetchProvenanceScopesAndSharedAge(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2Older=%v", reverse), func(t *testing.T) {
			times := []time.Time{fixedAt, fixedAt.Add(time.Hour)}
			if reverse {
				times[0], times[1] = times[1], times[0]
			}
			index := 0
			setClock(t, func() time.Time { at := times[index]; index++; return at })
			requests := 0
			var deadline time.Time
			bodies := []*trackedBody{{Reader: strings.NewReader(v1Body)}, {Reader: strings.NewReader(v2Body)}}
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(source.V1URL)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(u, []*http.Cookie{{Name: "token", Value: "secret"}})
			client := &http.Client{
				Jar:           jar,
				CheckRedirect: func(*http.Request, []*http.Request) error { t.Fatal("original redirect callback used"); return nil },
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					urls := []string{source.V1URL, source.V2URL}
					if requests >= 2 {
						t.Fatal("unexpected extra request")
					}
					if r.URL.String() != urls[requests] || r.Method != http.MethodGet {
						t.Fatalf("request = %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("If-None-Match") != "" {
						t.Fatalf("unexpected credentials or conditional headers: %v", r.Header)
					}
					d, ok := r.Context().Deadline()
					if !ok || (!deadline.IsZero() && !d.Equal(deadline)) {
						t.Fatal("requests do not share a deadline")
					}
					deadline = d
					if requests == 1 && !bodies[0].closed {
						t.Fatal("v1 body still open during v2")
					}
					body := bodies[requests]
					requests++
					return &http.Response{StatusCode: 200, Body: body, Header: http.Header{"Etag": {fmt.Sprint(requests)}}}, nil
				}),
			}
			c, err := Fetch(context.Background(), client)
			if err != nil {
				t.Fatal(err)
			}
			if requests != 2 || !bodies[0].closed || !bodies[1].closed || client.Jar != jar || client.CheckRedirect == nil {
				t.Fatal("request count, body closure or original client changed")
			}
			info := c.Info()
			if !info.VerifiedAt.Equal(fixedAt) || info.SkippedModels != 1 || info.ModelCount != 2 {
				t.Fatalf("info = %+v", info)
			}
			for i, body := range []string{v1Body, v2Body} {
				s := info.Sources[i]
				if s.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(body))) || !s.VerifiedAt.Equal(times[i]) || s.ETag != fmt.Sprint(i+1) {
					t.Fatalf("source %d = %+v", i, s)
				}
			}
			for _, tc := range []struct {
				provider string
				want     int
			}{{"openai", 1000}, {source.Codex, 2000}} {
				w, err := c.Windows(tc.provider, fixedAt.Add(time.Hour-1), time.Hour)
				if err != nil || w["m"] != tc.want {
					t.Fatalf("windows = %v, %v", w, err)
				}
				if w, err := c.Windows(tc.provider, fixedAt.Add(time.Hour), time.Hour); w != nil || !errors.Is(err, ErrStale) {
					t.Fatalf("expiry = %v, %v", w, err)
				}
			}
		})
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

func TestFetchFailuresAreAtomicAndCloseBodies(t *testing.T) {
	readErr := errors.New("injected read failure")
	huge := strings.Repeat(" ", source.BodyLimit+1)
	for _, tc := range []struct {
		name   string
		step   int
		status int
		body   string
		read   bool
	}{
		{"v1 redirect", 0, 302, "", false},
		{"v1 rate limit", 0, 429, "", false},
		{"v1 server error", 0, 500, "", false},
		{"v2 server error", 1, 500, "", false},
		{"v1 read error", 0, 200, "", true},
		{"v2 read error", 1, 200, "", true},
		{"v1 oversized", 0, 200, huge, false},
		{"v2 oversized", 1, 200, huge, false},
		{"v1 malformed", 0, 200, "{", false},
		{"v2 malformed", 1, 200, "{", false},
		{"v2 missing codex", 1, 200, `{"openai":{}}`, false},
		{"v2 no windows", 1, 200, `{"openai-codex":{"models":{"m":{}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setClock(t, func() time.Time { return fixedAt })
			old := testCatalog()
			oldInfo := old.Info()
			requests := 0
			bodies := []*trackedBody{}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := 200, v1Body
				if requests == 1 {
					body = v2Body
				}
				if requests == tc.step {
					status, body = tc.status, tc.body
				}
				b := &trackedBody{Reader: strings.NewReader(body)}
				if requests == tc.step && tc.read {
					b.Reader = failedReader{err: readErr}
				}
				bodies = append(bodies, b)
				requests++
				return &http.Response{StatusCode: status, Body: b, Header: http.Header{"Location": {source.V2URL}}}, nil
			})}
			got, err := Fetch(context.Background(), client)
			if got != nil || err == nil || requests != tc.step+1 {
				t.Fatalf("fetch = %v, %v, %d requests", got, err, requests)
			}
			if tc.read && !errors.Is(err, readErr) {
				t.Fatalf("lost read error: %v", err)
			}
			for _, b := range bodies {
				if !b.closed {
					t.Fatal("response body leaked")
				}
			}
			if !reflect.DeepEqual(old.Info(), oldInfo) {
				t.Fatal("old catalog changed")
			}
		})
	}
}

func TestFetchCancellationAndCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}
	if got, err := Fetch(ctx, client); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v, %v", got, err)
	}
	// An already expired fixed deadline also proves that Fetch does not replace
	// the caller context with an unrelated ten-second context.
	ctx, cancel = context.WithDeadline(context.Background(), fixedAt.Add(-100*365*24*time.Hour))
	defer cancel()
	if got, err := Fetch(ctx, client); got != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline = %v, %v", got, err)
	}
	if got, err := Fetch(nil, client); got != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context = %v, %v", got, err)
	}
}

func TestRefreshDoesNotExtendOldGuard(t *testing.T) {
	old := testCatalog()
	base := &stubProvider{}
	guard, err := old.Guard(base, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	setClock(t, func() time.Time { return fixedAt.Add(time.Hour) })
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := strings.Replace(v1Body, "1000", "1500", 1)
		if r.URL.String() == source.V2URL {
			body = v2Body
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	fresh, err := Fetch(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	windows, err := fresh.Windows("openai", fresh.Info().VerifiedAt, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nextBase, err := llm.WithContextWindows(&stubProvider{}, windows)
	if err != nil {
		t.Fatal(err)
	}
	next, err := fresh.Guard(nextBase, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := guard.(llm.ContextWindowProvider).ContextWindow("m"); ok {
		t.Fatal("fetch extended old guard")
	}
	if n, ok := next.(llm.ContextWindowProvider).ContextWindow("m"); !ok || n != 1500 {
		t.Fatalf("new guard = %d, %v", n, ok)
	}
}
