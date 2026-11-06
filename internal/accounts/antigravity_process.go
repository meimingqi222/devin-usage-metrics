package accounts

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

// antigravityApp is the desktop client as found on this machine.
type antigravityApp struct {
	running bool
	pids    []int
	path    string
}

// These are variables so tests never touch a real Antigravity.
var (
	findAntigravity    = discoverAntigravity
	stopAntigravity    = stopAntigravityApp
	startAntigravity   = startAntigravityApp
	languageServers    = listLanguageServers
	killLanguageServer = killPids
)

// SwitchApp switches Antigravity after quitting the desktop client. The
// client writes its old token on shutdown, so writing the keychain first
// would be overwritten. Full restart is the authoritative path Antigravity
// Tools also falls back to for the IDE and older clients. Every run is
// logged to the diagnostics log: a silent switch failure is undiagnosable.
func (s *Store) SwitchApp(ctx context.Context, provider, id string, live quota.Account) error {
	ctx = syncdiag.WithFields(syncdiag.WithRun(ctx), "provider", provider, "target_profile", diagnosticRef(id))
	start := time.Now()
	syncdiag.Log(ctx, "account_switch_start", "source_kind", live.CredentialKind())
	err := s.switchApp(ctx, provider, id, live)
	syncdiag.Log(ctx, "account_switch_end", "elapsed_ms", time.Since(start).Milliseconds(), "error", DiagnosticErrorCode(err))
	return err
}

func (s *Store) switchApp(ctx context.Context, provider, id string, live quota.Account) error {
	if provider != "antigravity" {
		return s.Switch(provider, id, live)
	}
	app, err := findAntigravity(ctx)
	if err != nil {
		return err
	}
	targetDoc, err := s.ProfileCredential(provider, id)
	if err != nil {
		return errors.New("this account has no saved sign-in; sign in again")
	}
	targetEmail := s.ProfileEmail(provider, id)
	if !app.running {
		syncdiag.Log(ctx, "account_switch_mode", "mode", "client_closed")
		// The client is closed: nothing competes for the stores. Write the
		// keychain plus every mirror the client restores a session from, or
		// the next launch brings the previous account back.
		if err := s.Switch(provider, id, live); err != nil {
			return err
		}
		return applyAntigravityMirrors(ctx, targetEmail, targetDoc)
	}
	// Native Antigravity keeps its window and an Electron supervisor:
	// write the keychain, kill only language_server, and let the supervisor
	// start a child with the new credential. Restarting the whole app
	// re-triggers its first-run Security Notice.
	if len(languageServers(ctx, app)) > 0 && !strings.Contains(strings.ToLower(app.path), "antigravity ide") {
		syncdiag.Log(ctx, "account_switch_mode", "mode", "language_service_restart")
		// Capture the current login and write the target before stopping the
		// child: the supervisor respawns it within seconds and the new child
		// reads the keychain on start.
		if err := s.Switch(provider, id, live); err != nil {
			return err
		}
		old := languageServers(ctx, app)
		killLanguageServer(ctx, old)
		// The child is gone; this write is the one the respawned child
		// reads, and it must hold before reporting anything.
		if err := s.Reapply(provider, id, live); err != nil {
			return fmt.Errorf("assert the new sign-in after stopping the language service: %w", err)
		}
		// The running client keeps the state database locked at times; a
		// failed mirror stays best-effort while the keychain is the truth
		// source the client itself re-reads.
		mirrorErr := applyAntigravityMirrors(ctx, targetEmail, targetDoc)
		syncdiag.Log(ctx, "account_switch_mirrors", "error", DiagnosticErrorCode(mirrorErr))
		if waitLanguageServerRespawn(ctx, app, old, 15*time.Second) {
			// The main process may write its in-memory token back just after
			// the child starts; reassert once more after it settles and make
			// sure the selected account actually won before reporting success.
			time.Sleep(time.Second)
			if err := s.Reapply(provider, id, live); err != nil {
				return fmt.Errorf("re-assert the new sign-in: %w", err)
			}
			mirrorErr := applyAntigravityMirrors(ctx, targetEmail, targetDoc)
			syncdiag.Log(ctx, "account_switch_mirrors", "error", DiagnosticErrorCode(mirrorErr))
			return s.verifySwitched(provider, id, live)
		}
		// No supervisor respawn: fall back to the authoritative full
		// restart, which also avoids a window without an engine.
	}
	// Full restart. The client writes its previous sign-in on shutdown, so
	// quit first, and capture the previous login while the live store still
	// holds it: marking the target active before that capture let the next
	// refresh file the old tokens under the target's profile and overwrite
	// that account's saved copy.
	syncdiag.Log(ctx, "account_switch_mode", "mode", "full_restart")
	if err := stopAntigravity(ctx, app); err != nil {
		return fmt.Errorf("quit Antigravity: %w", err)
	}
	werr := s.Switch(provider, id, live)
	if werr != nil {
		// A compare-and-swap or write failure: fall back to the plain write
		// without capture so a half-finished switch still ends on the target.
		werr = s.Reapply(provider, id, live)
	}
	if werr != nil {
		return fmt.Errorf("write the selected sign-in: %w", werr)
	}
	if err := applyAntigravityMirrors(ctx, targetEmail, targetDoc); err != nil {
		return err
	}
	if err := startAntigravity(ctx, app); err != nil {
		return fmt.Errorf("sign-in switched, but reopening Antigravity failed: %w", err)
	}
	// The restart is part of the switch: give the client time to show up
	// again before the caller refreshes accounts, or its card is missing
	// until the next periodic refresh.
	waitAntigravityRunning(ctx, 15*time.Second)
	return nil
}

