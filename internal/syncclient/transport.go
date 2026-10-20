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
	"strconv"
	"strings"
	"time"
)

type Authorization struct {
	VerificationURI string `json:"verification_uri"`
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}
type transport struct{ base, token string }

func baseURL(s string) (string, error) {
	u, e := url.Parse(strings.TrimRight(strings.TrimSpace(s), "/"))
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", errors.New("invalid sync API URL")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func (t transport) request(ctx context.Context, method, p string, b []byte, h map[string]string) ([]byte, http.Header, int, error) {
	for retry := 0; ; retry++ {
		body, headers, status, err := t.requestOnce(ctx, method, p, b, h)
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
	c := http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := c.Do(r)
	if e != nil {
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
