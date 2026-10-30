package ext

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestSearchSkills(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" || r.URL.Query().Get("q") != "pdf" {
			t.Errorf("query %s %s", r.URL.Path, r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(searchResponse{Skills: []Hit{{ID: "anthropics/skills/pdf", Source: "anthropics/skills", SkillID: "pdf", Name: "pdf", Installs: 10}}})
	}))
	t.Cleanup(srv.Close)
	searchEndpoint = srv.URL + "/api/search"
	hits, err := SearchSkills(context.Background(), "pdf")
	if err != nil || len(hits) != 1 || hits[0].SkillID != "pdf" {
		t.Fatalf("%v %#v", err, hits)
	}
}

func TestInstallSkillCopiesDedicatedDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("repo-main/skills/pdf/SKILL.md", "---\nname: pdf\ndescription: PDF files\n---\n# PDF\n")
	add("repo-main/README.md", "no")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/zip/") {
			http.NotFound(w, r)
			return
		}
		w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	githubZip = srv.URL
	hit := Hit{ID: "anthropics/skills/pdf", Source: "anthropics/skills", SkillID: "pdf", Name: "pdf"}
	if err := InstallSkill(context.Background(), hit, []model.Agent{model.Claude, model.Pi}); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(home, ".claude/skills/pdf/SKILL.md")
	pi := filepath.Join(home, ".pi/agent/skills/pdf/SKILL.md")
	for _, p := range []string{claude, pi} {
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), "PDF files") {
			t.Fatalf("%s: %v %s", p, err, b)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".agents/skills/pdf")); err == nil {
		t.Fatal("partial install wrote the shared agents directory")
	}
	rows, err := InventorySkills()
	if err != nil || len(rows) != 1 {
		t.Fatalf("inventory %v %#v", err, rows)
	}
	if _, ok := rows[0].ByAgent[model.Claude]; !ok {
		t.Fatal("claude missing")
	}
	if err := RemoveSkill("pdf", []model.Agent{model.Claude}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claude); !os.IsNotExist(err) {
		t.Fatal("claude skill remains")
	}
	if _, err := os.Stat(pi); err != nil {
		t.Fatal("pi skill removed with claude")
	}
}

func writeSkill(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: pdf\ndescription: PDF files\n---\n# PDF\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func skillRow(t *testing.T, name string) SkillRow {
	t.Helper()
	rows, err := InventorySkills()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("skill %s missing: %#v", name, rows)
	return SkillRow{}
}

