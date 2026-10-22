package syncdiag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func readRecords(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var record map[string]any
		if err = json.Unmarshal(line, &record); err != nil {
			t.Fatal("invalid JSON log", err)
		}
		records = append(records, record)
	}
	return records
}

func TestConcurrentLogsKeepRunScopeAndRedactErrors(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	secret := "credential-must-never-appear"
	ctx := WithDevice(WithRun(context.Background()), secret)
	ctx, finish := Stage(ctx, "download_blocks")
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			Log(ctx, "failure", "error", ErrorCode(fmt.Errorf("%s: %w", secret, &url.Error{Op: "Get", URL: "https://" + secret, Err: errors.New(secret)})))
		})
	}
	wg.Wait()
	finish(context.DeadlineExceeded)
	records := readRecords(t)
	for _, r := range records {
		if r["run"] != ctx.Value(runKey) || r["stage"] != "download_blocks" || r["device"] != ID(secret) {
			t.Fatalf("lost log scope: %v", r)
		}
	}
	last := records[len(records)-1]
	if last["status"] != "failed" || last["error"] != "deadline_exceeded" {
		t.Fatal(last)
	}
	raw, _ := os.ReadFile(Path())
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("credential leaked into diagnostic")
	}
	info, _ := os.Stat(Path())
	// Windows reports synthetic POSIX modes; ACLs are checked during native acceptance.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("log mode: %v", info.Mode())
	}
	if WithRun(ctx).Value(runKey) != ctx.Value(runKey) || WithRun(context.Background()).Value(runKey) == ctx.Value(runKey) {
		t.Fatal("run correlation is not stable and distinct")
	}
}

func TestLogRotationAndUnavailableDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	ctx := context.Background()
	Log(ctx, "initial")
	padding := bytes.Repeat([]byte("x"), maxLogBytes-16)
	if err := os.WriteFile(Path(), padding, 0600); err != nil {
		t.Fatal(err)
	}
	Log(ctx, "rotated")
	backup, err := os.ReadFile(Path() + ".1")
	if err != nil || !bytes.Equal(backup, padding) {
		t.Fatal("rotation lost prior log", err)
	}
	if records := readRecords(t); len(records) != 1 || records[0]["event"] != "rotated" {
		t.Fatal(records)
	}
	// A log failure must never change sync behavior or panic.
	blocked := root + "/file"
	if err = os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", blocked)
	Log(ctx, "unavailable")
}

func TestStalledTransferEmitsHeartbeatAndFinishesOnDeadline(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, _ := Stage(WithRun(context.Background()), "download_blocks")
	p := NewProgress(ctx, 10)
	p.Completed()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(7 * time.Second)
	found := false
	for !found {
		select {
		case <-ticker.C:
			for _, r := range readRecords(t) {
				if r["event"] == "transfer_progress" {
					found = true
					if r["completed"] != float64(1) || r["total"] != float64(10) {
						t.Fatal(r)
					}
				}
			}
		case <-deadline:
			p.Finish(context.DeadlineExceeded)
			t.Fatal("stalled transfer produced no heartbeat")
		}
	}
	p.Finish(context.DeadlineExceeded)
	records := readRecords(t)
	last := records[len(records)-1]
	if last["event"] != "transfer_end" || last["error"] != "deadline_exceeded" {
		t.Fatal(last)
	}
}

func TestFieldsRemainIsolatedAcrossWorkers(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	parent := WithFields(WithRun(context.Background()), "operation", "sync")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			child := WithFields(parent, "worker", i)
			Log(WithFields(child, "operation", "query"), "child")
			Log(child, "worker")
		})
	}
	wg.Wait()
	Log(parent, "parent")
	for _, r := range readRecords(t) {
		if r["run"] != parent.Value(runKey) || r["diagnostics_version"] != float64(2) {
			t.Fatal("missing correlation or version", r)
		}
		switch r["event"] {
		case "child":
			if r["operation"] != "query" || r["worker"] == nil {
				t.Fatal(r)
			}
		case "worker":
			if r["operation"] != "sync" || r["worker"] == nil {
				t.Fatal("child mutated worker scope", r)
			}
		case "parent":
			if r["operation"] != "sync" || r["worker"] != nil {
				t.Fatal("worker mutated parent scope", r)
			}
		}
	}
}

func TestWatchJoinsHeartbeatBeforeStageEnds(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, finish := watch(WithRun(context.Background()), "local_collection", 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		time.Sleep(5 * time.Millisecond)
		for _, r := range readRecords(t) {
			if r["event"] == "stage_progress" {
				found = true
				if r["run"] != ctx.Value(runKey) || r["elapsed_ms"] == nil {
					t.Fatal(r)
				}
			}
		}
	}
	finish(context.DeadlineExceeded)
	finish(nil)
	if !found {
		t.Fatal("watch emitted no heartbeat")
	}
	before := len(readRecords(t))
	time.Sleep(20 * time.Millisecond)
	records := readRecords(t)
	if len(records) != before || records[len(records)-1]["event"] != "stage_end" || records[len(records)-1]["error"] != "deadline_exceeded" {
		t.Fatal("heartbeat outlived stage or finish was not idempotent", records)
	}
}
