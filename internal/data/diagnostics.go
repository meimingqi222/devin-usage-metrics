package data

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

var errSourceDiagnostics = errors.New("local collection source errors")

type decodeStats struct {
	hits, misses, bytes, lines, evictions atomic.Int64
}
type decodeStatsKey struct{}

func diagnosticAgent(a model.Agent) string {
	for _, known := range model.Agents {
		if a == known {
			return string(a)
		}
	}
	return "unsupported"
}

func sourceContext(ctx context.Context, path string) context.Context {
	kind := "transcript"
	if filepath.Ext(path) == ".db" || filepath.Ext(path) == ".sqlite" {
		kind = "database"
	}
	if filepath.Base(path) == "sessions.db" {
		switch filepath.Base(filepath.Dir(path)) {
		case "cli":
			kind = "devin_cli"
		case "cli-next":
			kind = "devin_cli_next"
		}
	}
	return syncdiag.WithFields(ctx, "source", syncdiag.ID(path), "source_kind", kind)
}

func sourceResultError(d model.Data) error {
	if len(d.Errors) != 0 {
		return errSourceDiagnostics
	}
	return nil
}

func snapshotReason(s snapshot, start, end, now int64, refresh, reuse bool, readErr, decodeErr error) string {
	switch {
	case readErr != nil:
		return "snapshot_unavailable"
	case decodeErr != nil:
		return "snapshot_invalid"
	case s.Version != snapshotVersion:
		return "schema_changed"
	case refresh && !reuse:
		return "forced_rebuild"
	case refresh && !s.FreshSources:
		return "uncertified_snapshot"
	case s.Data.TurnsStart > start:
		return "range_start_not_covered"
	case s.Data.TurnsEnd < min(end, now):
		return "range_end_not_covered"
	case refresh:
		return "manual_source_check"
	case now-s.At > 300:
		return "ttl_expired"
	default:
		return "snapshot_hit"
	}
}

func logDecodeTotals(ctx context.Context, stats *decodeStats) {
	decodedWeight.Lock()
	weight := decodedWeight.total
	decodedWeight.Unlock()
	syncdiag.Log(ctx, "decode_cache_result", "file_cache_hits", stats.hits.Load(), "file_decodes", stats.misses.Load(), "decoded_input_bytes", stats.bytes.Load(), "decoded_lines", stats.lines.Load(), "evicted_files", stats.evictions.Load(), "global_decoded_cache_bytes", weight, "decoded_cache_budget_bytes", decodedBudget)
}

func stageMS(start time.Time) int64 { return time.Since(start).Milliseconds() }

func queryDiagnostic(ctx context.Context, db *sqlite.DB, label string) func(int, int64, time.Duration, error) {
	ctx = syncdiag.WithFields(ctx, "query", label)
	ctx, finish := syncdiag.Watch(ctx, "sqlite_query")
	started, before := time.Now(), db.ReadStats()
	return func(rows int, bytes int64, consumer time.Duration, err error) {
		after := db.ReadStats()
		elapsed := time.Since(started)
		syncdiag.Log(ctx, "query_result", "elapsed_ms", elapsed.Milliseconds(), "consumer_ms", consumer.Milliseconds(), "read_and_materialize_ms", max(0, elapsed-consumer).Milliseconds(), "rows", rows, "materialized_bytes", bytes, "main_read_requests", after.Requests-before.Requests, "main_cache_hits", after.CacheHits-before.CacheHits, "main_file_reads", after.FileReads-before.FileReads, "main_file_bytes", after.FileBytes-before.FileBytes, "error", syncdiag.ErrorCode(err))
		finish(err)
	}
}

func materializedBytes(row []any) int64 {
	var n int64
	for _, v := range row {
		switch v := v.(type) {
		case string:
			n += int64(len(v))
		case []byte:
			n += int64(len(v))
		}
	}
	return n
}
