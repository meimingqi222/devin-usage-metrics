package accounts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

// misfiledAntigravity sets up a live Antigravity login (one@) and a saved
// record that claims two@ while holding tokens the issuer says are one@'s.
func misfiledAntigravity(t *testing.T) (*Store, quota.Account, Profile) {
	t.Helper()
	fixture(t)
	googleUserInfoFake(t)
	state := writeAntigravityStateFixture(t, "access-1", "refresh-1", "one@example.test")
	live := quota.ProfileAccount("antigravity-local", "antigravity", "one@example.test", state)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile([]quota.Account{live}); err != nil {
		t.Fatal(err)
	}
	misfiled := Profile{ID: "misf00000000001", Provider: "antigravity", StableID: "two@example.test", Email: "two@example.test", Kind: "file", CreatedAt: 1}
	raw, err := json.Marshal(map[string]any{
		"access_token": "access-2", "refresh_token": "refresh-2",
		"id_token": jwt(map[string]any{"email": "two@example.test"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.data.Profiles = append(s.data.Profiles, misfiled)
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	if err := os.WriteFile(s.CredentialPath("antigravity", misfiled.ID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return s, live, misfiled
}

func liveRefresh(t *testing.T, live quota.Account) any {
	t.Helper()
	d, err := live.CredentialDoc()
	if err != nil {
		t.Fatal(err)
	}
	return d["refresh_token"]
}

// TestRestartFallbackKeepsSwitchChecks pins that the full-restart path only
// falls back to a plain write for a write race or a write failure. It used
// to fall back on any Switch error, so a copy Switch had just refused for
// holding another account's sign-in was written anyway.
func TestRestartFallbackKeepsSwitchChecks(t *testing.T) {
	s, live, misfiled := misfiledAntigravity(t)
	if err := s.switchAfterQuit("antigravity", misfiled.ID, live); err == nil {
		t.Fatal("restart path switched to a copy holding another account's sign-in")
	}
	if got := liveRefresh(t, live); got != "refresh-1" {
		t.Fatalf("restart path wrote the refused copy: %v", got)
	}
}

// TestReapplyRefusesUnswitchableTarget pins that Reapply checks the target
// like Switch: it runs after the language service restarts and must not be
// a way around the checks.
func TestReapplyRefusesUnswitchableTarget(t *testing.T) {
	s, live, misfiled := misfiledAntigravity(t)
	s.mu.Lock()
	s.suspect["antigravity/"+misfiled.ID] = true
	s.mu.Unlock()
	if err := s.Reapply("antigravity", misfiled.ID, live); err == nil {
		t.Fatal("Reapply wrote a copy flagged as another account's sign-in")
	}
	if err := s.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	delete(s.suspect, "antigravity/"+misfiled.ID)
	s.mu.Unlock()
	if err := s.Reapply("antigravity", misfiled.ID, live); err == nil {
		t.Fatal("Reapply wrote with account switching off")
	}
	if got := liveRefresh(t, live); got != "refresh-1" {
		t.Fatalf("Reapply changed the live sign-in: %v", got)
	}
}

// blockingIssuer makes Google's userinfo hang until release is closed and
// reports each request on asked.
func blockingIssuer(t *testing.T) (asked chan struct{}, release func()) {
	t.Helper()
	asked = make(chan struct{}, 8)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		<-gate
		w.WriteHeader(401)
	}))
	old, oldToken := googleUserInfoURL, googleTokenURL
	googleUserInfoURL, googleTokenURL = srv.URL, srv.URL
	t.Cleanup(func() {
		release() // a failed check must not leave the handler holding Close
		googleUserInfoURL, googleTokenURL = old, oldToken
		srv.Close()
	})
	return asked, release
}

// storeResponsive fails when the UI-thread calls cannot get through while
// a network lookup is in flight.
func storeResponsive(t *testing.T, s *Store, stage string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.Enabled()
		s.List()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s: the store lock is held across a network lookup", stage)
	}
}

// TestIdentityLookupsRunOutsideTheStoreLock pins that the Google identity
// lookups of a refresh and a switch run without the store lock. The UI
// thread reads the store every frame; a slow issuer froze the window for up
// to the lookups' timeouts.
func TestIdentityLookupsRunOutsideTheStoreLock(t *testing.T) {
	fixture(t)
	state := writeAntigravityStateFixture(t, "access-x", "refresh-x", "")
	live := quota.ProfileAccount("antigravity-local", "antigravity", "", state)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	asked, release := blockingIssuer(t)
	finished := make(chan struct{})
	go func() {
		_, _ = s.Reconcile([]quota.Account{live})
		close(finished)
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never asked the issuer")
	}
	storeResponsive(t, s, "refresh")
	release()
	<-finished

	// The switch's issuer check: a saved copy with bare tokens.
	asked, release = blockingIssuer(t)
	target, err := s.Add("antigravity", map[string]any{
		"access_token": "access-y", "refresh_token": "refresh-y",
		"id_token": jwt(map[string]any{"email": "y@example.test", "sub": "sub-y"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	finished = make(chan struct{})
	go func() {
		_ = s.Switch("antigravity", target.ID, live)
		close(finished)
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("switch never asked the issuer")
	}
	storeResponsive(t, s, "switch")
	release()
	<-finished
}

// TestSignInAgainClearsSuspectFlag pins the repair the suspect message asks
// for: "sign in again to refresh it". The fresh sign-in replaces the copy's
// tokens, so the flag must go with them; it used to stay until restart.
func TestSignInAgainClearsSuspectFlag(t *testing.T) {
	s, _, misfiled := misfiledAntigravity(t)
	s.mu.Lock()
	s.suspect["antigravity/"+misfiled.ID] = true
	s.mu.Unlock()
	if _, err := s.Add("antigravity", map[string]any{
		"access_token": "access-fresh", "refresh_token": "refresh-fresh",
		"id_token": jwt(map[string]any{"email": "two@example.test"}),
	}, "file"); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.List() {
		if p.ID == misfiled.ID && !p.Writable {
			t.Fatalf("a fresh sign-in left the copy flagged: %+v", p)
		}
	}
}
