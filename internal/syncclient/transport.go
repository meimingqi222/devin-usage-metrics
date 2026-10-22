package syncclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

type Authorization struct {
	VerificationURI string `json:"verification_uri"`
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}
type transport struct{ base, token string }

// One client keeps connections alive across a cycle's many small transfers;
// the default pool keeps only two idle connections per host.
var httpClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: func() http.RoundTripper {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConnsPerHost = 2 * transferLimit
		return t
	}(),
}

func baseURL(s string) (string, error) {
	u, e := url.Parse(strings.TrimRight(strings.TrimSpace(s), "/"))
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", errors.New("invalid sync API URL")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func (t transport) request(ctx context.Context, method, p string, b []byte, h map[string]string) ([]byte, http.Header, int, error) {
	for retry := 0; ; retry++ {
		started := time.Now()
		body, headers, status, err := t.requestOnce(ctx, method, p, b, h)
		// For an immutable upload a 412 only says the object already exists,
		// the expected answer when re-sending history. Head creation sends the
		// same precondition, but there a 412 is a real CAS conflict.
		expected := status == 404 || (status == 412 && ctx.Value(existingObjectOK{}) != nil)
		if err != nil || (status >= 400 && !expected) || time.Since(started) >= 2*time.Second {
			syncdiag.Log(ctx, "http_result", "method", method, "resource", resourceKind(p), "object", syncdiag.ID(strings.TrimPrefix(p, "/v1/objects/")), "attempt", retry+1, "status", status, "elapsed_ms", time.Since(started).Milliseconds(), "error", syncdiag.ErrorCode(err))
		}
		transient := err != nil || status == 408 || status == 425 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
		if t.token == "" || !transient || retry == 2 || ctx.Err() != nil {
			return body, headers, status, err
		}
		wait := time.Duration(1+2*retry) * time.Second
		if n, e := strconv.Atoi(headers.Get("Retry-After")); e == nil {
			wait = time.Duration(max(1, n)) * time.Second
		} else if at, e := http.ParseTime(headers.Get("Retry-After")); e == nil {
			wait = max(time.Second, time.Until(at))
		}
		if wait > 30*time.Second {
			return body, headers, status, err
		}
		syncdiag.Log(ctx, "http_retry", "method", method, "resource", resourceKind(p), "object", syncdiag.ID(strings.TrimPrefix(p, "/v1/objects/")), "next_attempt", retry+2, "wait_ms", wait.Milliseconds())
		select {
		case <-ctx.Done():
			return nil, nil, 0, ctx.Err()
		case <-time.After(wait):
		}
	}
}
func (t transport) requestOnce(ctx context.Context, method, p string, b []byte, h map[string]string) ([]byte, http.Header, int, error) {
	r, e := http.NewRequestWithContext(ctx, method, t.base+p, bytes.NewReader(b))
	if e != nil {
		return nil, nil, 0, e
	}
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		r.Header.Set(k, v)
	}
	resp, e := httpClient.Do(r)
	if e != nil {
		syncdiag.Log(ctx, "transport_failure", "method", method, "resource", resourceKind(p), "object", syncdiag.ID(strings.TrimPrefix(p, "/v1/objects/")), "cause", syncdiag.ErrorCode(e))
		if ctx.Err() != nil {
			return nil, nil, 0, ctx.Err()
		}
		return nil, nil, 0, errors.New("sync transport request failed")
	}
	defer resp.Body.Close()
	b, e = io.ReadAll(io.LimitReader(resp.Body, 256*1024*1024+1))
	if len(b) > 256*1024*1024 {
		return nil, nil, 0, errors.New("sync response exceeds limit")
	}
	return b, resp.Header, resp.StatusCode, e
}
func statusError(s int) error { return fmt.Errorf("sync API HTTP %d", s) }
func BeginLogin(ctx context.Context, apiURL string) (Authorization, error) {
	var a Authorization
	base, e := baseURL(apiURL)
	if e != nil {
		return a, e
	}
	b, _, s, e := (transport{base: base}).request(ctx, "POST", "/v1/auth/github/device", nil, nil)
	if e != nil {
		return a, e
	}
	if s != 200 {
		return a, statusError(s)
	}
	if e = json.Unmarshal(b, &a); e != nil {
		return a, e
	}
	if a.DeviceCode == "" || a.UserCode == "" || a.VerificationURI == "" {
		return a, errors.New("incomplete authorization")
	}
	a.Interval = max(1, a.Interval)
	a.ExpiresIn = max(60, a.ExpiresIn)
	return a, nil
}
func CompleteLogin(ctx context.Context, apiURL string, a Authorization) error {
	base, e := baseURL(apiURL)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.ExpiresIn)*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"device_code": a.DeviceCode})
	interval := max(1, a.Interval)
	for {
		b, h, s, e := (transport{base: base}).request(ctx, "POST", "/v1/auth/github/token", body, nil)
		if e != nil {
			return e
		}
		if s == 202 {
			if n, e := strconv.Atoi(h.Get("Retry-After")); e == nil {
				interval = max(1, n)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(interval) * time.Second):
			}
			continue
		}
		if s != 200 {
			return statusError(s)
		}
		var login session
		if e = json.Unmarshal(b, &login); e != nil {
			return e
		}
		if login.Token == "" || login.Login == "" || login.Expires <= time.Now().Unix() {
			return errors.New("invalid login session")
		}
		return saveSession(login)
	}
}

