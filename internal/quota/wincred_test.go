package quota

import (
	"runtime"
	"strings"
	"testing"
)

// TestTestBinaryNeverUsesRealCredential pins the guard that keeps tests off
// the user's real Antigravity sign-in: the Credential Manager ignores the
// temporary HOME and APPDATA a fixture sets, and a test once overwrote the
// real item with fixture tokens.
func TestTestBinaryNeverUsesRealCredential(t *testing.T) {
	fixtureHome(t)
	first := antigravityWinCredTarget()
	if first == "gemini:antigravity" || !strings.HasPrefix(first, "aum-test:") {
		t.Fatalf("a test resolves the real credential name: %q", first)
	}
	fixtureHome(t)
	if second := antigravityWinCredTarget(); second == first {
		t.Fatalf("two fixtures share one scratch credential: %q", second)
	}
}

// TestAntigravityCredentialManagerIsTheLiveSignIn pins the Windows store
// Antigravity 2.x reads: discovery takes its sign-in from the Credential
// Manager item and a switch writes it back there, as compact JSON naming
// the account. The state database alone left Antigravity on the previous
// account while this app showed the switch as done.
func TestAntigravityCredentialManagerIsTheLiveSignIn(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Credential Manager is Windows only")
	}
	fixtureHome(t)
	item := object{
		"token":          object{"access_token": "a-live", "token_type": "Bearer", "refresh_token": "r-live", "expiry": "2030-01-01T00:00:00.000000Z"},
		"auth_method":    "consumer",
		"id_token_email": "live@example.test",
	}
	if err := keyWriteDoc("antigravity", "gemini", item); err != nil {
		t.Fatal(err)
	}
	var live *Account
	for _, a := range Discover(false) {
		if a.Provider == "antigravity" {
			live = &a
		}
	}
	if live == nil || live.CredentialKind() != "keychain" || live.Label != "live@example.test" || !live.Writable() {
		t.Fatalf("the Credential Manager sign-in is not the live account: %+v", live)
	}
	next := object{
		"token":          object{"access_token": "a-next", "token_type": "Bearer", "refresh_token": "r-next", "expiry": "2030-01-01T00:00:00.000000Z"},
		"auth_method":    "consumer",
		"id_token_email": "next@example.test",
	}
	if err := live.WriteCredential(next); err != nil {
		t.Fatal(err)
	}
	got, err := live.CredentialDoc()
	if err != nil || str(at(got, "token", "refresh_token")) != "r-next" || str(got["id_token_email"]) != "next@example.test" {
		t.Fatalf("switch did not land in the Credential Manager: %#v %v", got, err)
	}
	raw, err := winCredRead(antigravityWinCredTarget())
	if err != nil || !strings.HasPrefix(string(raw), "{") {
		t.Fatalf("item is not the plain JSON go-keyring writes on Windows: %v", err)
	}
}
