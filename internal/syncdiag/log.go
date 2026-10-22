// Package syncdiag writes bounded sync diagnostics without payloads or credentials.
package syncdiag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type key int

const (
	runKey key = iota
	stageKey
	deviceKey
	fieldsKey
)

var sequence atomic.Uint64
var fileMu sync.Mutex

const maxLogBytes = 4 << 20

func Path() string {
	root := os.Getenv("XDG_CACHE_HOME")
	if root == "" {
		root, _ = os.UserCacheDir()
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "devin-usage-metrics", "sync.log")
}

func WithRun(ctx context.Context) context.Context {
	if ctx.Value(runKey) != nil {
		return ctx
	}
	return context.WithValue(ctx, runKey, fmt.Sprintf("%d-%d", os.Getpid(), sequence.Add(1)))
}

func HasRun(ctx context.Context) bool { return ctx.Value(runKey) != nil }

// WithFields scopes fixed diagnostic metadata. Callers must hash paths and
// identifiers and must not supply payloads, credentials or arbitrary errors.
func WithFields(ctx context.Context, fields ...any) context.Context {
	old, _ := ctx.Value(fieldsKey).(map[string]any)
	values := maps.Clone(old)
	if values == nil {
		values = map[string]any{}
	}
	for i := 0; i+1 < len(fields); i += 2 {
		if k, ok := fields[i].(string); ok {
			values[k] = fields[i+1]
		}
	}
	return context.WithValue(ctx, fieldsKey, values)
}

// ID permits correlation without exposing device IDs, URLs or object names.
func ID(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:6])
}
func WithDevice(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, deviceKey, ID(id))
}

// ErrorCode never includes an arbitrary error string: HTTP/client errors may
// embed URLs, headers or user data. Preserve known validation reasons only.
func ErrorCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "request_timeout"
	}
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		return "file_not_found"
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "invalid_json"
	}
	var valueType *json.UnmarshalTypeError
	if errors.As(err, &valueType) {
		return "json_type_mismatch"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_failure"
	}
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	switch err.Error() {
	case "local collection source errors":
		return "source_errors"
	case "invalid zstd frame header", "truncated zstd block", "reserved zstd block type", "zstd requires exactly one complete frame", "invalid zstd size", "zstd size mismatch", "device identity must be initialized before sync", "missing device identity", "invalid encrypted session", "cannot decrypt session", "invalid sync API URL", "sync response exceeds limit", "single turn exceeds protocol block limit", "single session exceeds protocol block limit", "too many blocks", "manifest exceeds limit", "too many shards", "invalid generation content hash", "negative record count", "invalid session partition", "invalid turn partition", "unknown record kind", "v1 identity/schema mismatch":
		return err.Error()
	case "shard hash/size mismatch", "manifest hash mismatch", "manifest record count mismatch", "shard record count mismatch", "invalid shard reference", "invalid head", "invalid existing head", "head identity mismatch", "manifest identity/schema mismatch", "shard schema mismatch", "turn partition mismatch", "session device mismatch", "sync login expired", "sync transport request failed", "sync head CAS conflict", "object exceeds protocol limit", "existing immutable manifest is corrupt", "strong head ETag required for safe publication":
		return err.Error()
	}
	if code, ok := strings.CutPrefix(err.Error(), "sync API HTTP "); ok {
		if n, e := strconv.Atoi(code); e == nil && n >= 100 && n <= 599 {
			return "http_" + strconv.Itoa(n)
		}
	}
	return fmt.Sprintf("error_type:%T", err)
}

func Log(ctx context.Context, event string, fields ...any) {
	record := map[string]any{"time": time.Now().Format(time.RFC3339Nano), "pid": os.Getpid(), "event": event, "diagnostics_version": 2}
	if fields, ok := ctx.Value(fieldsKey).(map[string]any); ok {
		for k, v := range fields {
			record[k] = v
		}
	}
	for k, name := range map[key]string{runKey: "run", stageKey: "stage", deviceKey: "device"} {
		if v := ctx.Value(k); v != nil {
			record[name] = v
		}
	}
	for i := 0; i+1 < len(fields); i += 2 {
		if k, ok := fields[i].(string); ok {
			record[k] = fields[i+1]
		}
	}
	b, err := json.Marshal(record)
	if err != nil {
		return
	}
	path := Path()
	if path == "" {
		return
	}
	fileMu.Lock()
	defer fileMu.Unlock()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(b)+1) > maxLogBytes {
		if os.Rename(path, path+".1") != nil {
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func Stage(ctx context.Context, name string) (context.Context, func(error)) {
	ctx = context.WithValue(ctx, stageKey, name)
	started := time.Now()
	Log(ctx, "stage_start")
	return ctx, func(err error) {
		status := "ok"
		if err != nil {
			status = "failed"
		}
		Log(ctx, "stage_end", "status", status, "elapsed_ms", time.Since(started).Milliseconds(), "error", ErrorCode(err))
	}
}

// Progress emits a heartbeat even when all workers are waiting on requests.
// Finish joins the reporter so no progress line appears after stage completion.
type Progress struct {
	completed atomic.Int64
	done      chan struct{}
	stopped   chan struct{}
	ctx       context.Context
	total     int
	started   time.Time
}

func NewProgress(ctx context.Context, total int) *Progress {
	p := &Progress{done: make(chan struct{}), stopped: make(chan struct{}), ctx: ctx, total: total, started: time.Now()}
	Log(ctx, "transfer_start", "total", total, "workers", min(total, 4))
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				p.report("transfer_progress", nil)
			}
		}
	}()
	return p
}
func (p *Progress) Completed() { p.completed.Add(1) }
func (p *Progress) report(event string, err error) {
	Log(p.ctx, event, "completed", p.completed.Load(), "total", p.total, "elapsed_ms", time.Since(p.started).Milliseconds(), "error", ErrorCode(err))
}
func (p *Progress) Finish(err error) {
	close(p.done)
	<-p.stopped
	p.report("transfer_end", err)
}
