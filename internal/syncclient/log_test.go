package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

func TestCycleLogCapturesPublicationAndFailureWithoutCredentials(t *testing.T) {
	setup(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s := &objectServer{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	if err := SaveConfig(Config{true, server.URL}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := saveSession(session{"token", "private-login", now + 3600}); err != nil {
		t.Fatal(err)
	}
	local := model.Data{TurnsStart: now, TurnsEnd: now, Turns: []model.Turn{{Agent: model.Claude, CreatedAt: now, SessionKey: "private-session"}}}
	if _, err := Cycle(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	// Force a deadline at the same public entry point used by the desktop.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := Cycle(ctx, local); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{server.URL, "Bearer token", "private-login", "private-session"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("sensitive value leaked: %q", secret)
		}
	}
	events := map[string]bool{}
	deadlineLogged := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r map[string]any
		if err = json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		events[r["event"].(string)] = true
		deadlineLogged = deadlineLogged || (r["event"] == "stage_end" && r["stage"] == "cycle" && r["error"] == "deadline_exceeded")
	}
	for _, event := range []string{"cycle_lock_wait", "cycle_lock_acquired", "devices_discovered", "encode_start", "encode_end", "transfer_end", "upload_result", "head_cas_start", "head_cas_committed"} {
		if !events[event] {
			t.Fatalf("missing diagnostic %s", event)
		}
	}
	if !deadlineLogged {
		t.Fatal("cycle timeout is missing from diagnostics")
	}
}