func listLanguageServers(ctx context.Context, app antigravityApp) []int {
	if runtime.GOOS == "windows" {
		return nil
	}
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	prefix := strings.TrimSuffix(app.path, string(filepath.Separator)) + string(filepath.Separator)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		pidText, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		command = strings.TrimSpace(command)
		if !ok || !strings.Contains(strings.ToLower(command), "language_server") {
			continue
		}
		if app.path != "" && !strings.HasPrefix(command, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func killPids(ctx context.Context, pids []int) {
	for _, pid := range pids {
		_ = signalProcess(ctx, pid, false)
	}
	if waitPidsGone(ctx, pids, 2*time.Second) {
		return
	}
	for _, pid := range pids {
		_ = signalProcess(ctx, pid, true)
	}
	_ = waitPidsGone(ctx, pids, time.Second)
}

func waitLanguageServerRespawn(ctx context.Context, app antigravityApp, old []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
		now := languageServers(ctx, app)
		for _, pid := range now {
			found := false
			for _, was := range old {
				if pid == was {
					found = true
					break
				}
			}
			if !found {
				return true
			}
		}
	}
	return false
}

func discoverAntigravity(ctx context.Context) (antigravityApp, error) {
	switch runtime.GOOS {
	case "windows":
		return discoverWindowsAntigravity(ctx)
	case "darwin", "linux":
		return discoverUnixAntigravity(ctx)
	}
	return antigravityApp{}, nil
}

func discoverUnixAntigravity(ctx context.Context) (antigravityApp, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return antigravityApp{}, nil // no process list: treat as not running
	}
	var app antigravityApp
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		pidText, command, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || !isAntigravityMain(command) {
			continue
		}
		app.running = true
		app.pids = append(app.pids, pid)
		if p := appBundle(command); p != "" {
			app.path = p
		}
	}
	return app, nil
}

func discoverWindowsAntigravity(ctx context.Context) (antigravityApp, error) {
	out, err := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return antigravityApp{}, nil
	}
	var app antigravityApp
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.Trim(line, "\r\n"), `","`)
		if len(fields) < 2 {
			continue
		}
		name := strings.Trim(fields[0], `"`)
		pid, err := strconv.Atoi(strings.Trim(fields[1], `"`))
		if err != nil || !isAntigravityMain(name) {
			continue
		}
		app.running = true
		app.pids = append(app.pids, pid)
	}
	if app.running {
		for _, p := range []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Antigravity", "Antigravity.exe"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Antigravity IDE", "Antigravity IDE.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Antigravity", "Antigravity.exe"),
		} {
			if _, err := os.Stat(p); err == nil {
				app.path = p
				break
			}
		}
	}
	return app, nil
}

