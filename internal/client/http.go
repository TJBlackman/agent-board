package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// API is an authenticated HTTP client bound to one origin.
type API struct {
	base  string
	token string
	http  *http.Client
}

// NewAPI builds the client. Redirects are refused so credentials never
// leave the configured origin.
func NewAPI(base, token string) *API {
	return &API{base: base, token: token, http: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("redirects are refused; update the configured board URL instead")
		},
	}}
}

// Response is a decoded HTTP result.
type Response struct {
	Status  int
	Header  http.Header
	Body    []byte
	ErrCode string
	ErrMsg  string
	ErrFld  string
}

// Err converts an error status into a Go error.
func (r *Response) Err() error {
	if r.Status >= 200 && r.Status < 300 {
		return nil
	}
	if r.ErrCode != "" {
		f := ""
		if r.ErrFld != "" {
			f = " [" + r.ErrFld + "]"
		}
		return fmt.Errorf("HTTP %d %s%s: %s", r.Status, r.ErrCode, f, r.ErrMsg)
	}
	return fmt.Errorf("HTTP %d: %s", r.Status, strings.TrimSpace(string(r.Body)))
}

// Retryable reports whether a failed response should be retried.
func (r *Response) Retryable() bool {
	return r.Status == 429 || r.Status == 502 || r.Status == 503 || r.Status == 504
}

// RetryAfter parses the Retry-After header in seconds.
func (r *Response) RetryAfter() time.Duration {
	if v := r.Header.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// Do performs one request. Auth is added unless noAuth.
func (a *API) Do(ctx context.Context, method, path string, headers map[string]string, body []byte, noAuth bool) (*Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return nil, err
	}
	if !noAuth {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/markdown")
	req.Header.Set("User-Agent", "agent-board-cli/"+Version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	r := &Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
	if resp.StatusCode >= 400 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		var env struct {
			Error struct {
				Code, Message, Field string
			} `json:"error"`
		}
		if json.Unmarshal(b, &env) == nil {
			r.ErrCode, r.ErrMsg, r.ErrFld = env.Error.Code, env.Error.Message, env.Error.Field
		}
	}
	return r, nil
}

// Version is set by the build.
var Version = "dev"

// DoWithRetry applies the three attempt, 30 second budget policy. The same
// headers (idempotency key, open event id) are reused on every attempt.
func (a *API) DoWithRetry(method, path string, headers map[string]string, body []byte, p *Pending) (*Response, error) {
	deadline := time.Now().Add(30 * time.Second)
	backoff := time.Second
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if p != nil {
			p.Attempts++
			p.Save()
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		r, err := a.Do(ctx, method, path, headers, body, false)
		cancel()
		if err == nil {
			if r.Status < 400 {
				return r, nil
			}
			if r.ErrCode == "board_instance_changed" {
				return r, errors.New("board instance changed: the board at this URL was replaced; run agent-board init again and start a new deliberate operation (the pending record was kept for review)")
			}
			if !r.Retryable() {
				return r, nil // non retryable 4xx: caller decides
			}
			last = r.Err()
			if ra := r.RetryAfter(); ra > 0 {
				backoff = ra
			}
		} else {
			last = err
		}
		if p != nil {
			p.LastError = last.Error()
			p.Save()
		}
		if attempt == 3 || time.Now().Add(backoff).After(deadline) {
			break
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	return nil, fmt.Errorf("outcome unknown after retries: %v", last)
}

// NewKey returns a fresh idempotency key or open event id.
func NewKey() string {
	id, err := domain.NewUUID()
	if err != nil {
		fmt.Fprintln(os.Stderr, "random source failed:", err)
		os.Exit(2)
	}
	return id
}
