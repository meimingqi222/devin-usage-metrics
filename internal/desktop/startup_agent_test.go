package desktop

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestStartupSelectsInstalledAgent(t *testing.T) {
	for _, installed := range []model.Agent{model.Codex, model.Claude, model.Devin} {
		t.Run(string(installed), func(t *testing.T) {
			fixture(t) // Isolate config, data directories and device identity.
			home, _ := os.UserHomeDir()
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			paths := map[model.Agent]string{
				model.Codex:  filepath.Join(home, ".codex/sessions"),
				model.Claude: filepath.Join(home, ".claude/projects"),
				model.Devin:  filepath.Join(home, ".local/share/devin/cli/sessions.db"),
			}
			if runtime.GOOS == "windows" {
				paths[model.Devin] = filepath.Join(os.Getenv("APPDATA"), "devin/cli/sessions.db")
			}
			if installed != "" {
				p := paths[installed]
				if installed == model.Devin {
					if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, nil, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			a := New(context.Background())
			if a.agent != installed {
				t.Fatalf("startup selected %s, want %s", a.agent, installed)
			}
			if installed == model.Devin {
				return // The fixture only marks the path; it is not a SQLite DB.
			}
			a.load(a.agent, false)
			deadline := time.Now().Add(10 * time.Second)
			for a.busy[string(a.agent)] {
				a.drain()
				if time.Now().After(deadline) {
					t.Fatal("startup load timed out")
				}
				time.Sleep(time.Millisecond)
			}
			if len(a.loaded.Errors) != 0 {
				t.Fatal("startup produced an error for missing agent history")
			}
		})
	}
}

func TestSidebarGroupingDoesNotDependOnSelection(t *testing.T) {
	a := fixture(t)
	a.installed = map[model.Agent]bool{model.Codex: true}
	a.counts = map[model.Agent]int{model.Claude: 1}
	a.agent = model.Devin
	tt := ui.NewTester(func(c *ui.Context) { a.theme(c); a.sidebar(c) }, 240, 780)
	if tt.HasText("Devin") || !tt.HasText("Codex") || !tt.HasText("Claude Code") {
		t.Fatal("selected uninstalled agent changed the primary agent list")
	}
	a.agent = model.Codex
	tt.Frame()
	if tt.HasText("Devin") || !tt.HasText("Codex") || !tt.HasText("Claude Code") {
		t.Fatal("switching agents changed the primary agent list")
	}
	a.otherAgents = true
	tt.Frame()
	if !tt.HasText("Devin") {
		t.Fatal("uninstalled agent is missing from More agents")
	}
}