func isAntigravityMain(s string) bool {
	l := strings.ToLower(s)
	if !strings.Contains(l, "antigravity") || strings.Contains(l, "agent usage metrics") {
		return false
	}
	for _, other := range []string{"antigravity tools", "antigravity-tools", "antigravity_manager", "antigravity-manager", "antigravity-cli", "updater", "auto-updater"} {
		if strings.Contains(l, other) {
			return false
		}
	}
	for _, helper := range []string{"helper", "language_server", "plugin", "renderer", "gpu", "crashpad", "utility", "--type="} {
		if strings.Contains(l, helper) {
			return false
		}
	}
	return true
}

func appBundle(command string) string {
	lower := strings.ToLower(command)
	if at := strings.Index(lower, ".app"); at >= 0 {
		return command[:at+4]
	}
	return ""
}

func stopAntigravityApp(ctx context.Context, app antigravityApp) error {
	pids := bundlePids(ctx, app)
	for _, pid := range pids {
		_ = signalProcess(ctx, pid, false)
	}
	if waitPidsGone(ctx, pids, 8*time.Second) {
		return nil
	}
	for _, pid := range pids {
		_ = signalProcess(ctx, pid, true)
	}
	if waitPidsGone(ctx, pids, 3*time.Second) {
		return nil
	}
	return fmt.Errorf("Antigravity did not exit")
}

