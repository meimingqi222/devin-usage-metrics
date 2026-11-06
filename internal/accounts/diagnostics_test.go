package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

func TestReconcileDiagnosticsExplainIdentityWithoutCredentials(t *testing.T) {
	home := fixture(t)
	email, access, refresh := "private@example.com", "private-access-token", "private-refresh-token"
	write(t, home, ".claude/.credentials.json", claudeDoc(email, "private-org", access, refresh))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileContext(syncdiag.WithRun(context.Background()), quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{email, access, refresh, "private-org", home} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("reconcile diagnostics contain private data")
		}
	}
	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if r["event"] == "account_reconcile_decision" {
			found = r["identity_source"] == "credential_document" && r["match_reason"] == "stable_identity" && r["capture"] == "saved" && r["run"] != nil
		}
	}
	if !found {
		t.Fatal("identity source and binding reason are missing")
	}
}

func TestSwitchDiagnosticErrorDoesNotExposeEmail(t *testing.T) {
	err := fmt.Errorf("this saved copy holds %s's sign-in; sign in again to refresh it", "private@example.com")
	if DiagnosticErrorCode(err) != "saved_copy_conflict" {
		t.Fatal("conflicting sign-in lost safe error code")
	}
	if DiagnosticErrorCode(fmt.Errorf("wrapped: %w", ErrDisabled)) != "switching_disabled" {
		t.Fatal("wrapped disabled error lost classification")
	}
}

func TestIdentityLookupDiagnosticsDoNotLogResponse(t *testing.T) {
	fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"private-response-data"}`)
	}))
	defer server.Close()
	previous := googleUserInfoURL
	googleUserInfoURL = server.URL
	defer func() { googleUserInfoURL = previous }()
	if googleAccountEmail(syncdiag.WithRun(context.Background()), "private-access-token") != "" {
		t.Fatal("rejected identity lookup returned an email")
	}
	raw, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-response-data", "private-access-token", server.URL} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("lookup log contains private data")
		}
	}
	if !bytes.Contains(raw, []byte(`"status_code":401`)) || !bytes.Contains(raw, []byte(`"has_email":false`)) {
		t.Fatal("lookup log omitted HTTP status or unresolved identity")
	}
}
