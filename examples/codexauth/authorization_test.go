package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/codexauth"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

type promptReader struct {
	t      *testing.T
	input  io.Reader
	output *bytes.Buffer
	reads  int
}

func (r *promptReader) Read(p []byte) (int, error) {
	r.reads++
	if r.output.Len() == 0 {
		r.t.Error("input requested before displaying authorization choices")
	}
	return r.input.Read(p)
}

func TestAuthorizationPromptFlow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input       string
		terminal    bool
		headless    bool
		wantBrowser bool
		wantPrompt  bool
		wantLogin   bool
		browserErr  error
	}{
		{name: "default browser", input: "\n", terminal: true, wantBrowser: true, wantPrompt: true, wantLogin: true},
		{name: "explicit browser", input: "1\n", terminal: true, wantBrowser: true, wantPrompt: true, wantLogin: true},
		{name: "manual URL", input: "2\n", terminal: true, wantPrompt: true, wantLogin: true},
		{name: "invalid then manual", input: "x\n2\n", terminal: true, wantPrompt: true, wantLogin: true},
		{name: "headless TTY", terminal: true, headless: true, wantLogin: true},
		{name: "headless pipe", headless: true, wantLogin: true},
		{name: "non TTY defaults manual", input: "1\n", wantLogin: true},
		{name: "TTY EOF aborts", terminal: true, wantPrompt: true},
		{name: "browser unavailable", input: "1\n", terminal: true, wantBrowser: true, wantPrompt: true, wantLogin: true, browserErr: errors.New("browser unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldInput, oldTerminal := authorizationInput, terminalInput
			oldLogin, oldBrowser := login, launchBrowser
			t.Cleanup(func() {
				authorizationInput, terminalInput = oldInput, oldTerminal
				login, launchBrowser = oldLogin, oldBrowser
			})
			var out, stderr bytes.Buffer
			input := &promptReader{t: t, input: strings.NewReader(tc.input), output: &stderr}
			authorizationInput = input
			terminalInput = func() bool { return tc.terminal }
			browserCalls, loginCalls := 0, 0
			launchBrowser = func(context.Context, string) error {
				browserCalls++
				if input.reads == 0 {
					t.Error("browser opened before choosing an authorization method")
				}
				return tc.browserErr
			}
			stopped := errors.New("login stopped at authorization boundary")
			login = func(ctx context.Context, cfg codexauth.LoginConfig) (auth.State, error) {
				loginCalls++
				if cfg.Headless == tc.wantBrowser || (cfg.OnAuthorize != nil) != tc.wantBrowser {
					t.Errorf("authorization config: headless=%t, browser=%t", cfg.Headless, cfg.OnAuthorize != nil)
				}
				if cfg.OnAuthorize != nil {
					// Browser-open failure is advisory; Login owns URL fallback.
					if err := cfg.OnAuthorize(ctx, "https://auth.openai.com/oauth/authorize"); !errors.Is(err, tc.browserErr) {
						t.Errorf("browser callback error = %v", err)
					}
				}
				return auth.State{}, stopped
			}
			args := []string{"login", "--store", "unused-test-store"}
			if tc.headless {
				args = append(args, "--headless")
			}
			err := run(t.Context(), args, &out, &stderr)
			if tc.wantLogin {
				if !errors.Is(err, stopped) || loginCalls != 1 {
					t.Fatalf("login calls=%d, error=%v", loginCalls, err)
				}
			} else if !errors.Is(err, io.EOF) || loginCalls != 0 {
				t.Fatalf("EOF invoked login: calls=%d, error=%v", loginCalls, err)
			}
			if (browserCalls == 1) != tc.wantBrowser || browserCalls > 1 {
				t.Fatalf("browser calls = %d", browserCalls)
			}
			if (input.reads > 0) != tc.wantPrompt {
				t.Fatalf("input reads = %d", input.reads)
			}
			if !tc.terminal && !tc.headless && stderr.Len() == 0 {
				t.Fatal("non-TTY manual authorization omitted its notice")
			}
		})
	}
}