func TestInventorySkillsMarksInheritedShared(t *testing.T) {
	home := p0Home(t)
	writeSkill(t, filepath.Join(home, ".agents/skills/pdf"))
	row := skillRow(t, "pdf")
	if len(row.Places) != 1 || !row.Places[0].Shared || row.Places[0].Inherited {
		t.Fatalf("places %#v", row.Places)
	}
	for _, agent := range []model.Agent{model.Codex, model.OpenCode, model.Pi} {
		p, ok := row.ByAgent[agent]
		if !ok || !p.Inherited || !p.Shared {
			t.Fatalf("%s: ok=%v %#v", agent, ok, p)
		}
	}
	for _, agent := range []model.Agent{model.Claude, model.Antigravity, model.WorkBuddy} {
		if _, ok := row.ByAgent[agent]; ok {
			t.Fatalf("%s should not inherit ~/.agents/skills", agent)
		}
	}

	writeSkill(t, filepath.Join(home, ".claude/skills/pdf"))
	row = skillRow(t, "pdf")
	if p, ok := row.ByAgent[model.Claude]; !ok || p.Inherited {
		t.Fatalf("claude dedicated %#v %v", p, ok)
	}
	if p := row.ByAgent[model.OpenCode]; !p.Inherited || !p.Shared {
		t.Fatalf("opencode should still inherit shared first %#v", p)
	}

	writeSkill(t, filepath.Join(home, ".codex/skills/pdf"))
	row = skillRow(t, "pdf")
	if p := row.ByAgent[model.Codex]; p.Inherited || p.Shared {
		t.Fatalf("codex dedicated should win %#v", p)
	}

	if err := RemoveSkill("pdf", []model.Agent{model.Codex}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents/skills/pdf/SKILL.md")); err != nil {
		t.Fatal("per-agent remove deleted shared")
	}
	row = skillRow(t, "pdf")
	if p := row.ByAgent[model.Codex]; !p.Inherited {
		t.Fatalf("codex should fall back to inherited %#v", p)
	}

	if err := UnshareSkill("pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents/skills/pdf")); !os.IsNotExist(err) {
		t.Fatal("shared remains")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/pdf/SKILL.md")); err != nil {
		t.Fatal("unshare deleted claude copy")
	}
	row = skillRow(t, "pdf")
	if _, ok := row.ByAgent[model.Codex]; ok {
		t.Fatal("codex still on after unshare")
	}
	if _, ok := row.ByAgent[model.Pi]; ok {
		t.Fatal("pi still on after unshare")
	}
	if p := row.ByAgent[model.OpenCode]; !p.Inherited || p.Shared {
		t.Fatalf("opencode should inherit claude copy %#v", p)
	}
	if err := EnableSkill("pdf", model.Pi); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".pi/agent/skills/pdf/SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestInventorySkillsOpenCodeInheritsClaude(t *testing.T) {
	home := p0Home(t)
	writeSkill(t, filepath.Join(home, ".claude/skills/pdf"))
	row := skillRow(t, "pdf")
	if p, ok := row.ByAgent[model.Claude]; !ok || p.Inherited {
		t.Fatalf("claude %#v %v", p, ok)
	}
	if p, ok := row.ByAgent[model.OpenCode]; !ok || !p.Inherited || p.Shared {
		t.Fatalf("opencode %#v %v", p, ok)
	}
	for _, agent := range []model.Agent{model.Codex, model.Pi, model.Antigravity, model.WorkBuddy} {
		if _, ok := row.ByAgent[agent]; ok {
			t.Fatalf("%s on without shared or dedicated", agent)
		}
	}
	if err := UnshareSkill("pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/pdf/SKILL.md")); err != nil {
		t.Fatal("unshare deleted claude-only skill")
	}
	if err := RemoveSkill("pdf", []model.Agent{model.Claude}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := InventorySkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("skill remains %#v", rows)
	}
}

func TestEnableSkillRefusesInheritedShared(t *testing.T) {
	home := p0Home(t)
	writeSkill(t, filepath.Join(home, ".agents/skills/pdf"))
	if err := EnableSkill("pdf", model.Pi); err == nil || !strings.Contains(err.Error(), "already enabled") {
		t.Fatalf("enable inherited: %v", err)
	}
	if err := EnableSkill("pdf", model.Antigravity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini/config/skills/pdf/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	row := skillRow(t, "pdf")
	if p := row.ByAgent[model.Antigravity]; p.Inherited {
		t.Fatalf("antigravity should be dedicated %#v", p)
	}
}

func TestRemoveSkillSkipsSharedForInheritedAgent(t *testing.T) {
	home := p0Home(t)
	writeSkill(t, filepath.Join(home, ".agents/skills/pdf"))
	if err := RemoveSkill("pdf", []model.Agent{model.Codex}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents/skills/pdf/SKILL.md")); err != nil {
		t.Fatal("inherited remove deleted shared")
	}
}

func TestRemoveSkillWhenOtherAgentDirUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions")
	}
	home := p0Home(t)
	writeSkill(t, filepath.Join(home, ".claude/skills/pdf"))
	pi := filepath.Join(home, ".pi/agent/skills")
	if err := os.MkdirAll(pi, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pi, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(pi, 0700) })
	if err := RemoveSkill("pdf", []model.Agent{model.Claude}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/pdf")); !os.IsNotExist(err) {
		t.Fatal("Claude copy kept because pi-agent's directory is unreadable")
	}
}

func TestListSkillDirsSkipsBrokenLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := p0Home(t)
	root := filepath.Join(home, ".claude/skills")
	writeSkill(t, filepath.Join(home, "real/pdf"))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "file.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"pdf": "real/pdf", "gone": "missing", "file": "file.txt", "empty": "empty"} {
		if err := os.Symlink(filepath.Join(home, target), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listSkillDirs(root)
	if err != nil || len(got) != 1 || got[0].name != "pdf" || !got[0].link {
		t.Fatalf("%v %#v", err, got)
	}
}
