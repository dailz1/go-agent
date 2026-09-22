package codexauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dailz1/go-agent/llm/codex/auth"
)

// LoginConfig configures interaction, not the fixed OAuth authorization profile.
type LoginConfig struct {
	Path        string
	Output      io.Writer
	Headless    bool
	HTTPClient  *http.Client
	OnAuthorize func(context.Context, string) error
}

// Login owns the store and loopback listener until login completes or is canceled.
// The authorization URL is always printed. OnAuthorize may attempt to open a
// browser; its failure leaves the printed URL usable. No browser is opened here.
func Login(ctx context.Context, cfg LoginConfig) (state auth.State, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return auth.State{}, err
	}
	lock, err := acquireLock(cfg.Path)
	if err != nil {
		return auth.State{}, err
	}
	defer func() { err = errors.Join(err, lock.close()) }()
	listener, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		return auth.State{}, fmt.Errorf("bind codex callback port 1455: %w", err)
	}
	defer listener.Close()
	pkce, err := auth.NewPKCE()
	if err != nil {
		return auth.State{}, err
	}
	callback := &loginCallback{
		state: pkce.State,
		done:  make(chan callbackResult, 1),
	}
	server := &http.Server{
		Handler: callback, ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		closeErr := server.Close()
		serveErr := <-served
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		err = errors.Join(err, closeErr, serveErr)
	}()
	out := cfg.Output
	if out == nil {
		out = io.Discard
	}
	if cfg.Headless {
		if _, err := fmt.Fprintln(out, "Forward first: ssh -L 1455:127.0.0.1:1455 user@server"); err != nil {
			return auth.State{}, err
		}
	}
	address := auth.AuthorizationURL(pkce)
	if _, err := fmt.Fprintln(out, address); err != nil {
		return auth.State{}, err
	}
	if cfg.OnAuthorize != nil {
		if openErr := cfg.OnAuthorize(ctx, address); openErr != nil {
			if _, err := fmt.Fprintln(out, "Browser unavailable; open the authorization URL manually."); err != nil {
				return auth.State{}, err
			}
		}
	}
	select {
	case <-ctx.Done():
		return auth.State{}, ctx.Err()
	case result := <-callback.done:
		if result.err != nil {
			return auth.State{}, result.err
		}
		state, err = auth.Exchange(
			ctx,
			refreshConfig(auth.RefreshConfig{HTTPClient: cfg.HTTPClient}),
			result.code,
			pkce.Verifier,
		)
		if err != nil {
			return auth.State{}, err
		}
		if err := Save(ctx, cfg.Path, state); err != nil {
			return auth.State{}, err
		}
		return state, nil
	}
}

type callbackResult struct {
	code string
	err  error
}

type loginCallback struct {
	mu       sync.Mutex
	state    string
	consumed bool
	done     chan callbackResult
}

func (h *loginCallback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Path != "/auth/callback" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	stateValid := len(q["state"]) == 1 && subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(h.state)) == 1
	codeValid := len(q["code"]) == 1 && q.Get("code") != "" && len(q["error"]) == 0
	errorValid := len(q["error"]) == 1 && q.Get("error") != "" && len(q["code"]) == 0
	if !stateValid || (!codeValid && !errorValid) {
		http.Error(w, "Invalid authorization callback.", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.consumed {
		http.Error(w, "Authorization callback already used.", http.StatusConflict)
		return
	}
	h.consumed = true
	result := callbackResult{code: q.Get("code")}
	if errorValid {
		result.err = &auth.Error{Stage: "token", Code: "authorization_denied", Message: "authorization was not granted"}
	}
	h.done <- result
	fmt.Fprintln(w, "Authorization received. Return to the terminal.")
}
