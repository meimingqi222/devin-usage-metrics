package ext

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestApplyMCPTranslatesJSONToTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(home, ".claude.json")
	src := `{
  // keep
  "numStartups": 3,
  "mcpServers": {
    "github": {"command":"npx","args":["-y","@modelcontextprotocol/server-github"],"env":{"TOKEN":"abc"}}
  }
}
`
	if err := os.WriteFile(claude, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codex, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte("# header\nmodel = \"x\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryMCP()
	if err != nil || len(rows) != 1 || rows[0].Name != "github" {
		t.Fatalf("inventory %v %#v", err, rows)
	}
	srcMCP := rows[0].ByAgent[model.Claude].MCP
	if err := ApplyMCP("github", srcMCP, []model.Agent{model.Codex}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(claude)
	if !strings.Contains(string(got), "// keep") || !strings.Contains(string(got), `"numStartups": 3`) {
		t.Fatalf("claude rewritten:\n%s", got)
	}
	toml, _ := os.ReadFile(filepath.Join(codex, "config.toml"))
	text := string(toml)
	if !strings.Contains(text, "# header") || !strings.Contains(text, `model = "x"`) {
		t.Fatalf("codex preamble lost:\n%s", text)
	}
	if !strings.Contains(text, "[mcp_servers.github]") || !strings.Contains(text, `command = "npx"`) {
		t.Fatalf("codex missing server:\n%s", text)
	}
	rows, err = InventoryMCP()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Drift {
		t.Fatalf("clone drifted: %#v", rows[0])
	}
	if _, ok := rows[0].ByAgent[model.Codex]; !ok {
		t.Fatal("codex not mounted")
	}
}

func TestMCPDriftAndUndo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"github":{"command":"npx"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("[mcp_servers.github]\ncommand = \"uvx\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryMCP()
	if err != nil || !rows[0].Drift {
		t.Fatalf("expected drift: %v %#v", err, rows)
	}
	if drifted := rows[0].DriftedAgents(); len(drifted) != 1 || drifted[0] != model.Codex {
		t.Fatalf("drifted agents: %v", drifted)
	}
	if src, ok := rows[0].SourcePlacement(); !ok || src.Agent != model.Claude {
		t.Fatalf("source placement: %#v %v", src, ok)
	}
	if err := RemoveMCP("github", []model.Agent{model.Codex}); err != nil {
		t.Fatal(err)
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if strings.Contains(string(toml), "mcp_servers.github") {
		t.Fatalf("still present:\n%s", toml)
	}
	if err := UndoLast(); err != nil {
		t.Fatal(err)
	}
	toml, _ = os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if !strings.Contains(string(toml), `command = "uvx"`) {
		t.Fatalf("undo missed toml:\n%s", toml)
	}
}

func TestAntigravitySkipsSSE(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	err := ApplyMCP("old", MCP{Name: "old", URL: "https://example.com/sse", Transport: "sse"}, []model.Agent{model.Antigravity})
	if err == nil || !strings.Contains(err.Error(), "SSE") {
		t.Fatalf("got %v", err)
	}
	err = ApplyMCP("old", MCP{Name: "old", URL: "https://example.com/sse", Transport: "sse"}, []model.Agent{model.Codex})
	if err == nil || !strings.Contains(err.Error(), "SSE") {
		t.Fatalf("codex got %v", err)
	}
	if AcceptsSSE(model.Codex) || AcceptsSSE(model.Antigravity) || !AcceptsSSE(model.Claude) {
		t.Fatal("AcceptsSSE matrix")
	}
}

func TestOpenCodeJSONCRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := filepath.Join(home, ".config", "opencode")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	src := `{
  // plugin
  "plugin": ["x"],
  "mcp": {
  }
}
`
	if err := os.WriteFile(filepath.Join(cfg, "opencode.json"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMCP("exa", MCP{Name: "exa", URL: "https://mcp.exa.ai/mcp"}, []model.Agent{model.OpenCode}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(cfg, "opencode.json"))
	if !strings.Contains(string(got), "// plugin") || !strings.Contains(string(got), `"plugin": ["x"]`) {
		t.Fatalf("opencode rewritten:\n%s", got)
	}
	if !strings.Contains(string(got), `"exa"`) || !strings.Contains(string(got), "mcp.exa.ai") {
		t.Fatalf("missing server:\n%s", got)
	}
}

func TestAntigravityWritesURLToConfigDir(t *testing.T) {
	home := p0Home(t)
	if err := ApplyMCP("r", MCP{URL: "https://api.example.com/mcp/", Transport: "http"}, []model.Agent{model.Antigravity}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".gemini/config/mcp_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Antigravity's bundled mcp_servers.md: url, serverUrl is legacy, no type.
	if !strings.Contains(string(b), `"url":"https://api.example.com/mcp/"`) || strings.Contains(string(b), `"type"`) || strings.Contains(string(b), `"serverUrl"`) {
		t.Fatalf("antigravity entry: %s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini/antigravity/mcp_config.json")); !os.IsNotExist(err) {
		t.Fatal("wrote the legacy per-app path on a fresh install")
	}
}

func TestAntigravityConfigDirBeatsLeftoverLegacyFile(t *testing.T) {
	home := p0Home(t)
	legacy := filepath.Join(home, ".gemini/antigravity/mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"mcpServers":{"old":{"command":"x"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini/config"), 0700); err != nil {
		t.Fatal(err)
	}
	tg, _ := targetFor(model.Antigravity)
	if want := filepath.Join(home, ".gemini/config/mcp_config.json"); tg.mcpPath != want {
		t.Fatalf("wrote to %s, want %s", tg.mcpPath, want)
	}
	if want := filepath.Join(home, ".gemini/config/skills"); tg.skillDirs[0] != want {
		t.Fatalf("skills to %s, want %s", tg.skillDirs[0], want)
	}
	rows, err := InventoryMCP()
	if err != nil || len(rows) != 1 || rows[0].Name != "old" {
		t.Fatalf("legacy entry no longer read: %v %#v", err, rows)
	}
}

func TestAntigravityDanglingLegacyLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := p0Home(t)
	cfg := filepath.Join(home, ".gemini/config")
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	// ~/.gemini/antigravity -> ~/.gemini/config before mcp_config.json exists.
	if err := os.Symlink(cfg, filepath.Join(home, ".gemini/antigravity")); err != nil {
		t.Fatal(err)
	}
	tg, _ := targetFor(model.Antigravity)
	extras := extraMCPFiles(tg)
	if len(extras) != 1 || !strings.Contains(extras[0], "antigravity-cli") {
		t.Fatalf("dangling dir link not deduplicated: %v", extras)
	}
	// A file-level link whose target is not created yet must stay a link.
	link := filepath.Join(home, ".gemini/antigravity-cli/mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "elsewhere/mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if len(otherPaths(link, []string{target, link})) != 0 {
		t.Fatal("dangling file link and its target counted twice")
	}
	if err := atomicWrite(link, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dangling link replaced by a regular file")
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "{}" {
		t.Fatalf("target not written: %v %q", err, b)
	}
}

func TestUndoRemovesTargetCreatedThroughDanglingLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := p0Home(t)
	cfg := filepath.Join(home, ".gemini/config")
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg, "mcp_config.json")
	target := filepath.Join(home, "real/mcp_config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMCP("a", MCP{Command: "x"}, []model.Agent{model.Antigravity}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("write did not follow the link")
	}
	if err := UndoLast(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("undo left the created target")
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("undo replaced the link")
	}
}

func TestAntigravityLegacySymlinkNotDuplicated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := p0Home(t)
	cfg := filepath.Join(home, ".gemini/config/mcp_config.json")
	legacy := filepath.Join(home, ".gemini/antigravity/mcp_config.json")
	for _, d := range []string{filepath.Dir(cfg), filepath.Dir(legacy)} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"a":{"command":"x"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfg, legacy); err != nil {
		t.Fatal(err)
	}
	tg, _ := targetFor(model.Antigravity)
	if tg.mcpPath != cfg || len(extraMCPFiles(tg)) != 1 {
		t.Fatalf("path %s extras %v", tg.mcpPath, extraMCPFiles(tg))
	}
	if err := RemoveMCP("a", []model.Agent{model.Antigravity}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(legacy); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("legacy symlink replaced")
	}
}

func TestMCPDisabledPlacements(t *testing.T) {
	home := p0Home(t)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gemini/config/mcp_config.json", `{"mcpServers":{"a":{"command":"x","disabled":true}}}`)
	write(".config/opencode/opencode.json", `{"mcp":{"a":{"type":"local","command":["x"],"enabled":false}}}`)
	write(".codex/config.toml", "[mcp_servers.a]\ncommand = \"x\"\nenabled = false\n")
	write(".claude.json", `{"mcpServers":{"a":{"command":"x"}}}`)
	rows, err := InventoryMCP()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %#v", err, rows)
	}
	for _, agent := range []model.Agent{model.Antigravity, model.OpenCode, model.Codex} {
		if !rows[0].ByAgent[agent].Disabled {
			t.Fatalf("%s not disabled", agent)
		}
	}
	if rows[0].ByAgent[model.Claude].Disabled || rows[0].Drift {
		t.Fatalf("claude disabled or drift: %#v", rows[0])
	}
}

func TestMCPWritesSetSwitchAndKeepOtherKeys(t *testing.T) {
	home := p0Home(t)
	p := filepath.Join(home, ".gemini/config/mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		b, _ := os.ReadFile(p)
		return string(b)
	}
	old := `{"mcpServers":{"a":{"url":"https://old/mcp","type":"http","disabled":true,"timeoutSeconds":600,"disabledTools":["x"]}}}`

	// Re-enable in place: only the switch changes.
	write(old)
	if err := EnableMCP("a", model.Antigravity); err != nil {
		t.Fatal(err)
	}
	got := read()
	if strings.Contains(got, `"disabled"`) || !strings.Contains(got, `"timeoutSeconds":600`) || !strings.Contains(got, `"disabledTools":["x"]`) || !strings.Contains(got, "old/mcp") {
		t.Fatalf("enable: %s", got)
	}

	// A paste the user applied to this agent turns it on and keeps other keys.
	write(old)
	list, err := ParsePaste(`{"mcpServers":{"a":{"command":"npx"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyMCPs(list, []model.Agent{model.Antigravity}); err != nil {
		t.Fatal(err)
	}
	got = read()
	if strings.Contains(got, `"disabled"`) || !strings.Contains(got, `"timeoutSeconds":600`) || strings.Contains(got, "old/mcp") || strings.Contains(got, `"type"`) {
		t.Fatalf("paste: %s", got)
	}

	// Pasted switch and pasted keys win over the old entry.
	write(old)
	list, err = ParsePaste(`{"mcpServers":{"a":{"command":"npx","disabled":true,"timeoutSeconds":30}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyMCPs(list, []model.Agent{model.Antigravity}); err != nil {
		t.Fatal(err)
	}
	got = read()
	if !strings.Contains(got, `"disabled":true`) || !strings.Contains(got, `"timeoutSeconds":30`) || !strings.Contains(got, `"disabledTools":["x"]`) {
		t.Fatalf("pasted keys: %s", got)
	}

	// Copying from another agent's turned-off entry writes it turned on.
	if err := ApplyMCP("b", MCP{Command: "npx", Disabled: true}, []model.Agent{model.Antigravity}); err != nil {
		t.Fatal(err)
	}
	rows, _ := InventoryMCP()
	for _, r := range rows {
		if r.Name == "b" && r.ByAgent[model.Antigravity].Disabled {
			t.Fatal("copy kept the source's off switch")
		}
	}
}

func TestTOMLSwitchFollowsWrite(t *testing.T) {
	src := "[mcp_servers.a]\ncommand = \"x\"\nenabled = false\nstartup_timeout_sec = 30\n"
	got, err := upsertTOMLServer(src, MCP{Name: "a", Command: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "enabled") || !strings.Contains(got, "startup_timeout_sec = 30") {
		t.Fatalf("enable: %s", got)
	}
	got, err = upsertTOMLServer(got, MCP{Name: "a", Command: "x", Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "enabled = false") {
		t.Fatalf("disable: %s", got)
	}
	m, _, err := parseTOMLServer(got, "a")
	if err != nil || !m.Disabled {
		t.Fatalf("round trip: %v %#v", err, m)
	}
}

func TestRowSourcePrefersEnabledEntry(t *testing.T) {
	rows := buildRows([]Placement{
		{Agent: model.Claude, MCP: MCP{Name: "a", Command: "old-stdio"}, Disabled: true},
		{Agent: model.Codex, MCP: MCP{Name: "a", URL: "https://new.example.com/mcp"}},
	})
	src, ok := rows[0].Source()
	if !ok || src.URL == "" {
		t.Fatalf("source %#v", src)
	}
	if rows[0].Summary != src.Summary() || strings.Contains(rows[0].Summary, "old-stdio") {
		t.Fatalf("summary %q disagrees with source %q", rows[0].Summary, src.Summary())
	}
}

func TestSupportsMCPDisable(t *testing.T) {
	if !SupportsMCPDisable(model.Codex) || !SupportsMCPDisable(model.Antigravity) ||
		!SupportsMCPDisable(model.OpenCode) || !SupportsMCPDisable(model.WorkBuddy) {
		t.Fatal("expected Codex, Antigravity, OpenCode, WorkBuddy to support disable")
	}
	if SupportsMCPDisable(model.Claude) || SupportsMCPDisable(model.Devin) {
		t.Fatal("expected Claude and Devin to not support disable")
	}
}

func TestDisableMCPPreservesExtraAndUndo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))

	// Claude does not support DisableMCP.
	if err := DisableMCP("foo", model.Claude); err == nil {
		t.Fatal("expected error when disabling MCP for Claude")
	}

	// Setup Codex TOML with an extra custom field.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(codexDir, "config.toml")
	initialTOML := "[mcp_servers.my_server]\ncommand = \"npx\"\nstartup_timeout_sec = 45\n"
	if err := os.WriteFile(cfgPath, []byte(initialTOML), 0600); err != nil {
		t.Fatal(err)
	}

	// Disable MCP
	if err := DisableMCP("my_server", model.Codex); err != nil {
		t.Fatalf("DisableMCP failed: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "enabled = false") {
		t.Fatalf("expected enabled = false in %s", content)
	}
	if !strings.Contains(content, "startup_timeout_sec = 45") {
		t.Fatalf("expected startup_timeout_sec = 45 preserved in %s", content)
	}

	// Inventory should show Disabled
	rows, err := InventoryMCP()
	if err != nil || len(rows) != 1 {
		t.Fatalf("InventoryMCP: %v, rows: %v", err, rows)
	}
	if !rows[0].ByAgent[model.Codex].Disabled {
		t.Fatal("expected Placement.Disabled to be true")
	}

	// Disable again when already disabled should return nil
	if err := DisableMCP("my_server", model.Codex); err != nil {
		t.Fatalf("DisableMCP when already disabled returned err: %v", err)
	}

	// Undo should revert back to enabled
	if err := UndoLast(); err != nil {
		t.Fatalf("UndoLast failed: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	content = string(data)
	if strings.Contains(content, "enabled = false") {
		t.Fatalf("expected enabled = false to be removed on undo: %s", content)
	}
	if !strings.Contains(content, "startup_timeout_sec = 45") {
		t.Fatalf("expected startup_timeout_sec = 45 preserved after undo: %s", content)
	}
}

func TestWorkBuddyMultiRootDisableAndEnable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))

	wbDir := filepath.Join(home, ".workbuddy")
	cbDir := filepath.Join(home, ".codebuddy")
	if err := os.MkdirAll(wbDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cbDir, 0700); err != nil {
		t.Fatal(err)
	}

	wbFile := filepath.Join(wbDir, "mcp.json")
	cbFile := filepath.Join(cbDir, "mcp.json")
	if err := os.WriteFile(wbFile, []byte(`{"mcpServers":{"srv":{"command":"npx","extra_1":"val1"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cbFile, []byte(`{"mcpServers":{"srv":{"command":"npx","extra_2":"val2"}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	// DisableMCP should update both files
	if err := DisableMCP("srv", model.WorkBuddy); err != nil {
		t.Fatalf("DisableMCP failed: %v", err)
	}

	wbData, _ := os.ReadFile(wbFile)
	cbData, _ := os.ReadFile(cbFile)
	if !strings.Contains(string(wbData), `"disabled":true`) || !strings.Contains(string(wbData), "extra_1") {
		t.Fatalf(".workbuddy not disabled or extra field lost: %s", wbData)
	}
	if !strings.Contains(string(cbData), `"disabled":true`) || !strings.Contains(string(cbData), "extra_2") {
		t.Fatalf(".codebuddy not disabled or extra field lost: %s", cbData)
	}

	// EnableMCP should update both files
	if err := EnableMCP("srv", model.WorkBuddy); err != nil {
		t.Fatalf("EnableMCP failed: %v", err)
	}

	wbData, _ = os.ReadFile(wbFile)
	cbData, _ = os.ReadFile(cbFile)
	if strings.Contains(string(wbData), `"disabled":true`) || !strings.Contains(string(wbData), "extra_1") {
		t.Fatalf(".workbuddy still disabled: %s", wbData)
	}
	if strings.Contains(string(cbData), `"disabled":true`) || !strings.Contains(string(cbData), "extra_2") {
		t.Fatalf(".codebuddy still disabled: %s", cbData)
	}

	// Undo should revert back to disabled
	if err := UndoLast(); err != nil {
		t.Fatalf("UndoLast failed: %v", err)
	}
	wbData, _ = os.ReadFile(wbFile)
	cbData, _ = os.ReadFile(cbFile)
	if !strings.Contains(string(wbData), `"disabled":true`) {
		t.Fatalf(".workbuddy not reverted on undo: %s", wbData)
	}
	if !strings.Contains(string(cbData), `"disabled":true`) {
		t.Fatalf(".codebuddy not reverted on undo: %s", cbData)
	}
}

func TestOpenCodeExtraMCPFileDisableAndEnable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))

	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	mcpFile := filepath.Join(cfgDir, "mcp.json")
	if err := os.WriteFile(mcpFile, []byte(`{"mcpServers":{"demo":{"command":"npx","args":["foo"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	// DisableMCP should update mcp.json in OpenCode's extra directory
	if err := DisableMCP("demo", model.OpenCode); err != nil {
		t.Fatalf("DisableMCP failed: %v", err)
	}

	data, err := os.ReadFile(mcpFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"disabled":true`) {
		t.Fatalf("expected disabled:true in %s", data)
	}

	// EnableMCP should toggle it back
	if err := EnableMCP("demo", model.OpenCode); err != nil {
		t.Fatalf("EnableMCP failed: %v", err)
	}
	data, _ = os.ReadFile(mcpFile)
	if strings.Contains(string(data), `"disabled":true`) {
		t.Fatalf("expected disabled:true removed in %s", data)
	}
}