// bundlePids includes Electron helpers under the same .app bundle. Killing
// only the main process can leave a helper owning the app bundle, and
// `open` then merely activates the old background process instead of
// bringing up a window.
func bundlePids(ctx context.Context, app antigravityApp) []int {
	pids := append([]int(nil), app.pids...)
	if runtime.GOOS == "windows" || app.path == "" {
		return pids
	}
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return pids
	}
	prefix := strings.TrimSuffix(app.path, string(filepath.Separator)) + string(filepath.Separator)
	seen := map[int]bool{}
	for _, pid := range pids {
		seen[pid] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		pidText, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		command = strings.TrimSpace(command)
		if !ok || !strings.HasPrefix(command, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err == nil && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids
}

func signalProcess(ctx context.Context, pid int, force bool) error {
	if runtime.GOOS == "windows" {
		args := []string{"/PID", strconv.Itoa(pid), "/T"}
		if force {
			args = append(args, "/F")
		}
		return exec.CommandContext(ctx, "taskkill", args...).Run()
	}
	sig := "-TERM"
	if force {
		sig = "-KILL"
	}
	return exec.CommandContext(ctx, "kill", sig, strconv.Itoa(pid)).Run()
}

func waitPidsGone(ctx context.Context, pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			if processAlive(ctx, pid) {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
}

func processAlive(ctx context.Context, pid int) bool {
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(ctx, "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		return err == nil && strings.Contains(strings.ToLower(string(out)), strconv.Itoa(pid))
	}
	err := exec.CommandContext(ctx, "kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil
}

func startAntigravityApp(ctx context.Context, app antigravityApp) error {
	switch runtime.GOOS {
	case "darwin":
		args := []string{"-a", app.path}
		if strings.HasSuffix(strings.ToLower(app.path), ".app") {
			args = []string{app.path}
		}
		if app.path == "" {
			args = []string{"-a", "Antigravity"}
		}
		// Do not use CommandContext here: its cancel after SwitchApp
		// returns can kill a freshly launched client on some platforms.
		if err := exec.Command("open", args...).Run(); err != nil {
			if app.path != "" {
				_ = exec.Command("open", "-n", app.path).Run()
			}
		}
		_ = exec.Command("osascript", "-e", `tell application "Antigravity" to activate`).Run()
		return nil
	case "windows":
		if app.path != "" {
			return exec.Command("cmd", "/c", "start", "", app.path).Run()
		}
		return exec.Command("cmd", "/c", "start", "", "Antigravity").Run()
	default:
		path := app.path
		if path == "" {
			if found, err := exec.LookPath("antigravity"); err == nil {
				path = found
			} else {
				path = "antigravity"
			}
		}
		return exec.Command(path).Start()
	}
}

// applyAntigravityMirrors writes the state database and the file fallbacks
// the client restores a session from. The keychain is the signed-in
// desktop client's truth source; the mirrors keep a restart, a fresh
// launch or an unreachable keyring on the switched account.
func applyAntigravityMirrors(ctx context.Context, email string, doc map[string]any) error {
	if doc == nil {
		return nil
	}
	if err := writeAntigravityState(ctx, email, doc); err != nil {
		return err
	}
	return writeAntigravityFiles(email, doc)
}

// antigravityTokenSet is a login's tokens pulled out of either of
// Antigravity's credential shapes.
type antigravityTokenSet struct {
	access, refresh, idToken string
	expiry                   time.Time
}

func antigravityTokens(doc map[string]any) antigravityTokenSet {
	token := obj(doc["token"])
	if token == nil {
		token = doc
	}
	t := antigravityTokenSet{
		access:  str(token["access_token"]),
		refresh: str(token["refresh_token"]),
	}
	t.idToken = str(token["id_token"])
	if t.idToken == "" {
		t.idToken = str(doc["id_token"])
	}
	if exp, ok := expiryTime(token["expiry"]); ok {
		t.expiry = exp
	} else if exp, ok := expiryTime(doc["expiry_date"]); ok {
		t.expiry = exp
	}
	return t
}

// writeAntigravityFiles refreshes the file fallbacks under ~/.gemini, the
// same trio Antigravity Tools writes: the language service reads
// jetski-standalone-oauth-token when the keyring is unreachable, the CLI
// shape keeps command-line sign-ins on the same account, and
// google_accounts.json names the active account.
func writeAntigravityFiles(email string, doc map[string]any) error {
	tok := antigravityTokens(doc)
	if tok.access == "" && tok.refresh == "" {
		return nil
	}
	dir := filepath.Join(homeDir(), ".gemini")
	standalone := map[string]any{
		"token": map[string]any{
			"access_token": tok.access, "token_type": "Bearer", "refresh_token": tok.refresh,
		},
		"auth_method": "consumer",
	}
	if !tok.expiry.IsZero() {
		standalone["token"].(map[string]any)["expiry"] = tok.expiry.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
		standalone["token"].(map[string]any)["expires_in"] = int64(3599)
	}
	if tok.idToken != "" {
		standalone["id_token"] = tok.idToken
	}
	if email != "" {
		standalone["id_token_email"] = email
	}
	if err := writeGeminiFile(dir, "jetski-standalone-oauth-token", standalone); err != nil {
		return err
	}
	creds := map[string]any{
		"access_token": tok.access, "refresh_token": tok.refresh, "token_type": "Bearer",
		"scope": "https://www.googleapis.com/auth/userinfo.email openid https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.profile",
	}
	if !tok.expiry.IsZero() {
		creds["expiry_date"] = tok.expiry.UnixMilli()
	}
	if tok.idToken != "" {
		creds["id_token"] = tok.idToken
	}
	if err := writeGeminiFile(dir, "oauth_creds.json", creds); err != nil {
		return err
	}
	if email != "" {
		return writeGeminiFile(dir, "google_accounts.json", map[string]any{"active": email, "old": []string{}})
	}
	return nil
}

func writeGeminiFile(dir, name string, doc map[string]any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, name), append(b, '\n'))
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}

// verifySwitched reports whether the live store now holds the selected
// account's login. The client's main process can write the previous
// account back right after the respawned child settles, which must not
// report success.
func (s *Store) verifySwitched(provider, id string, live quota.Account) error {
	doc, err := live.CredentialDoc()
	if err != nil {
		return fmt.Errorf("read back the switched sign-in: %w", err)
	}
	target, err := s.ProfileCredential(provider, id)
	if err != nil {
		return err
	}
	// The client may refresh an expired access token immediately; the
	// refresh token is what identifies the account (Google's never
	// rotates), so either token matching counts.
	if !sameSavedTokens(provider, doc, target) {
		return errors.New("Antigravity went back to the previous sign-in; quit Antigravity fully and switch again")
	}
	return nil
}

// waitAntigravityRunning waits for the desktop client to appear in the
// process list again after a restart. A slow launch only delays the next
// account refresh, so this never fails.
func waitAntigravityRunning(ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		if app, err := findAntigravity(ctx); err == nil && app.running {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// writeAntigravityState updates the desktop client's state database with
// the target token and account. The client keeps an in-memory copy of the
// old account and restores the unified-state mirror on launch; without
// this it writes the old Keychain value back and the UI still queries the
// previous account's quota.
func writeAntigravityState(ctx context.Context, email string, doc map[string]any) error {
	entry := antigravityOAuthEntry(doc)
	status := ""
	if email != "" {
		status = antigravityUserStatus(email)
	}
	if len(entry) == 0 && status == "" {
		return nil
	}
	var firstErr error
	for _, path := range antigravityStateDBs() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := writeAntigravityStateDB(ctx, path, entry, status); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// antigravityStateDBs lists the desktop client's state databases, the
// classic client first. The IDE variant keeps its own copy under a
// differently named folder; whichever exists gets the mirror.
func antigravityStateDBs() []string {
	var base string
	var dirs []string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(os.Getenv("HOME"), "Library", "Application Support")
		dirs = []string{"Antigravity", "Antigravity IDE", "antigravity-ide"}
	case "windows":
		base = os.Getenv("APPDATA")
		dirs = []string{"Antigravity", "Antigravity IDE"}
	default:
		base = filepath.Join(os.Getenv("HOME"), ".config")
		dirs = []string{"Antigravity", "antigravity-ide", "antigravity_ide"}
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(base, d, "User", "globalStorage", "state.vscdb"))
	}
	return out
}

func writeAntigravityStateDB(ctx context.Context, path string, oauthEntry []byte, userStatus string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return fmt.Errorf("open the Antigravity state database: %w", err)
	}
	defer db.Close()
	if _, err := db.Execute(ctx, "PRAGMA busy_timeout=2000"); err != nil {
		return fmt.Errorf("lock the Antigravity state database: %w", err)
	}
	set := func(key, value string) error {
		if _, err := db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", key, value); err != nil {
			return fmt.Errorf("write %s in the Antigravity state database: %w", key, err)
		}
		return nil
	}
	remove := func(key string) error {
		if _, err := db.Execute(ctx, "DELETE FROM ItemTable WHERE key = ?", key); err != nil {
			return fmt.Errorf("clear %s in the Antigravity state database: %w", key, err)
		}
		return nil
	}
	if len(oauthEntry) > 0 {
		// The row holds one protobuf map entry per sentinel key: drop the
		// old oauthTokenInfo entry and keep every other one, or rewriting
		// the row loses the rest of the topic.
		merged := mergeAntigravityTopic(readAntigravityTopic(ctx, db), oauthEntry)
		if err := set("antigravityUnifiedStateSync.oauthToken", base64.StdEncoding.EncodeToString(merged)); err != nil {
			return err
		}
	}
	if userStatus != "" {
		if err := set("antigravityUnifiedStateSync.userStatus", userStatus); err != nil {
			return err
		}
	}
	// A stale init state pins the previous user id and breaks history
	// fetching after a switch; the client rebuilds it.
	if err := remove("jetskiStateSync.agentManagerInitState"); err != nil {
		return err
	}
	// An enterprise project preference left by a previous account breaks a
	// personal one, and this app only switches personal accounts.
	if err := remove("antigravityUnifiedStateSync.enterprisePreferences"); err != nil {
		return err
	}
	// A switch without the flag shows the first-run Security Notice on the
	// next reconnect.
	return set("antigravityOnboarding", "true")
}

