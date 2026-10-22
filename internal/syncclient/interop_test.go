package syncclient

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// TestRustGoLiveInterop is opt-in: it runs independently compiled Rust and Go
// clients against the real sync-server, with isolated credentials and storage.
func TestRustGoLiveInterop(t *testing.T) {
	rust := os.Getenv("AUM_RUST_SYNC_HELPER")
	serverBinary := os.Getenv("AUM_SYNC_SERVER_BINARY")
	if rust == "" || serverBinary == "" {
		t.Skip("set AUM_RUST_SYNC_HELPER and AUM_SYNC_SERVER_BINARY for migration acceptance")
	}
	setup(t)
	resetMemoryCaches()
	defer resetMemoryCaches()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{"tenant": "github-migration-fixture", "login": "fixture", "expires": now + 3600})
	encoded := base64.RawURLEncoding.EncodeToString(claims)
	signingSecret := base64.RawURLEncoding.EncodeToString(secret)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(encoded))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	command := exec.CommandContext(ctx, serverBinary)
	command.Env = append(os.Environ(), "GITHUB_CLIENT_ID=migration-fixture", "SYNC_AUTH_SECRET="+signingSecret, "SYNC_DATA_DIR="+filepath.Join(root, "objects"), "SYNC_LISTEN="+address)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { command.Process.Kill(); command.Wait() }()
	url := "http://" + address
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response, e := http.Get(url + "/healthz")
		if e == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("fixture sync-server did not become ready")
	}
	if err = SaveConfig(Config{true, url}); err != nil {
		t.Fatal(err)
	}
	if err = saveSession(session{token, "fixture", now + 3600}); err != nil {
		t.Fatal(err)
	}
	rustHome := filepath.Join(root, "rust-home")
	config := filepath.Join(rustHome, "Library", "Application Support", "devin-usage-metrics")
	if err = os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(config, "device.json"), []byte(`{"id":"rust-device","name":"Rust fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Linux reference helpers use XDG_CONFIG_HOME instead of the macOS path.
	xdg := filepath.Join(rustHome, "config", "devin-usage-metrics")
	if err = os.MkdirAll(xdg, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(xdg, "device.json"), []byte(`{"id":"rust-device","name":"Rust fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(root, "fixture-token")
	if err = os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	rustCycle := func(local model.Data) model.Data {
		t.Helper()
		input := filepath.Join(root, "input.json")
		b, e := json.Marshal(local)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(input, b, 0600); e != nil {
			t.Fatal(e)
		}
		cmd := exec.CommandContext(ctx, rust)
		cmd.Env = append(os.Environ(), "HOME="+rustHome, "USERPROFILE="+rustHome, "APPDATA="+filepath.Join(rustHome, "config"), "LOCALAPPDATA="+filepath.Join(rustHome, "cache"), "XDG_CONFIG_HOME="+filepath.Join(rustHome, "config"), "XDG_CACHE_HOME="+filepath.Join(rustHome, "cache"), "AUM_INTEROP_URL="+url, "AUM_INTEROP_TOKEN_FILE="+tokenFile, "AUM_INTEROP_INPUT="+input)
		out, e := cmd.Output()
		if e != nil {
			t.Fatal("Rust fixture cycle failed", e)
		}
		var remote model.Data
		if e = json.Unmarshal(out, &remote); e != nil {
			t.Fatal("invalid Rust fixture output", e)
		}
		return remote
	}
	check := func(got, want model.Data) {
		t.Helper()
		sort.Slice(got.Sessions, func(i, j int) bool { return got.Sessions[i].Key < got.Sessions[j].Key })
		sort.Slice(want.Sessions, func(i, j int) bool { return want.Sessions[i].Key < want.Sessions[j].Key })
		sort.Slice(got.Turns, func(i, j int) bool { return got.Turns[i].CreatedAt < got.Turns[j].CreatedAt })
		sort.Slice(want.Turns, func(i, j int) bool { return want.Turns[i].CreatedAt < want.Turns[j].CreatedAt })
		if !reflect.DeepEqual(got.Sessions, want.Sessions) || !reflect.DeepEqual(got.Turns, want.Turns) {
			t.Fatalf("wire records changed: sessions %d/%d turns %d/%d", len(got.Sessions), len(want.Sessions), len(got.Turns), len(want.Turns))
		}
	}
	rustData := interopData("rust-device", "Rust fixture", now, 1)
	if got := rustCycle(rustData); len(got.Turns) != 0 {
		t.Fatal("first device imported unexpected records")
	}
	goData := interopData("local", "Go fixture", now, 1)
	if err = atomic("device.json", []byte(`{"id":"local","name":"Go fixture"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := Cycle(ctx, goData)
	if err != nil {
		t.Fatal(err)
	}
	check(got, rustData)
	check(rustCycle(rustData), goData)
	goData = interopData("local", "Go fixture", now, 2)
	got, err = Cycle(ctx, goData)
	if err != nil {
		t.Fatal(err)
	}
	check(got, rustData)
	check(rustCycle(rustData), goData)
	rustData = interopData("rust-device", "Rust fixture", now, 2)
	check(rustCycle(rustData), goData)
	resetMemoryCaches() // A cold Go client must decode Rust's new blocks again.
	got, err = Cycle(ctx, goData)
	if err != nil {
		t.Fatal(err)
	}
	check(got, rustData)
	t.Log("Rust -> Go, Go -> Rust, incremental updates in both directions and cold re-import preserved all fixture fields")
}

func interopData(device, name string, now int64, version int) model.Data {
	d := model.Data{TurnsStart: now - 86400, TurnsEnd: now, Errors: []model.DataError{}, FileMtimes: map[string]int64{}, FileSessions: map[string]string{}}
	for i, agent := range append(append([]model.Agent(nil), model.Agents...), model.Agent("amp")) {
		key := string(agent) + "/fixture-中文<>\\u2028"
		d.Sessions = append(d.Sessions, model.Session{Agent: agent, Key: key, ID: key, Title: "fixture 中文\u2028", Source: "fixture", CreatedAt: now - 3600, LastActivityAt: now - 60, SelectedModel: "fixture-model", DeviceID: device, DeviceName: name})
		for v := 0; v < version; v++ {
			cost := 0.12345
			d.Turns = append(d.Turns, model.Turn{Agent: agent, SessionKey: key, CreatedAt: now - 3600 + int64(i*10+v), InputTokens: 123.5, OutputTokens: 45, CacheReadTokens: 67, CacheCreationTokens: 89, CacheCreation5mTokens: 80, CacheCreation1hTokens: 9, Model: "fixture-model", ContextStream: "fixture", ContextOrder: uint64(v), TTFTMs: 12.5, TotalTimeMs: 321.5, RecordedCost: &cost, DeviceID: device, DeviceName: name})
		}
	}
	return d
}
