package syncdiag

import (
	"context"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// Watch is a Stage with a joined heartbeat for long local operations.
func Watch(ctx context.Context, name string) (context.Context, func(error)) {
	return watch(ctx, name, 5*time.Second)
}

func watch(ctx context.Context, name string, interval time.Duration) (context.Context, func(error)) {
	ctx, finish := Stage(ctx, name)
	started := time.Now()
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				Log(ctx, "stage_progress", "elapsed_ms", time.Since(started).Milliseconds(), "goroutines", runtime.NumGoroutine())
			}
		}
	}()
	var once sync.Once
	return ctx, func(err error) {
		once.Do(func() { close(done); <-stopped; finish(err) })
	}
}

// RuntimeFields captures process-wide memory/GC pressure, not per-agent costs.
func RuntimeFields() []any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return []any{"heap_alloc_bytes", m.HeapAlloc, "heap_sys_bytes", m.HeapSys, "gc_cycles", m.NumGC, "gc_pause_total_ms", m.PauseTotalNs / uint64(time.Millisecond), "goroutines", runtime.NumGoroutine()}
}

func BuildFields() []any {
	fields := []any{"go_version", runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				fields = append(fields, "build_revision", s.Value)
			case "vcs.modified":
				fields = append(fields, "build_modified", s.Value == "true")
			}
		}
	}
	return fields
}
