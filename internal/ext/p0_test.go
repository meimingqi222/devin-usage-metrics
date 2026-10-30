package ext

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func p0Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	return home
}

func TestSummaryRedactsArgs(t *testing.T) {
	m := MCP{
		Name:    "StitchMCP",
		Command: "npx",
		Args: []string{
			"-y", "mcp-remote", "https://stitch.googleapis.com/mcp",
			"--header", "X-Goog-Api-Key: AQ.FakeTestKey000000000000000000000000000000",
		},
	}
	s := m.Summary()
	if strings.Contains(s, "FakeTestKey") {
		t.Fatalf("key leaked: %s", s)
	}
	if !strings.Contains(s, "--header ***") {
		t.Fatalf("header not redacted: %s", s)
	}
	if strings.Contains(s, "stitch.googleapis.com/mcp") {
		t.Fatalf("url path not redacted: %s", s)
	}
	if !strings.Contains(s, "https://stitch.googleapis.com") {
		t.Fatalf("host dropped: %s", s)
	}
	cases := []struct {
		cmd  string
		args []string
		bad  []string
	}{
		{"docker", []string{"run", "-e", "GITHUB_TOKEN=ghp_secret", "img", "--api-key", "sk-abc"}, []string{"ghp_secret", "sk-abc"}},
		{"npx", []string{"-y", "server-postgres", "postgresql://user:hunter2@localhost/db"}, []string{"hunter2"}},
		{"x", []string{"--token=abc123plain"}, []string{"abc123plain"}},
		{"x", []string{"--api-key", "abc123plain"}, []string{"abc123plain"}},
		{"x", []string{"--access-token", "abc"}, []string{"abc"}},
		{"docker", []string{"run", "-e", "DB_PASS=hunter2", "img"}, []string{"hunter2"}},
		{"x", []string{"mongodb+srv://u:pw@c.example.net/db"}, []string{"pw@"}},
		{"x", []string{"redis://:pw@h:6379"}, []string{":pw@"}},
		{"x", []string{"--key", "abc123plain"}, []string{"abc123plain"}},
	}
	for _, tc := range cases {
		got := (MCP{Command: tc.cmd, Args: tc.args}).Summary()
		for _, bad := range tc.bad {
			if strings.Contains(got, bad) {
				t.Fatalf("%q leaked in %s", bad, got)
			}
		}
	}
}

