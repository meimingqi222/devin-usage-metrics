package ext

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestTOMLInventoryIgnoresOtherMultiline(t *testing.T) {
	src := `
developer_instructions = """
keep me
"""

[mcp_servers.gh]
command = "npx"
args = [
  "-y",
  "pkg",
]
env = { TOKEN = "abc" }
`
	names, err := listTOMLServers(src)
	if err != nil || len(names) != 1 || names[0] != "gh" {
		t.Fatalf("list %v %#v", err, names)
	}
	m, ok, err := parseTOMLServer(src, "gh")
	if err != nil || !ok {
		t.Fatalf("parse %v %v", ok, err)
	}
	if m.Command != "npx" || len(m.Args) != 2 || m.Args[1] != "pkg" {
		t.Fatalf("args %#v", m)
	}
	if m.Env["TOKEN"] != "abc" {
		t.Fatalf("env %#v", m.Env)
	}
}

func TestTOMLUpsertKeepsExtraKeysAndPosition(t *testing.T) {
	src := `[mcp_servers.a]
command = "old"
startup_timeout_sec = 30
cwd = "/tmp"

[other]
x = 1
`
	got, err := upsertTOMLServer(src, MCP{Name: "a", Command: "new", Args: []string{"-y"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "startup_timeout_sec = 30") || !strings.Contains(got, `cwd = "/tmp"`) {
		t.Fatalf("lost extra keys:\n%s", got)
	}
	if strings.Contains(got, "transport") {
		t.Fatalf("wrote transport:\n%s", got)
	}
	idxA := strings.Index(got, "[mcp_servers.a]")
	idxO := strings.Index(got, "[other]")
	if idxA < 0 || idxO < 0 || idxA > idxO {
		t.Fatalf("block moved:\n%s", got)
	}
	if !strings.Contains(got, `command = "new"`) || !strings.Contains(got, `args = ["-y"]`) {
		t.Fatalf("missing patch:\n%s", got)
	}
}

func TestTOMLQuotedURLHashAndUnicodeEscape(t *testing.T) {
	src := `[mcp_servers.a]
url = "https://x/#a b"
command = "echo"
`
	m, ok, err := parseTOMLServer(src, "a")
	if err != nil || !ok || m.URL != "https://x/#a b" {
		t.Fatalf("url cut: %v %v %#v", ok, err, m)
	}
	got := tomlQuote("ok\x7f")
	if !strings.Contains(got, `\u007F`) && !strings.Contains(got, `\u007f`) {
		t.Fatalf("DEL not escaped: %s", got)
	}
	v, _, err := unquoteTOML(`"line\nnext"`)
	if err != nil || v != "line\nnext" {
		t.Fatalf("unquote \\n: %v %q", err, v)
	}
}

func TestTOMLRefusesQuotedRootMCP(t *testing.T) {
	quoted := `"mcp_servers".gh.command = "npx"` + "\n"
	if _, err := upsertTOMLServer(quoted, MCP{Name: "gh", Command: "uvx"}); err == nil {
		t.Fatal("quoted root accepted")
	}
	spaced := "mcp_servers . gh.command = \"npx\"\n"
	if _, err := upsertTOMLServer(spaced, MCP{Name: "gh", Command: "uvx"}); err == nil {
		t.Fatal("spaced root accepted")
	}
	literal := `'mcp_servers'.gh.command = "npx"` + "\n"
	if _, err := upsertTOMLServer(literal, MCP{Name: "gh", Command: "uvx"}); err == nil {
		t.Fatal("literal-quoted root accepted")
	}
}

func TestJSONCSlashEscapeAndCommaComment(t *testing.T) {
	src := `{
  "mcpServers": {
    "a": { "url": "https:\/\/x.example/mcp" } // x, y
  }
}
`
	members, _, _, err := jsoncPathObject(src, []string{"mcpServers"})
	if err != nil || len(members) != 1 || members[0].key != "a" {
		t.Fatalf("scan %v %#v", err, members)
	}
	obj, err := decodeObject(src[members[0].valueStart:members[0].valueEnd])
	if err != nil {
		t.Fatal(err)
	}
	if obj["url"] != "https://x.example/mcp" {
		t.Fatalf("url %v", obj["url"])
	}
	got, err := upsertJSONC(src, "b", []string{"mcpServers"}, `{"command":"npx"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"b": {"command":"npx"}`) {
		t.Fatalf("missing b:\n%s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stripJSONC(got)), &parsed); err != nil {
		t.Fatalf("invalid json after insert: %v\n%s", err, got)
	}
}

func TestJSONCRemoveKeepsNextComment(t *testing.T) {
	src := `{
  "mcpServers": {
    "a": { "command": "echo" },
    // keep: b is prod
    "b": { "command": "npx" }
  }
}
`
	got, err := deleteJSONC(src, "a", []string{"mcpServers"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `"a"`) {
		t.Fatalf("a remains:\n%s", got)
	}
	if !strings.Contains(got, "keep: b is prod") || !strings.Contains(got, `"b"`) {
		t.Fatalf("lost next comment:\n%s", got)
	}
}

func TestInventoryMCPPartialAgentError(t *testing.T) {
	home := p0Home(t)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"ok":{"command":"npx"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ag := filepath.Join(home, ".gemini", "config")
	if err := os.MkdirAll(ag, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ag, "mcp_config.json"), []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryMCP()
	if err == nil {
		t.Fatal("expected per-agent error")
	}
	if len(rows) != 1 || rows[0].Name != "ok" {
		t.Fatalf("lost good agent: %v %#v", err, rows)
	}
}

func TestApplyMCPsOneSnapshotAndRollback(t *testing.T) {
	home := p0Home(t)
	claude := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claude, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("model = \"x\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	skipped, err := ApplyMCPs([]MCP{
		{Name: "a", Command: "npx"},
		{Name: "b", Command: "uvx"},
	}, []model.Agent{model.Claude, model.Codex})
	if err != nil || skipped {
		t.Fatalf("apply %v skipped=%v", err, skipped)
	}
	if err := UndoLast(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(claude)
	if strings.Contains(string(got), `"a"`) || strings.Contains(string(got), `"b"`) {
		t.Fatalf("undo missed batch:\n%s", got)
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if strings.Contains(string(toml), "mcp_servers") {
		t.Fatalf("codex undo missed:\n%s", toml)
	}
}

func TestApplyMCPsSkipsSSEWithoutFailing(t *testing.T) {
	home := p0Home(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("model = \"x\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	skipped, err := ApplyMCPs([]MCP{
		{Name: "sse", URL: "https://example.com/sse"},
		{Name: "ok", Command: "npx"},
	}, []model.Agent{model.Claude, model.Codex})
	if err != nil {
		t.Fatal(err)
	}
	if !skipped {
		t.Fatal("expected SSE skip")
	}
	claude, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if !strings.Contains(string(claude), `"sse"`) || !strings.Contains(string(claude), `"type":"sse"`) {
		t.Fatalf("claude sse: %s", claude)
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if strings.Contains(string(toml), "mcp_servers.sse") {
		t.Fatalf("codex wrote sse:\n%s", toml)
	}
	if !strings.Contains(string(toml), "[mcp_servers.ok]") {
		t.Fatalf("codex missed stdio:\n%s", toml)
	}
}

func TestSSELastPathSegment(t *testing.T) {
	if !(MCP{URL: "https://x.example/v1/sse"}).SSE() {
		t.Fatal("/sse last segment")
	}
	if (MCP{URL: "https://x.example/sse-proxy/mcp"}).SSE() {
		t.Fatal("sse-proxy false positive")
	}
	if (MCP{URL: "https://x.example/sseverything"}).SSE() {
		t.Fatal("sseverything false positive")
	}
}

func TestExtractSkillRequiresMatchingDir(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	w, err := zw.Create("repo-main/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("# other\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extractSkill(buf.Bytes(), "pdf"); err == nil {
		t.Fatal("fallback to first SKILL.md")
	}
}

func TestZipPathAllowsDotDotInName(t *testing.T) {
	if zipPathEscapes("repo/a..b.md") {
		t.Fatal("rejected a..b.md")
	}
	if !zipPathEscapes("repo/../etc/passwd") {
		t.Fatal("allowed parent segment")
	}
}

func TestUrlQueryEscapesPlus(t *testing.T) {
	if got := urlQuery("c++"); !strings.Contains(got, "%2B") {
		t.Fatalf("got %s", got)
	}
}

func TestPreviewAndInstallShareZip(t *testing.T) {
	home := p0Home(t)
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	w, err := zw.Create("repo-main/skills/pdf/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("---\nname: pdf\ndescription: PDF files\n---\n# PDF\n")); err != nil {
		t.Fatal(err)
	}
	fh := &zip.FileHeader{Name: "repo-main/skills/pdf/scripts/run.sh", Method: zip.Deflate}
	fh.SetMode(0755)
	hw, err := zw.CreateHeader(fh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hw.Write([]byte("#!/bin/sh\necho ok\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipBytes := buf.Bytes()
	ar := SkillArchive{Hit: Hit{ID: "anthropics/skills/pdf", Source: "anthropics/skills", SkillID: "pdf"}, Zip: zipBytes}
	rel, md, files, err := inspectSkillZip(zipBytes, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	ar.Rel, ar.Markdown, ar.Files = rel, md, files
	if !strings.Contains(md, "PDF files") {
		t.Fatalf("md %s", md)
	}
	if err := InstallSkillArchive(ar, []model.Agent{model.Claude}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".claude/skills/pdf/SKILL.md"))
	if err != nil || !strings.Contains(string(got), "PDF files") {
		t.Fatalf("install %v %s", err, got)
	}
	st, err := os.Stat(filepath.Join(home, ".claude/skills/pdf/scripts/run.sh"))
	if err != nil || st.Mode()&0111 == 0 {
		t.Fatalf("exec bit lost: %v %v", err, st)
	}
	if err := os.MkdirAll(filepath.Join(home, ".agents/skills/pdf"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agents/skills/pdf/SKILL.md"), []byte("# shared\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnableSkill("pdf", model.Antigravity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini/config/skills/pdf/SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeSSEType(t *testing.T) {
	raw, err := encodeServersValue(MCP{Name: "s", URL: "https://example.com/v1/sse"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"type":"sse"`) {
		t.Fatalf("%s", raw)
	}
	raw, err = encodeServersValue(MCP{Name: "s", URL: "https://example.com/mcp/sse", Transport: "http"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"type":"sse"`) || !strings.Contains(raw, `"type":"http"`) {
		t.Fatalf("explicit http lost: %s", raw)
	}
	if (MCP{URL: "https://example.com/mcp/sse", Transport: "http"}).SSE() {
		t.Fatal("explicit http treated as SSE")
	}
}

func zipFiles(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFindSkillRelZipRoot(t *testing.T) {
	z := zipFiles(t, [2]string{"my-skill-main/SKILL.md", "---\nname: my-skill\ndescription: d\n---\n# Root\n"})
	rel, md, _, err := inspectSkillZip(z, "my-skill")
	if err != nil || rel != "my-skill-main" || !strings.Contains(md, "# Root") {
		t.Fatalf("root skill %v %q %q", err, rel, md)
	}
	if _, _, _, err := inspectSkillZip(z, "pdf"); err == nil {
		t.Fatal("zip-root SKILL.md installed under a different id")
	}
	nested := zipFiles(t,
		[2]string{"s/sub/SKILL.md", "WRONG"},
		[2]string{"s/SKILL.md", "---\nname: s\ndescription: ok\n---\n# OK\n"},
	)
	rel, md, _, err = inspectSkillZip(nested, "s")
	if err != nil || rel != "s" || !strings.Contains(md, "# OK") || strings.Contains(md, "WRONG") {
		t.Fatalf("nested md %v %q %q", err, rel, md)
	}
	two := zipFiles(t,
		[2]string{"repo-main/SKILL.md", "---\nname: other\n---\n"},
		[2]string{"repo-main/docs/SKILL.md", "---\nname: docs\n---\n"},
	)
	if _, _, _, err := inspectSkillZip(two, "my-skill"); err == nil {
		t.Fatal("ambiguous root accepted")
	}
	named := zipFiles(t,
		[2]string{"repo-main/SKILL.md", "---\nname: my-skill\ndescription: d\n---\n# Named\n"},
		[2]string{"repo-main/docs/SKILL.md", "---\nname: docs\n---\n"},
	)
	rel, md, _, err = inspectSkillZip(named, "my-skill")
	if err != nil || rel != "repo-main" || !strings.Contains(md, "# Named") {
		t.Fatalf("frontmatter root %v %q %q", err, rel, md)
	}
}

func TestJSONCRemoveLastMemberCommaInComment(t *testing.T) {
	src := `{
  "mcpServers": {
    "a": { "command": "echo" }, // note, with comma
    "b": { "command": "npx" }
  }
}
`
	got, err := deleteJSONC(src, "b", []string{"mcpServers"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `"b"`) {
		t.Fatalf("b remains:\n%s", got)
	}
	stripped := stripJSONC(got)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stripped), &parsed); err != nil {
		t.Fatalf("invalid json after delete: %v\n%s", err, got)
	}
}

func TestParseMCPFileReportsTOMLMultiline(t *testing.T) {
	home := p0Home(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	src := `[mcp_servers.a]
cmd = """
x
"""

[mcp_servers.b]
command = "echo"
`
	if err := os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryMCP()
	if err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("expected multiline error for a: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "b" {
		t.Fatalf("lost b: %v %#v", err, rows)
	}
}

func TestPatchTOMLHeadersWhenEnvAbutsMain(t *testing.T) {
	src := `[mcp_servers.a]
command = "old"
[mcp_servers.a.env]
TOKEN = "x"
[profile]
k = 1
`
	got, err := upsertTOMLServer(src, MCP{Name: "a", Command: "new", Headers: map[string]string{"H": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "[mcp_servers.a.env]") {
		t.Fatalf("env remains:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.a.http_headers]") || !strings.Contains(got, "[profile]") {
		t.Fatalf("headers/profile:\n%s", got)
	}
	if !strings.Contains(got, `command = "new"`) {
		t.Fatalf("command:\n%s", got)
	}
}

func TestSkillAtBareZipRoot(t *testing.T) {
	z := zipFiles(t,
		[2]string{"SKILL.md", "---\nname: pdf\n---\n# Bare\n"},
		[2]string{"ref/a.md", "a"},
	)
	rel, md, files, err := inspectSkillZip(z, "pdf")
	if err != nil || rel != "." || !strings.Contains(md, "# Bare") || len(files) != 2 {
		t.Fatalf("bare root %v %q %q %v", err, rel, md, files)
	}
	dir, _, err := extractSkill(z, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(dir))
	if _, err := os.Stat(filepath.Join(dir, "ref/a.md")); err != nil {
		t.Fatal(err)
	}
}
