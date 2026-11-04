package accounts

import (
	"context"
	"encoding/base64"
	"encoding/binary"
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
// Tools also falls back to for the IDE and older clients.
func (s *Store) SwitchApp(ctx context.Context, provider, id string, live quota.Account) error {
	if provider != "antigravity" {
		return s.Switch(provider, id, live)
	}
	app, err := findAntigravity(ctx)
	if err != nil {
		return err
	}
	if !app.running {
		return s.Switch(provider, id, live)
	}
	// Native Antigravity keeps its window and an Electron supervisor:
	// write the keychain, kill only language_server, and let the supervisor
	// start a child with the new credential. Restarting the whole app
	// re-triggers its first-run Security Notice.
	targetDoc, _ := s.ProfileCredential(provider, id)
	targetEmail := s.ProfileEmail(provider, id)
	if len(languageServers(ctx, app)) > 0 && !strings.Contains(strings.ToLower(app.path), "antigravity ide") {
		if err := s.Switch(provider, id, live); err != nil {
			return err
		}
		old := languageServers(ctx, app)
		killLanguageServer(ctx, old)
		writeAntigravityState(ctx, targetEmail, targetDoc)
		_ = s.Reapply(provider, id, live)
		if waitLanguageServerRespawn(ctx, app, old, 15*time.Second) {
			// The main process may write its in-memory token back just after
			// the child starts; reassert once more after it settles.
			time.Sleep(time.Second)
			writeAntigravityState(ctx, targetEmail, targetDoc)
			_ = s.Reapply(provider, id, live)
			return nil
		}
		// No supervisor respawn: fall back to the authoritative full
		// restart, which also avoids a window without an engine.
	}
	if err := stopAntigravity(ctx, app); err != nil {
		return fmt.Errorf("quit Antigravity: %w", err)
	}
	writeAntigravityState(ctx, targetEmail, targetDoc)
	switched := s.Reapply(provider, id, live)
	if switched == nil {
		switched = s.Switch(provider, id, live)
	}
	if err := startAntigravity(ctx, app); err != nil {
		if switched != nil {
			return fmt.Errorf("%v; reopening Antigravity failed: %w", switched, err)
		}
		return fmt.Errorf("sign-in switched, but reopening Antigravity failed: %w", err)
	}
	return switched
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

// writeAntigravityOnboarding marks the switched account as onboarded. The
// desktop client keeps this flag in its state database; a switch without
// it shows the Security Notice on the next reconnect.
func writeAntigravityOnboarding(ctx context.Context, email string) {
	if email == "" {
		return
	}
	path := antigravityStateDB()
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Execute(ctx, "PRAGMA busy_timeout=2000")
	_, _ = db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", "antigravityUnifiedStateSync.userStatus", antigravityUserStatus(email))
	_, _ = db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", "antigravityOnboarding", "true")
}

func antigravityStateDB() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Antigravity", "User", "globalStorage", "state.vscdb")
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "Antigravity", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(os.Getenv("HOME"), ".config", "Antigravity", "User", "globalStorage", "state.vscdb")
	}
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

// writeAntigravityState updates the desktop client's state database with
// the target token and account. The client keeps an in-memory copy of the
// old account; without this it writes the old Keychain value back and the
// UI still queries the previous account's quota.
func writeAntigravityState(ctx context.Context, email string, doc map[string]any) {
	if email == "" || doc == nil {
		return
	}
	path := antigravityStateDB()
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Execute(ctx, "PRAGMA busy_timeout=2000")
	if tok := antigravityOAuthEntry(doc); tok != "" {
		_, _ = db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", "antigravityUnifiedStateSync.oauthToken", tok)
	}
	_, _ = db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", "antigravityUnifiedStateSync.userStatus", antigravityUserStatus(email))
	_, _ = db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", "antigravityOnboarding", "true")
}

func antigravityOAuthEntry(doc map[string]any) string {
	token := obj(doc["token"])
	if token == nil {
		token = doc
	}
	access := str(token["access_token"])
	refresh := str(token["refresh_token"])
	idToken := str(doc["id_token"])
	if idToken == "" {
		idToken = str(token["id_token"])
	}
	if access == "" || refresh == "" {
		return ""
	}
	seconds := int64(0)
	if exp, ok := expiryTime(token["expiry"]); ok {
		seconds = exp.Unix()
	} else if exp, ok := expiryTime(doc["expiry_date"]); ok {
		seconds = exp.Unix()
	}
	timestamp := binary.AppendUvarint(nil, uint64(1<<3|0))
	timestamp = binary.AppendUvarint(timestamp, uint64(max(0, seconds)))
	timestamp = binary.AppendUvarint(timestamp, uint64(2<<3|0))
	timestamp = binary.AppendUvarint(timestamp, 0)
	info := pbString(1, access)
	info = append(info, pbString(2, "Bearer")...)
	info = append(info, pbString(3, refresh)...)
	info = append(info, pbLen(4, timestamp)...)
	if idToken != "" {
		info = append(info, pbString(5, idToken)...)
	}
	row := pbString(1, base64.StdEncoding.EncodeToString(info))
	entry := append(pbString(1, "oauthTokenInfoSentinelKey"), pbLen(2, row)...)
	topic := pbLen(1, entry)
	return base64.StdEncoding.EncodeToString(topic)
}
