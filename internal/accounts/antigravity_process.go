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
	ide     bool
	pids    []int
	path    string // .app bundle on macOS, install/executable path elsewhere
	exec    string // exact executable captured before the client is stopped
}

// These are variables so tests never touch a real Antigravity.
var (
	findAntigravity       = discoverAntigravity
	stopAntigravity       = stopAntigravityApp
	startAntigravity      = startAntigravityApp
	languageServers       = listLanguageServers
	killLanguageServer    = killPids
	writeSystemCredential = quota.WriteAntigravityKeychain
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

// writeTargetSystemCredential writes the selected login to the system
// credential store independently of which mirror discovery surfaced as the
// live account. Antigravity 2.x and the agy language service treat that
// store as authoritative; writing only oauth_creds.json or state.vscdb is
// the upstream "switch looks successful but stays on the old account" case.
func writeTargetSystemCredential(ctx context.Context, targetDoc map[string]any) error {
	doc := adaptCredential("antigravity", targetDoc, "file", "keychain")
	doc = switchDocument("antigravity", doc)
	err := writeSystemCredential(doc)
	syncdiag.Log(ctx, "account_switch_system_credential", "error", DiagnosticErrorCode(err))
	if err != nil {
		// Upstream keeps a legacy Linux client switchable when libsecret is
		// absent or its D-Bus session is unreachable, provided the SQLite
		// state mirror exists. Without that escape a pre-2.0 install would
		// regress from a working SQLite switch to an unconditional error.
		if runtime.GOOS == "linux" && hasAntigravityStateDB() {
			syncdiag.Log(ctx, "account_switch_system_credential_fallback", "fallback", "state_db")
			return nil
		}
		return fmt.Errorf("write Antigravity's system credential store: %w", err)
	}
	return nil
}

func hasAntigravityStateDB() bool {
	for _, path := range antigravityStateDBs() {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
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
	backupAntigravityStateDBs()
	if !app.running {
		syncdiag.Log(ctx, "account_switch_mode", "mode", "client_closed")
		// The client is closed: nothing competes for the stores. Write the
		// keychain plus every mirror the client restores a session from, or
		// the next launch brings the previous account back.
		if err := s.Switch(provider, id, live); err != nil {
			return err
		}
		if err := writeTargetSystemCredential(ctx, targetDoc); err != nil {
			return err
		}
		return applyAntigravityMirrors(ctx, targetEmail, targetDoc)
	}
	// Classic Antigravity and the IDE both keep their window and a
	// supervisor: write every credential store, kill only the language
	// service (language_server or agy), and let the supervisor start a child
	// with the new credential. A full restart remains the fallback.
	if len(languageServers(ctx, app)) > 0 {
		syncdiag.Log(ctx, "account_switch_mode", "mode", "language_service_restart")
		// Capture the current login and write the target before stopping the
		// child: the supervisor respawns it within seconds and the new child
		// reads the keychain on start.
		if err := s.Switch(provider, id, live); err != nil {
			return err
		}
		if err := writeTargetSystemCredential(ctx, targetDoc); err != nil {
			return err
		}
		// Seed the state/file mirrors before stopping the child, matching
		// upstream. The running client can hold the database briefly, so the
		// post-kill write below remains the authoritative one.
		preMirrorErr := applyAntigravityMirrors(ctx, targetEmail, targetDoc)
		syncdiag.Log(ctx, "account_switch_mirrors_prekill", "error", DiagnosticErrorCode(preMirrorErr))
		old := languageServers(ctx, app)
		killLanguageServer(ctx, old)
		// The child is gone; this write is the one the respawned child
		// reads, and it must hold before reporting anything.
		if err := s.Reapply(provider, id, live); err != nil {
			return fmt.Errorf("assert the new sign-in after stopping the language service: %w", err)
		}
		if err := writeTargetSystemCredential(ctx, targetDoc); err != nil {
			return err
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
			if err := writeTargetSystemCredential(ctx, targetDoc); err != nil {
				return err
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
	if werr := s.switchAfterQuit(provider, id, live); werr != nil {
		return fmt.Errorf("write the selected sign-in: %w", werr)
	}
	if err := writeTargetSystemCredential(ctx, targetDoc); err != nil {
		return err
	}
	if err := applyAntigravityMirrors(ctx, targetEmail, targetDoc); err != nil {
		return err
	}
	sweepOrphanLanguageServers(ctx, app)
	if err := startAntigravity(ctx, app); err != nil {
		return fmt.Errorf("sign-in switched, but reopening Antigravity failed: %w", err)
	}
	// The restart is part of the switch: give the client time to show up
	// again before the caller refreshes accounts, or its card is missing
	// until the next periodic refresh.
	waitAntigravityRunning(ctx, 15*time.Second)
	return nil
}

// processInfo is the small cross-platform process snapshot the switcher
// needs. Windows comes from CIM because tasklist has neither parent ids nor
// command lines; Unix comes from ps.
type processInfo struct {
	pid, ppid  int
	name, path string
	command    string
}

func listLanguageServers(ctx context.Context, app antigravityApp) []int {
	return selectLanguageServers(listSystemProcesses(ctx), app)
}

// selectLanguageServers follows the running client's process tree and only
// then falls back to install-path matching. The tree walk is what upstream
// relies on for IDE installs in custom directories: a broad path match can
// miss those and leave the old language service holding the old account.
func selectLanguageServers(procs []processInfo, app antigravityApp) []int {
	descendants := map[int]bool{}
	frontier := append([]int(nil), app.pids...)
	for _, pid := range frontier {
		descendants[pid] = true
	}
	for len(frontier) > 0 {
		parent := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, p := range procs {
			if p.ppid == parent && !descendants[p.pid] {
				descendants[p.pid] = true
				frontier = append(frontier, p.pid)
			}
		}
	}
	var pids []int
	seen := map[int]bool{}
	for _, p := range procs {
		if descendants[p.pid] && isLanguageServerProcess(p) && !seen[p.pid] {
			seen[p.pid] = true
			pids = append(pids, p.pid)
		}
	}
	if len(pids) > 0 {
		return pids
	}
	for _, p := range procs {
		if isLanguageServerProcess(p) && matchesLanguageServerTarget(p, app) && !seen[p.pid] {
			seen[p.pid] = true
			pids = append(pids, p.pid)
		}
	}
	return pids
}

func isLanguageServerProcess(p processInfo) bool {
	text := strings.ToLower(p.name + " " + p.path + " " + p.command)
	return strings.Contains(text, "language_server") || isAgyExecutable(p.name) || isAgyExecutable(p.path) || isAgyExecutable(firstCommandToken(p.command))
}

func isAgyExecutable(s string) bool {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), `"`))
	base := s
	if at := strings.LastIndexAny(s, `/\\`); at >= 0 {
		base = s[at+1:]
	}
	return base == "agy" || base == "agy.exe"
}

func firstCommandToken(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// matchesLanguageServerTarget keeps classic and IDE language services
// isolated. An agy hub belongs to the IDE; a standalone agy CLI is never a
// hot-switch victim merely because it shares the executable name.
func matchesLanguageServerTarget(p processInfo, app antigravityApp) bool {
	lowerCommand := strings.ToLower(p.command)
	if isAgyExecutable(p.name) || isAgyExecutable(p.path) || isAgyExecutable(firstCommandToken(p.command)) {
		return app.ide && (strings.Contains(lowerCommand, "--hub") || strings.Contains(lowerCommand, "--app_data_dir=antigravity"))
	}
	exe := processExecutable(p)
	if prefix := installPrefix(app); prefix != "" {
		path, command := normalizeProcessPath(exe), normalizeProcessPath(firstCommandToken(p.command))
		if strings.HasPrefix(path, prefix) || strings.HasPrefix(command, prefix) {
			return true
		}
	}
	// Workspace or file arguments are not client identity. Only the
	// executable name/path may decide whether an orphaned language server
	// belongs to this target; otherwise an editor opening a folder named
	// antigravity-ide-demo can become a kill target.
	identity := strings.ToLower(p.name + " " + exe)
	if isAntigravityIDEString(identity) {
		return app.ide
	}
	return strings.Contains(identity, "antigravity") && !app.ide
}

func processExecutable(p processInfo) string {
	if p.path != "" {
		return p.path
	}
	if exe := executableFromCommand(p.command); exe != "" {
		return exe
	}
	return firstCommandToken(p.command)
}

func normalizeProcessPath(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), `\`, `/`)
}

func installPrefix(app antigravityApp) string {
	base := app.path
	if base == "" {
		base = app.exec
	}
	if base == "" {
		return ""
	}
	lower := strings.ToLower(base)
	bundle := false
	if runtime.GOOS == "darwin" {
		if at := strings.Index(lower, ".app"); at >= 0 {
			base = base[:at+4]
			bundle = true
		}
	}
	if !bundle && !strings.HasSuffix(lower, "/") && !strings.HasSuffix(lower, `\`) {
		if at := strings.LastIndexAny(base, `/\\`); at >= 0 {
			base = base[:at]
		}
	}
	prefix := normalizeProcessPath(base)
	return strings.TrimSuffix(prefix, "/") + "/"
}

// sweepOrphanLanguageServers removes the target client's leftover language
// service before a full restart. A detached child can keep its old token and
// the engine port after the main window exits, making the restart reopen on
// the previous account.
func sweepOrphanLanguageServers(ctx context.Context, app antigravityApp) {
	for _, p := range listSystemProcesses(ctx) {
		if isLanguageServerProcess(p) && matchesLanguageServerTarget(p, app) {
			_ = signalProcess(ctx, p.pid, true)
		}
	}
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
	return discoverAntigravityFromProcesses(listSystemProcesses(ctx)), nil
}

func discoverWindowsAntigravity(ctx context.Context) (antigravityApp, error) {
	procs := listSystemProcesses(ctx)
	if len(procs) == 0 {
		return discoverWindowsAntigravityTasklist(ctx)
	}
	app := discoverAntigravityFromProcesses(procs)
	if app.running && app.path == "" {
		for _, p := range windowsAntigravityInstalls(app.ide) {
			if _, err := os.Stat(p); err == nil {
				app.path = p
				if app.exec == "" {
					app.exec = p
				}
				break
			}
		}
	}
	return app, nil
}

func windowsAntigravityInstalls(ide bool) []string {
	type variant struct {
		folder string
		exes   []string
	}
	classic := variant{"Antigravity", []string{"Antigravity.exe", "antigravity.exe"}}
	ideVariant := variant{"Antigravity IDE", []string{"Antigravity IDE.exe", "antigravity-ide.exe", "antigravity_ide.exe", "Antigravity.exe"}}
	variants := []variant{classic, ideVariant}
	if ide {
		variants = []variant{ideVariant, classic}
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	programFilesX86 := os.Getenv("ProgramFiles(x86)")
	if programFilesX86 == "" {
		programFilesX86 = `C:\Program Files (x86)`
	}
	roots := []string{programFiles, programFilesX86}
	if w64 := os.Getenv("ProgramW6432"); w64 != "" {
		roots = append(roots, w64)
	}
	var out []string
	for _, v := range variants {
		for _, exe := range v.exes {
			if local := os.Getenv("LOCALAPPDATA"); local != "" {
				out = append(out,
					filepath.Join(local, "Programs", v.folder, exe),
					filepath.Join(local, v.folder, exe),
				)
			}
			for _, root := range roots {
				out = append(out, filepath.Join(root, v.folder, exe))
			}
		}
	}
	return out
}

func discoverAntigravityFromProcesses(procs []processInfo) antigravityApp {
	var classic, ide antigravityApp
	for _, p := range procs {
		kind := antigravityProcessKind(p)
		if kind == "" {
			continue
		}
		app := &classic
		if kind == "ide" {
			app = &ide
			app.ide = true
		}
		app.running = true
		app.pids = append(app.pids, p.pid)
		exe := p.path
		if exe == "" {
			exe = executableFromCommand(p.command)
		}
		if exe != "" && app.exec == "" {
			app.exec = exe
		}
		if bundle := appBundle(firstNonEmpty(exe, p.command)); bundle != "" {
			app.path = bundle
		} else if app.path == "" {
			app.path = exe
		}
	}
	// Upstream keeps the classic desktop client authoritative when both
	// variants are running; the IDE is selected only when classic is absent.
	if classic.running {
		return classic
	}
	return ide
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func executableFromCommand(command string) string {
	command = strings.TrimSpace(command)
	if strings.HasPrefix(command, `"`) {
		if end := strings.Index(command[1:], `"`); end >= 0 {
			return command[1 : 1+end]
		}
	}
	// Windows command lines need not quote or even still exist at their
	// executable path (an update can replace it). The .exe suffix is the
	// reliable token boundary there, including paths containing spaces.
	withoutQuote := strings.TrimPrefix(command, `"`)
	if len(withoutQuote) > 2 && withoutQuote[1] == ':' {
		if at := strings.Index(strings.ToLower(withoutQuote), ".exe"); at >= 0 {
			return withoutQuote[:at+len(".exe")]
		}
	}
	fields := strings.Fields(command)
	for n := len(fields); n > 0; n-- {
		candidate := strings.Trim(strings.Join(fields[:n], " "), `"`)
		if !strings.HasPrefix(candidate, "/") && !strings.Contains(candidate, `:\`) {
			break
		}
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}

func listSystemProcesses(ctx context.Context) []processInfo {
	if runtime.GOOS == "windows" {
		return listWindowsProcesses(ctx)
	}
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	var procs []processInfo
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		command := strings.Join(fields[2:], " ")
		name := firstCommandToken(command)
		if at := strings.LastIndexAny(name, `/\\`); at >= 0 {
			name = name[at+1:]
		}
		procs = append(procs, processInfo{pid: pid, ppid: ppid, name: name, command: command})
	}
	return procs
}

type windowsProcessJSON struct {
	ProcessID       int    `json:"ProcessId"`
	ParentProcessID int    `json:"ParentProcessId"`
	Name            string `json:"Name"`
	ExecutablePath  string `json:"ExecutablePath"`
	CommandLine     string `json:"CommandLine"`
}

func listWindowsProcesses(ctx context.Context) []processInfo {
	const script = `Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,Name,ExecutablePath,CommandLine | ConvertTo-Json -Compress`
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	var rows []windowsProcessJSON
	if err := json.Unmarshal(out, &rows); err != nil {
		var one windowsProcessJSON
		if json.Unmarshal(out, &one) != nil || one.ProcessID == 0 {
			return nil
		}
		rows = []windowsProcessJSON{one}
	}
	procs := make([]processInfo, 0, len(rows))
	for _, row := range rows {
		procs = append(procs, processInfo{pid: row.ProcessID, ppid: row.ParentProcessID, name: row.Name, path: row.ExecutablePath, command: firstNonEmpty(row.CommandLine, row.ExecutablePath, row.Name)})
	}
	return procs
}

func discoverWindowsAntigravityTasklist(ctx context.Context) (antigravityApp, error) {
	out, err := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return antigravityApp{}, nil
	}
	var classic, ide antigravityApp
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.Trim(line, "\r\n"), `","`)
		if len(fields) < 2 {
			continue
		}
		name := strings.Trim(fields[0], `"`)
		pid, err := strconv.Atoi(strings.Trim(fields[1], `"`))
		if err != nil || antigravityProcessKind(processInfo{name: name, command: name}) == "" {
			continue
		}
		app := &classic
		if isAntigravityIDEString(name) {
			app = &ide
			app.ide = true
		}
		app.running = true
		app.pids = append(app.pids, pid)
	}
	app := classic
	if !app.running {
		app = ide
	}
	if app.running {
		for _, p := range windowsAntigravityInstalls(app.ide) {
			if _, err := os.Stat(p); err == nil {
				app.path, app.exec = p, p
				break
			}
		}
	}
	return app, nil
}

// antigravityProcessKind classifies only the executable identity, matching
// upstream's rule that a workspace path or terminal command must never make
// an unrelated editor an Antigravity client. Command-line arguments are used
// only as a classic/IDE discriminator for custom IDE installs.
func antigravityProcessKind(p processInfo) string {
	exe := p.path
	if exe == "" {
		exe = executableFromCommand(p.command)
	}
	if exe == "" {
		exe = firstCommandToken(p.command)
	}
	name := strings.ToLower(p.name)
	exeLower := strings.ToLower(exe)
	if name == "" {
		name = exeLower
		if at := strings.LastIndexAny(name, `/\\`); at >= 0 {
			name = name[at+1:]
		}
	}
	identity := name + " " + exeLower
	if !strings.Contains(identity, "antigravity") {
		return ""
	}
	for _, other := range []string{"antigravity tools", "antigravity-tools", "antigravitytools", "antigravity_manager", "antigravity-manager", "antigravitymanager", "antigravity manager", "antigravity-cli", "updater", "auto-updater", "agent usage metrics"} {
		if strings.Contains(identity, other) {
			return ""
		}
	}
	if isHelperProcess(p, exe) {
		return ""
	}
	if isAntigravityIDEString(identity) {
		return "ide"
	}
	if isAntigravityIDEString(chromiumUserDataDir(strings.ToLower(p.command))) {
		return "ide"
	}
	return "classic"
}

// isHelperProcess mirrors upstream's helper classifier: process flags can
// mark a helper, but ordinary words such as "gpu" or "plugin" only count in
// the executable identity. Otherwise a main window opened with a workspace
// or flag containing one of those words disappears from discovery.
func isHelperProcess(p processInfo, exe string) bool {
	name := strings.ToLower(p.name)
	args := strings.ToLower(p.command)
	exeLower := strings.ToLower(firstNonEmpty(exe, p.path))
	for _, marker := range []string{"--type=", "node-ipc", "nodeipc", "max-old-space-size", "node_modules", "--standalone", "--subclient_type", "--override_ide_name", "language_server"} {
		if strings.Contains(args, marker) {
			return true
		}
	}
	for _, marker := range []string{"helper", "plugin", "renderer", "gpu", "crashpad", "utility", "audio", "sandbox", "language_server"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return strings.Contains(exeLower, "crashpad") || strings.Contains(exeLower, "helper") || strings.Contains(exeLower, "language_server") || isLanguageServerProcess(p)
}

// chromiumUserDataDir extracts the Chromium user-data-dir value after the
// shell's quotes have been lost. Only that value may discriminate IDE from
// classic; a workspace named antigravity-ide-demo must not change identity.
func chromiumUserDataDir(command string) string {
	at := strings.Index(command, "user-data-dir")
	if at < 0 {
		return ""
	}
	rest := strings.TrimLeft(command[at+len("user-data-dir"):], "=: ")
	rest = strings.TrimPrefix(rest, `"`)
	end := len(rest)
	if at := strings.Index(rest, `" --`); at >= 0 {
		end = at
	} else if at := strings.Index(rest, " --"); at >= 0 {
		end = at
	}
	return strings.TrimRight(rest[:end], `" `)
}

func isAntigravityIDEString(s string) bool {
	s = strings.ToLower(s)
	for _, marker := range []string{"antigravity ide", "antigravity-ide", "antigravity_ide", "antigravityide", "antigravity.ide"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func appBundle(command string) string {
	lower := strings.ToLower(command)
	for at := strings.Index(lower, ".app"); at >= 0; {
		end := at + len(".app")
		if end == len(command) || command[end] == '/' || command[end] == '\\' {
			return command[:end]
		}
		next := strings.Index(lower[end:], ".app")
		if next < 0 {
			break
		}
		at = end + next
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
	seen := map[int]bool{}
	for _, pid := range pids {
		seen[pid] = true
	}
	procs := listSystemProcesses(ctx)
	// Electron nests helpers several levels below the window process; take
	// the whole descendant tree rather than only the bundle path.
	frontier := append([]int(nil), app.pids...)
	for len(frontier) > 0 {
		parent := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, p := range procs {
			if p.ppid == parent && !seen[p.pid] {
				seen[p.pid] = true
				pids = append(pids, p.pid)
				frontier = append(frontier, p.pid)
			}
		}
	}
	prefix := installPrefix(app)
	if prefix == "" {
		return pids
	}
	for _, p := range procs {
		path, command := normalizeProcessPath(p.path), normalizeProcessPath(p.command)
		if (strings.HasPrefix(path, prefix) || strings.HasPrefix(command, prefix)) && !seen[p.pid] {
			seen[p.pid] = true
			pids = append(pids, p.pid)
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
		out, err := exec.CommandContext(ctx, "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(strings.Trim(line, "\r\n"), `","`)
			if len(fields) > 1 && strings.Trim(fields[1], `"`) == strconv.Itoa(pid) {
				return true
			}
		}
		return false
	}
	err := exec.CommandContext(ctx, "kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil
}

func startAntigravityApp(ctx context.Context, app antigravityApp) error {
	switch runtime.GOOS {
	case "darwin":
		target := firstNonEmpty(app.path, app.exec)
		if bundle := appBundle(target); bundle != "" {
			target = bundle
		}
		// Do not use CommandContext here: its cancel after SwitchApp
		// returns can kill a freshly launched client on some platforms.
		if target != "" {
			if strings.HasSuffix(strings.ToLower(target), ".app") {
				if err := exec.Command("open", target).Run(); err != nil {
					return err
				}
			} else if err := exec.Command(target).Start(); err != nil {
				return err
			}
		} else if err := exec.Command("open", "-a", "Antigravity").Run(); err != nil {
			return err
		}
		name := "Antigravity"
		if app.ide {
			name = "Antigravity IDE"
		}
		_ = exec.Command("osascript", "-e", `tell application "`+name+`" to activate`).Run()
		return nil
	case "windows":
		if target := firstNonEmpty(app.exec, app.path); target != "" {
			cmd := exec.Command(target)
			cmd.Dir = filepath.Dir(target)
			return cmd.Start()
		}
		if app.ide {
			return exec.Command("cmd", "/c", "start", "", "Antigravity IDE").Run()
		}
		return exec.Command("cmd", "/c", "start", "", "Antigravity").Run()
	default:
		path := firstNonEmpty(app.exec, app.path)
		if path == "" {
			names := []string{"antigravity"}
			if app.ide {
				names = []string{"antigravity-ide", "antigravity_ide", "Antigravity IDE"}
			}
			for _, name := range names {
				if found, err := exec.LookPath(name); err == nil {
					path = found
					break
				}
			}
			if path == "" {
				path = names[0]
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
		if err := writeAntigravityStateDB(ctx, path, entry, status, email); err != nil && firstErr == nil {
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
	dirs := []string{"Antigravity", "Antigravity IDE", "antigravity-ide", "antigravity_ide"}
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(os.Getenv("HOME"), "Library", "Application Support")
	case "windows":
		base = os.Getenv("APPDATA")
	default:
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(base, d, "User", "globalStorage", "state.vscdb"))
	}
	return out
}

func backupAntigravityStateDBs() {
	for _, path := range antigravityStateDBs() {
		if raw, err := os.ReadFile(path); err == nil {
			_ = writePrivate(path+".backup", raw)
		}
	}
}

func writeAntigravityStateDB(ctx context.Context, path string, oauthEntry []byte, userStatus, email string) error {
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
	// next reconnect. The IDE additionally keys that notice and its login
	// state off these onboarding rows; upstream writes both families.
	if err := set("antigravityOnboarding", "true"); err != nil {
		return err
	}
	if email != "" {
		// These rows are advisory mirrors, so a locked row must not undo the
		// authoritative token write above.
		_ = set("agy.onboarding.lastLoginUsername", email)
		_ = set("agy.onboarding.lastLoginIsGcpTos", "false")
		_ = set("jetski.onboarding.lastLoginUsername", email)
		_ = set("jetski.onboarding.lastLoginIsGcpTos", "false")
	}
	return nil
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

// switchAfterQuit writes the target once the client has quit.
func (s *Store) switchAfterQuit(provider, id string, live quota.Account) error {
	werr := s.Switch(provider, id, live)
	// Only a write race or a failed write may fall back to the plain
	// rewrite, so a half-finished switch still ends on the target. Any other
	// error is Switch refusing the target (a copy of another account, a
	// mismatched identity, switching off) and must stand; falling back on
	// every error wrote exactly the copies Switch had just refused.
	if errors.Is(werr, errLiveChanged) || errors.Is(werr, errWriteFailed) {
		werr = s.Reapply(provider, id, live)
	}
	return werr
}
