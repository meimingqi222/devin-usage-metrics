// Package quota reads local subscription credentials. Accounts deliberately carry
// only an opaque source reference: formatting or marshaling one cannot expose tokens.
package quota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Account struct {
	Key      string `json:"key"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	source   string
}

func (a Account) String() string   { return a.Provider + ": " + a.Label }
func (a Account) GoString() string { return a.String() }

type Window struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}
type Result struct {
	Plan    string   `json:"plan"`
	Extra   string   `json:"extra"`
	Windows []Window `json:"windows"`
	// Stale is the refresh error when cached data is shown instead.
	Stale string `json:"stale"`
	// StaleAge is how old the cached data is; RetryIn is the remaining
	// rate-limit cooldown, zero when the refresh failed otherwise.
	StaleAge int64 `json:"stale_age_secs,omitempty"`
	RetryIn  int64 `json:"retry_in_secs,omitempty"`
	// Reset is Claude's reset grants, when the account has any.
	Reset *ClaudeReset `json:"reset,omitempty"`
}
type object = map[string]any

func obj(v any) object { m, _ := v.(map[string]any); return m }
func at(v any, keys ...string) any {
	for _, k := range keys {
		v = obj(v)[k]
	}
	return v
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}
func arr(v any) []any { a, _ := v.([]any); return a }
func when(v any) int64 {
	if s, ok := v.(string); ok {
		for _, f := range []string{time.RFC3339Nano, "2006-01-02T15:04:05"} {
			if t, e := time.Parse(f, s); e == nil {
				return t.Unix()
			}
		}
		return 0
	}
	n := num(v)
	if n > 1e12 {
		n /= 1000
	}
	return int64(n)
}

type httpError struct {
	status int
	retry  time.Duration
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }
func status(e error) int {
	var h *httpError
	if errors.As(e, &h) {
		return h.status
	}
	return 0
}

var client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 8, IdleConnTimeout: 5 * time.Minute}, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

func request(ctx context.Context, method, url, body string, h map[string]string, refresh bool) ([]byte, error) {
	timeout := 10 * time.Second
	if refresh {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, e := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if e != nil {
		return nil, errors.New("invalid quota request")
	}
	for k, v := range h {
		r.Header.Set(k, v)
	}
	resp, e := client.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("quota network request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		d := 5 * time.Minute
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, e := strconv.ParseInt(s, 10, 64); e == nil && n >= 0 {
				d = time.Duration(min(n, 3600)) * time.Second
			} else if t, e := http.ParseTime(s); e == nil {
				d = time.Until(t)
			}
		}
		return nil, &httpError{resp.StatusCode, d, string(b)}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if e != nil {
		return nil, errors.New("quota response read failed")
	}
	return b, nil
}
func jsonRequest(ctx context.Context, method, url, body string, h map[string]string, refresh bool) (object, error) {
	b, e := request(ctx, method, url, body, h, refresh)
	if e != nil {
		return nil, e
	}
	var v object
	if json.Unmarshal(b, &v) != nil {
		return nil, errors.New("invalid quota JSON response")
	}
	return v, nil
}
func bearer(t string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + strings.TrimSpace(t), "Accept": "application/json", "Content-Type": "application/json"}
}

type entry struct {
	gate               chan struct{}
	result             Result
	at, until, backoff time.Time
	err                error
}

var registry = struct {
	sync.Mutex
	m map[string]*entry
}{m: map[string]*entry{}}

func clone(r Result) Result {
	r.Windows = append([]Window(nil), r.Windows...)
	r.Reset = cloneReset(r.Reset)
	return r
}

// Fetch respects cancellation even while waiting for another query of the account.
// force bypasses the five-minute TTL and failure backoff, never a 429 cooldown.
func Fetch(ctx context.Context, a Account, force bool) (Result, error) {
	if e := ctx.Err(); e != nil {
		return Result{}, e
	}
	if a.source == "" {
		return Result{}, errors.New("account must come from Discover")
	}
	fingerprint := sha256.Sum256([]byte(a.source + credentialIdentity(a)))
	key := fmt.Sprintf("%s:%x", a.Key, fingerprint)
	registry.Lock()
	s := registry.m[key]
	if s == nil {
		s = &entry{gate: make(chan struct{}, 1)}
		registry.m[key] = s
	}
	registry.Unlock()
	started := time.Now()
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-s.gate }()
	now := time.Now()
	fallback := func(e error) (Result, error) {
		if !s.at.IsZero() && now.Sub(s.at) <= 2*time.Hour {
			r := clone(s.result)
			r.Stale = e.Error()
			r.StaleAge = int64(now.Sub(s.at).Seconds())
			if now.Before(s.until) {
				r.RetryIn = int64(s.until.Sub(now).Seconds()) + 1
			}
			return r, nil
		}
		return Result{}, e
	}
	if now.Before(s.until) {
		return fallback(fmt.Errorf("HTTP 429 (retry in %ds)", int(time.Until(s.until).Seconds())+1))
	}
	if (!force && now.Sub(s.at) < 5*time.Minute) || s.at.After(started) {
		return clone(s.result), nil
	}
	if !force && now.Before(s.backoff) {
		return fallback(s.err)
	}
	r, e := fetch(ctx, a)
	now = time.Now()
	if e == nil {
		s.result = clone(r)
		s.at = now
		s.until = time.Time{}
		s.backoff = time.Time{}
		s.err = nil
		return r, nil
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	s.err = e
	if status(e) == 429 {
		var h *httpError
		errors.As(e, &h)
		s.until = now.Add(max(time.Minute, min(time.Hour, h.retry)))
	} else {
		s.backoff = now.Add(time.Minute)
	}
	return fallback(e)
}