func readAntigravityTopic(ctx context.Context, db *sqlite.DB) []byte {
	rows, err := db.Query(ctx, "SELECT value FROM ItemTable WHERE key = ?", "antigravityUnifiedStateSync.oauthToken")
	if err != nil || len(rows.Values) == 0 || len(rows.Values[0]) == 0 {
		return nil
	}
	text, ok := rows.Values[0][0].(string)
	if !ok {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil
	}
	return raw
}

// mergeAntigravityTopic replaces the oauthTokenInfoSentinelKey entry in the
// client's unified-state topic while keeping every other sentinel row.
func mergeAntigravityTopic(current, entry []byte) []byte {
	out := make([]byte, 0, len(current)+len(entry))
	for _, f := range pbFields(current) {
		if f.wire == 2 && antigravityTopicEntryKey(f.data) == "oauthTokenInfoSentinelKey" {
			continue
		}
		out = append(out, current[f.start:f.end]...)
	}
	return append(out, entry...)
}

// antigravityTopicEntryKey reads the sentinel key of one Topic.data entry
// (field 1 key, field 2 row).
func antigravityTopicEntryKey(entry []byte) string {
	for _, f := range pbFields(entry) {
		if f.num == 1 && f.wire == 2 {
			return string(f.data)
		}
	}
	return ""
}