func TestEncodeServersFromIR(t *testing.T) {
	open := mcpFromMap("gh", map[string]any{
		"type":    "local",
		"command": []any{"npx", "-y", "gh"},
		"environment": map[string]any{
			"TOKEN": "abc",
		},
	})
	raw, err := encodeServersValue(open)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"type":"local"`) || strings.Contains(raw, `"environment"`) {
		t.Fatalf("opencode shape leaked: %s", raw)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["command"] != "npx" {
		t.Fatalf("command %v", obj["command"])
	}
	commented := `{"command":"npx" // tool}`
	m := MCP{Name: "gh", Command: "npx"}
	out, err := encodeServersValue(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "//") || strings.Contains(out, commented) {
		t.Fatalf("raw jsonc passed through: %s", out)
	}
	remote, err := encodeServersValue(MCP{Name: "r", URL: "https://mcp.exa.ai/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote, `"type":"http"`) {
		t.Fatalf("missing http type: %s", remote)
	}
}

func TestApplyMCPDoesNotCopyJSONC(t *testing.T) {
	home := p0Home(t)
	wb := filepath.Join(home, ".workbuddy")
	if err := os.MkdirAll(wb, 0700); err != nil {
		t.Fatal(err)
	}
	src := "{\n  \"mcpServers\": {\n    \"gh\": {\n      \"command\": \"npx\", // tool\n      \"args\": [\"-y\", \"gh\"]\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(wb, "mcp.json"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryMCP()
	if err != nil || len(rows) == 0 {
		t.Fatalf("inventory %v %#v", err, rows)
	}
	m := rows[0].ByAgent[model.WorkBuddy].MCP
	if err := ApplyMCP("gh", m, []model.Agent{model.Claude}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "//") {
		t.Fatalf("jsonc copied into claude.json:\n%s", text)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("claude.json invalid: %v\n%s", err, text)
	}
}

func TestUndoRefusesStaleFile(t *testing.T) {
	home := p0Home(t)
	claude := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claude, []byte(`{"mcpServers":{"github":{"command":"npx"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMCP("github", MCP{Name: "github", Command: "npx", Args: []string{"-y", "gh"}}, []model.Agent{model.Claude}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, []byte(`{"mcpServers":{"github":{"command":"npx","args":["-y","gh"]}},"projects":{"x":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UndoLast(); !errors.Is(err, errStaleUndo) {
		t.Fatalf("got %v", err)
	}
	got, _ := os.ReadFile(claude)
	if !strings.Contains(string(got), `"projects"`) {
		t.Fatalf("stale undo overwrote later writes:\n%s", got)
	}
}

func TestAtomicWriteFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(real, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(link, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	got, err := os.ReadFile(real)
	if err != nil || string(got) != `{"a":2}` {
		t.Fatalf("target %v %s", err, got)
	}
}

func TestInstallSkillRejectsBadID(t *testing.T) {
	p0Home(t)
	ctx := context.Background()
	for _, id := range []string{"", ".", "..", "../.ssh", "foo/bar", `foo\bar`} {
		err := InstallSkill(ctx, Hit{ID: "x/y/" + id, Source: "x/y", SkillID: id}, []model.Agent{model.Claude})
		if err == nil || !strings.Contains(err.Error(), "invalid skill id") {
			t.Fatalf("id %q: %v", id, err)
		}
	}
}

func TestRemoveSkillUndoRestoresDir(t *testing.T) {
	home := p0Home(t)
	dir := filepath.Join(home, ".claude", "skills", "foo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: foo\ndescription: d\n---\n# Foo\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSkill("foo", []model.Agent{model.Claude}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("skill still present")
	}
	if err := UndoLast(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || string(got) != body {
		t.Fatalf("undo missed skill dir: %v %s", err, got)
	}
}

func TestRemoveSkillUndoRestoresSymlink(t *testing.T) {
	home := p0Home(t)
	shared := filepath.Join(home, ".agents", "skills", "foo")
	if err := os.MkdirAll(shared, 0700); err != nil {
		t.Fatal(err)
	}
	body := "# Foo\n"
	if err := os.WriteFile(filepath.Join(shared, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", "skills", "foo")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSkill("foo", []model.Agent{model.Claude}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("link still present")
	}
	if _, err := os.Stat(filepath.Join(shared, "SKILL.md")); err != nil {
		t.Fatal("shared skill was deleted")
	}
	if err := UndoLast(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(link)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link not restored: %v %#v", err, st)
	}
	got, err := os.Readlink(link)
	if err != nil || got != shared {
		t.Fatalf("link target %v %s", err, got)
	}
}

func TestTOMLDeleteKeepsArrayAndCommentedHeaders(t *testing.T) {
	src := `[mcp_servers.a]
command = "x"

[[profiles]]
name = "keep"

[mcp_servers.a.env]
K = "v"

[profiles.fast] # tuned
model = "y"
`
	got, err := deleteTOMLServer(src, "a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "mcp_servers.a") {
		t.Fatalf("server remains:\n%s", got)
	}
	if !strings.Contains(got, "[[profiles]]") || !strings.Contains(got, `name = "keep"`) {
		t.Fatalf("array table lost:\n%s", got)
	}
	if !strings.Contains(got, "[profiles.fast]") || !strings.Contains(got, `model = "y"`) {
		t.Fatalf("commented header lost:\n%s", got)
	}
}

func TestTOMLRefusesRootMCPAssignments(t *testing.T) {
	dotted := "model = \"o3\"\nmcp_servers.gh.command = \"npx\"\n"
	if _, err := upsertTOMLServer(dotted, MCP{Name: "gh", Command: "uvx"}); err == nil {
		t.Fatal("dotted root accepted")
	}
	inline := "mcp_servers = { gh = { command = \"npx\" } }\n"
	if _, err := upsertTOMLServer(inline, MCP{Name: "gh", Command: "uvx"}); err == nil {
		t.Fatal("root inline accepted")
	}
	commented := "[mcp_servers] # servers\ngh = { command = \"npx\" }\n"
	if _, err := upsertTOMLServer(commented, MCP{Name: "gh", Command: "uvx"}); err == nil {
		t.Fatal("commented [mcp_servers] accepted")
	}
}

func TestTOMLQuotesNonASCIIAndOmitsTransport(t *testing.T) {
	got := encodeTOMLServer(MCP{Name: "数据库", Command: "npx", Transport: "http", URL: "https://x.example", Env: map[string]string{"é": "v"}})
	if strings.Contains(got, "[mcp_servers.数据库]") {
		t.Fatalf("unquoted non-ascii header:\n%s", got)
	}
	if !strings.Contains(got, `[mcp_servers."数据库"]`) {
		t.Fatalf("missing quoted header:\n%s", got)
	}
	if strings.Contains(got, "transport") {
		t.Fatalf("wrote transport:\n%s", got)
	}
	if strings.Contains(got, `\x`) {
		t.Fatalf("go \\x escape:\n%s", got)
	}
}

func TestSummaryRedactsShortHeaderAndFlagURL(t *testing.T) {
	m := MCP{Name: "x", Command: "npx", Args: []string{
		"mcp-remote", "https://mcp.example.com/sse", "-H", "Cookie: session=abc123secret",
		"--db=postgresql://admin:hunter2@db.local/app", "Bearer tok_abcdef123456",
	}}
	got := m.Summary()
	for _, leak := range []string{"abc123secret", "hunter2", "admin", "tok_abcdef123456"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked: %s", leak, got)
		}
	}
	if !strings.Contains(got, "--db=postgresql://db.local") {
		t.Fatalf("flag url lost: %s", got)
	}
	stitch := MCP{Name: "s", Command: "npx", Args: []string{"-y", "mcp-remote", "https://stitch.googleapis.com/mcp", "--header", "X-Goog-Api-Key: AQ.FakeTestKey000000000000000000000000000000"}}
	if strings.Contains(stitch.Summary(), "FakeTestKey") {
		t.Fatalf("API key in summary: %s", stitch.Summary())
	}
}
