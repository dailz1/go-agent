package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

func (p *Provider) openStream(ctx context.Context, endpoint string, body []byte) (*llm.StreamResult, error) {
	token, err := p.source.Refresh(ctx, nil)
	if err != nil {
		return nil, &Error{Kind: KindAuth, Cause: err}
	}
	account := token.AccountID
	for attempt := range 2 {
		if token.AccessToken == "" || token.AccountID == "" || token.AccountID != account {
			return nil, &Error{Kind: KindAuth, Code: "invalid_account_token", Cause: auth.ErrLoginRequired}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create codex request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		request.Header.Set("ChatGPT-Account-Id", token.AccountID)
		request.Header.Set("Originator", p.originator)
		request.Header.Set("User-Agent", "go-agent/codex")
		response, err := p.httpClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("codex request: %w", err)
		}
		closeBody := sync.OnceFunc(func() { response.Body.Close() })
		stopCancel := context.AfterFunc(ctx, closeBody)
		cleanup := func() { stopCancel(); closeBody() }
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return &llm.StreamResult{Body: response.Body, Cleanup: cleanup}, nil
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, llm.MaxResponseBody+1))
		cleanup()
		if readErr != nil {
			return nil, fmt.Errorf("read codex error: %w", readErr)
		}
		if len(data) > llm.MaxResponseBody {
			return nil, protocolError("error response exceeds 10 MiB")
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			token, err = p.source.Refresh(ctx, &token)
			if err != nil {
				return nil, &Error{Kind: KindAuth, Cause: err}
			}
			continue
		}
		api := &llm.APIError{StatusCode: response.StatusCode, Body: string(data), RetryAfter: retryAfter(response.Header.Get("Retry-After"))}
		var envelope struct {
			Error serverError `json:"error"`
		}
		// Non-JSON HTTP errors still retain their actual status and body.
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, classifyServerError(&serverError{}, api)
		}
		return nil, classifyServerError(&envelope.Error, api)
	}
	return nil, protocolError("authentication recovery exhausted")
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 63); err == nil {
		if seconds <= uint64((1<<63-1)/time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}
