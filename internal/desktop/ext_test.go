package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/ext"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestExtMatrixListsLocalMCP(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"github":{"command":"npx","args":["-y","server"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("[mcp_servers.github]\ncommand = \"npx\"\nargs = [\"-y\", \"server\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Claude: true, model.Codex: true, model.Antigravity: true, model.OpenCode: true, model.WorkBuddy: true, model.Pi: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	if !tt.HasText("MCP 与 Skills") {
		t.Fatalf("missing sidebar entry: %q", tt.Texts())
	}
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	if !tt.HasText("MCP Servers") || !tt.HasText("github") {
		t.Fatalf("matrix missing github: %q", tt.Texts())
	}
	if !tt.HasText("命令 / 服务源") || !tt.HasText("生效 Agent") || !tt.HasText("Codex") {
		t.Fatalf("card layout missing: %q", tt.Texts())
	}
	if !tt.HasText("批量添加 MCP") || !tt.HasText("撤销上次变更") {
		t.Fatal("toolbar actions missing")
	}
	capture(t, tt, "ext-mcp-matrix")
	click(t, tt, "已安装 Skills")
	if !tt.HasText("已安装 Skills") {
		t.Fatal("skills tab missing")
	}
}

func TestExtSkillsInheritedFromShared(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".agents/skills/create-skill")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: create-skill\ndescription: Create skills\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Claude: true, model.Codex: true, model.Antigravity: true, model.OpenCode: true, model.WorkBuddy: true, model.Pi: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	click(t, tt, "已安装 Skills")
	if !tt.HasText("create-skill") || !tt.HasText("共享") {
		t.Fatalf("shared skill missing: %q", tt.Texts())
	}
	if !tt.HasText("已启用 3 个") {
		t.Fatalf("inherited count: %q", tt.Texts())
	}
	if !tt.HasText("✓ Codex") || !tt.HasText("✓ OpenCode") || !tt.HasText("✓ pi-agent") {
		t.Fatalf("inheriting agents off: %q", tt.Texts())
	}
	if tt.HasText("✓ Claude Code") || tt.HasText("✓ Antigravity") || tt.HasText("✓ WorkBuddy") {
		t.Fatalf("dedicated-only agents marked on: %q", tt.Texts())
	}
	if !tt.HasText("取消共享") {
		t.Fatalf("unshare action missing: %q", tt.Texts())
	}
	click(t, tt, "✓ Codex")
	if !strings.Contains(a.message, "取消共享") {
		t.Fatalf("inherited click deleted or silent: %s", a.message)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal("clicking an inherited pill deleted ~/.agents/skills")
	}
	capture(t, tt, "ext-skills-inherited")
}

func TestExtWriteKeepsRowsWhenAgentFileBad(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("[mcp_servers.github]\ncommand = \"npx\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Claude: true, model.Codex: true, model.Antigravity: true, model.OpenCode: true, model.WorkBuddy: true, model.Pi: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	if !tt.HasText("github") {
		t.Fatalf("initial rows missing: %q", tt.Texts())
	}
	a.runExt(func() error {
		return ext.ApplyMCP("new", ext.MCP{Name: "new", Command: "uvx"}, []model.Agent{model.Codex})
	})
	waitExt(t, a, tt)
	if !tt.HasText("new") {
		t.Fatalf("stale after write: %q msg=%s", tt.Texts(), a.message)
	}
	if !strings.Contains(a.message, "Claude") {
		t.Fatalf("inventory error dropped: %s", a.message)
	}
}

func waitExt(t *testing.T, a *App, tt *ui.Tester) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tt.Frame()
		if a.extUI.loaded && !a.busy["ext"] {
			return
		}
		// Rendering alone can starve the inventory goroutine on Windows.
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("ext inventory did not load: busy=%v msg=%q", a.busy["ext"], a.message)
}

func TestExtColumnsIncludeConfiguredAgentWithoutSessions(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("[mcp_servers.github]\ncommand = \"npx\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Claude: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	if !tt.HasText("✓ Codex") {
		t.Fatalf("Codex has config but no pill: %q", tt.Texts())
	}
}

func TestExtPausedPillTurnsBackOn(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".codex/config.toml")
	if err := os.WriteFile(cfg, []byte("[mcp_servers.github]\ncommand = \"npx\"\nenabled = false\nstartup_timeout_sec = 30\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Codex: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	click(t, tt, "– Codex")
	waitExt(t, a, tt)
	if a.extUI.confirmOpen {
		t.Fatal("paused pill asked to remove")
	}
	b, _ := os.ReadFile(cfg)
	if strings.Contains(string(b), "enabled") || !strings.Contains(string(b), "startup_timeout_sec = 30") {
		t.Fatalf("not re-enabled in place: %s", b)
	}
	if !tt.HasText("✓ Codex") {
		t.Fatalf("pill still paused: %q", tt.Texts())
	}
}

func TestExtConfirmedWriteWaitsForBusyJob(t *testing.T) {
	a := fixture(t)
	a.installed = map[model.Agent]bool{model.Claude: true}
	tt := ui.NewTester(a.View, 1240, 780)
	ran := false
	a.busy["ext"] = true
	a.extUI.queued = func() error { ran = true; return nil }
	tt.Frame()
	if ran || a.extUI.queued == nil {
		t.Fatal("queued write ran or was dropped while ext was busy")
	}
	delete(a.busy, "ext")
	waitExt(t, a, tt)
	if !ran {
		t.Fatal("confirmed write was dropped")
	}
}

func TestExtMCPShowsDisabledPlacement(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"github":{"command":"npx"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("[mcp_servers.github]\ncommand = \"npx\"\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Claude: true, model.Codex: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	if !tt.HasText("✓ Claude Code") || !tt.HasText("– Codex") || tt.HasText("✓ Codex") {
		t.Fatalf("disabled Codex shown as on: %q", tt.Texts())
	}
	if tt.HasText("已停用") {
		t.Fatalf("row tagged disabled while Claude is on: %q", tt.Texts())
	}
	capture(t, tt, "ext-mcp-disabled")
}

func TestExtMCPDriftExplainsAndSyncs(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"github":{"command":"npx","args":["-y","server"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".codex/config.toml")
	if err := os.WriteFile(cfg, []byte("[mcp_servers.github]\ncommand = \"uvx\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.installed = map[model.Agent]bool{model.Claude: true, model.Codex: true}
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "MCP 与 Skills")
	waitExt(t, a, tt)
	if !tt.HasText("配置不一致：1 处") {
		t.Fatalf("drift tag missing: %q", tt.Texts())
	}
	click(t, tt, "配置不一致：1 处")
	tt.Frame()
	if !a.extUI.driftOpen || !tt.HasText("MCP 配置不一致") || !tt.HasText("以 Claude Code 为基准统一") || !tt.HasText("uvx") {
		t.Fatalf("drift dialog missing: open=%v %q", a.extUI.driftOpen, tt.Texts())
	}
	capture(t, tt, "ext-mcp-drift")
	click(t, tt, "以 Claude Code 为基准统一")
	waitExt(t, a, tt)
	b, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(b), `command = "npx"`) || !strings.Contains(string(b), "server") {
		t.Fatalf("sync did not copy the source config: %v %s", err, b)
	}
	if tt.HasText("配置不一致：") {
		t.Fatalf("drift tag survived sync: %q", tt.Texts())
	}
}