// list returns object names. Servers that understand prefix filter on their
// side; older ones ignore the query, so callers still filter.
func (t transport) list(ctx context.Context, prefixes ...string) ([]string, error) {
	q := url.Values{"prefix": prefixes}
	p := "/v1/objects"
	if len(prefixes) > 0 {
		p += "?" + q.Encode()
	}
	b, _, status, e := t.request(ctx, "GET", p, nil, nil)
	if e != nil {
		return nil, e
	}
	if status != 200 {
		return nil, statusError(status)
	}
	var names []string
	if e = json.Unmarshal(b, &names); e != nil {
		return nil, e
	}
	if len(prefixes) == 0 {
		return names, nil
	}
	var out []string
	for _, name := range names {
		if slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
			out = append(out, name)
		}
	}
	return out, nil
}

// putImmutable stores a content-addressed object in one request. A 412 means
// it exists; servers that report the stored ETag prove it intact without a
// download. Otherwise the stored copy is read back, and a damaged one replaced.
// existingObjectOK marks requests of putImmutable, where a 412 is expected.
type existingObjectOK struct{}

func (t transport) putImmutable(ctx context.Context, name string, z []byte) (uploaded bool, err error) {
	p := "/v1/objects/" + url.PathEscape(name)
	want := `"` + hash(z) + `"`
	// Only this first upload expects a 412; the read-back and repair below
	// use the caller's context, so a repair conflict is still logged.
	upload := context.WithValue(ctx, existingObjectOK{}, true)
	_, h, s, e := t.request(upload, "PUT", p, z, map[string]string{"If-None-Match": "*"})
	if e != nil {
		return false, e
	}
	switch {
	case s == 200 || s == 201 || s == 204:
		return true, nil
	case s != 409 && s != 412:
		return false, statusError(s)
	case h.Get("ETag") == want:
		return false, nil
	}
	b, h, s, e := t.request(ctx, "GET", p, nil, nil)
	if e != nil {
		return false, e
	}
	if s == 200 && hash(b) == hash(z) {
		return false, nil
	}
	repair := map[string]string{"If-None-Match": "*"}
	if s == 200 {
		if etag := h.Get("ETag"); etag != "" {
			repair = map[string]string{"If-Match": etag}
		} else {
			repair = nil
		}
	} else if s != 404 {
		return false, statusError(s)
	}
	syncdiag.Log(ctx, "object_repair", "object", syncdiag.ID(name), "status", s)
	return true, t.put(ctx, name, z, repair)
}

func (t transport) get(ctx context.Context, name string, limit int) ([]byte, error) {
	b, _, s, e := t.request(ctx, "GET", "/v1/objects/"+url.PathEscape(name), nil, nil)
	if e != nil {
		return nil, e
	}
	if s != 200 {
		return nil, statusError(s)
	}
	if len(b) > limit {
		return nil, errors.New("object exceeds protocol limit")
	}
	return b, nil
}

// Only fixed resource classes reach the log; p may contain arbitrary server data.
func resourceKind(p string) string {
	if p == "/v1/objects" || strings.HasPrefix(p, "/v1/objects?") {
		return "object_list"
	}
	for _, prefix := range []string{"v3-head-", "v2-head-", "v3-manifest-", "v2-generation-", "v3-block-", "v3-session-", "v2-object-", "device-"} {
		if strings.HasPrefix(p, "/v1/objects/"+prefix) {
			return strings.TrimSuffix(prefix, "-")
		}
	}
	return "other"
}
