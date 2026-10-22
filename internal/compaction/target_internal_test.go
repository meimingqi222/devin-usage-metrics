package compaction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// Rust keys undo records by root.join(file) and canonicalizes only when the
// file itself is a symlink; a symlinked parent directory must not change keys.
func TestTargetPathMatchesRustCanonicalization(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.WriteFile(filepath.Join(real, "config.toml"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", link)
	got, err := local(model.Codex, "")
	if err != nil || got.path != filepath.Join(link, "config.toml") {
		t.Fatalf("symlinked directory was resolved: %q %v", got.path, err)
	}
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Symlink(filepath.Join(real, "config.toml"), file); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Dir(file))
	got, err = local(model.Codex, "")
	want, _ := filepath.EvalSymlinks(file)
	if err != nil || got.path != want {
		t.Fatalf("symlinked file was not resolved: %q want %q (%v)", got.path, want, err)
	}
}

func TestProcessOverrideLocksWithoutSettingsFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "")
	if Locked(model.Claude) {
		t.Fatal("locked without any override")
	}
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "300000")
	if !Locked(model.Claude) {
		t.Fatal("process override ignored when settings.json is missing")
	}
}