// pbField is one top-level field of a protobuf message.
type pbField struct {
	num, wire  int
	start, end int
	data       []byte // wire type 2 payload
}

// pbFields walks a protobuf message. It stops at the first malformed byte,
// which yields the readable prefix.
func pbFields(data []byte) []pbField {
	var out []pbField
	i := 0
	for i < len(data) {
		tag, n := binary.Uvarint(data[i:])
		if n <= 0 {
			break
		}
		start := i
		i += n
		num, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			_, n = binary.Uvarint(data[i:])
			if n <= 0 {
				return out
			}
			i += n
		case 1:
			i += 8
		case 2:
			l, n := binary.Uvarint(data[i:])
			if n <= 0 {
				return out
			}
			i += n
			if l > uint64(len(data)-i) {
				return out
			}
			out = append(out, pbField{num: num, wire: wire, start: start, end: i + int(l), data: data[i : i+int(l)]})
			i += int(l)
			continue
		case 5:
			i += 4
		default:
			return out
		}
		out = append(out, pbField{num: num, wire: wire, start: start, end: i})
	}
	return out
}

func antigravityUserStatus(email string) string {
	payload := append(pbString(3, email), pbString(7, email)...)
	row := pbString(1, base64.StdEncoding.EncodeToString(payload))
	entry := append(pbString(1, "userStatusSentinelKey"), pbLen(2, row)...)
	topic := pbLen(1, entry)
	return base64.StdEncoding.EncodeToString(topic)
}

func pbString(field int, value string) []byte {
	return pbLen(field, []byte(value))
}

func pbLen(field int, value []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3|2))
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

// antigravityOAuthEntry builds the unified-state oauthTokenInfo row for the
// account's credential document. It returns the raw topic bytes; the row is
// one map entry keyed by oauthTokenInfoSentinelKey. The client's own
// GCP enterprise flag has no counterpart here, so it stays unset, which is
// what personal accounts carry.
func antigravityOAuthEntry(doc map[string]any) []byte {
	tok := antigravityTokens(doc)
	if tok.access == "" || tok.refresh == "" {
		return nil
	}
	timestamp := binary.AppendUvarint(nil, uint64(1<<3|0))
	timestamp = binary.AppendUvarint(timestamp, uint64(max(0, tok.expiry.Unix())))
	timestamp = binary.AppendUvarint(timestamp, uint64(2<<3|0))
	timestamp = binary.AppendUvarint(timestamp, 0)
	info := pbString(1, tok.access)
	info = append(info, pbString(2, "Bearer")...)
	info = append(info, pbString(3, tok.refresh)...)
	info = append(info, pbLen(4, timestamp)...)
	if tok.idToken != "" {
		info = append(info, pbString(5, tok.idToken)...)
	}
	row := pbString(1, base64.StdEncoding.EncodeToString(info))
	entry := append(pbString(1, "oauthTokenInfoSentinelKey"), pbLen(2, row)...)
	return pbLen(1, entry)
}
