package ext

import (
	"strings"
	"testing"
)

func TestTOMLUpsertPreservesComments(t *testing.T) {
	src := `# keep
model = "x"

[mcp_servers.a]
command = "echo"

[other]
x = 1
`
	got, err := upsertTOMLServer(src, MCP{Name: "b", Command: "npx", Args: []string{"-y", "pkg"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# keep") || !strings.Contains(got, `model = "x"`) || !strings.Contains(got, "[other]") {
		t.Fatalf("lost surrounding bytes:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.a]") || !strings.Contains(got, "[mcp_servers.b]") {
		t.Fatalf("missing tables:\n%s", got)
	}
	del, err := deleteTOMLServer(got, "a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(del, "[mcp_servers.a]") {
		t.Fatalf("table a remains:\n%s", del)
	}
	if !strings.Contains(del, "# keep") || !strings.Contains(del, "[mcp_servers.b]") {
		t.Fatalf("delete touched the rest:\n%s", del)
	}
}

func TestTOMLQuotedNameAndEnv(t *testing.T) {
	src := `
[mcp_servers."database-manager"]
command = "npx"

[mcp_servers."database-manager".env]
TOKEN = "secret"
`
	m, ok, err := parseTOMLServer(src, "database-manager")
	if err != nil || !ok {
		t.Fatalf("parse: %v %v", ok, err)
	}
	if m.Command != "npx" || m.Env["TOKEN"] != "secret" {
		t.Fatalf("%#v", m)
	}
}

func TestTOMLDeleteKeepsUnrelatedTables(t *testing.T) {
	src := `[mcp_servers.a]
command = "x"

[profile]
model = "keep"

[mcp_servers.a.env]
K = "v"
`
	got, err := deleteTOMLServer(src, "a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "mcp_servers.a") {
		t.Fatalf("server remains:\n%s", got)
	}
	if !strings.Contains(got, "[profile]") || !strings.Contains(got, `model = "keep"`) {
		t.Fatalf("unrelated table lost:\n%s", got)
	}
}

func TestTOMLRefusesInlineDuplicate(t *testing.T) {
	src := `[mcp_servers]
gh = { command = "npx" }
`
	_, err := upsertTOMLServer(src, MCP{Name: "gh", Command: "uvx"})
	if err == nil || !strings.Contains(err.Error(), "inline") {
		t.Fatalf("got %v", err)
	}
	if _, err := deleteTOMLServer(src, "gh"); err == nil || !strings.Contains(err.Error(), "inline") {
		t.Fatalf("delete got %v", err)
	}
}

func TestDeleteTOMLKeepsProfileWhenTripleQuoteInString(t *testing.T) {
	src := `[mcp_servers.a]
command = "x"
note = "use ''' here"

[profile]
model = "keep"
`
	got, err := deleteTOMLServer(src, "a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "mcp_servers.a") {
		t.Fatalf("server remains:\n%s", got)
	}
	if !strings.Contains(got, "[profile]") || !strings.Contains(got, `model = "keep"`) {
		t.Fatalf("profile lost:\n%s", got)
	}
	src = `[mcp_servers.a]
command = "x"
note = 'use """ here'

[profile]
model = "keep"
`
	got, err = deleteTOMLServer(src, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[profile]") {
		t.Fatalf("literal-quoted triple-quote wiped file:\n%s", got)
	}
}

func TestDeleteTOMLRefusesUnclosedMultiline(t *testing.T) {
	src := `[mcp_servers.a]
cmd = """
x

[profile]
model = "keep"
`
	if _, err := deleteTOMLServer(src, "a"); err != errTOMLMultiline {
		t.Fatalf("got %v", err)
	}
}

func TestTOMLRefusesMultiline(t *testing.T) {
	src := `
[mcp_servers.a]
cmd = """
x
"""

[mcp_servers.b]
command = "echo"
`
	got, err := upsertTOMLServer(src, MCP{Name: "c", Command: "npx"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[mcp_servers.c]") || !strings.Contains(got, "[mcp_servers.b]") {
		t.Fatalf("other servers lost:\n%s", got)
	}
	if _, err := upsertTOMLServer(src, MCP{Name: "a", Command: "npx"}); err != errTOMLMultiline {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteTOMLNestedArrayLineIsNotHeader(t *testing.T) {
	src := "[mcp_servers.a]\ncommand = \"x\"\nargs = [\n  [\"n\"],\n]\nenabled = true\n\n[profile]\nx = 1\n"
	got, err := deleteTOMLServer(src, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "\n[profile]\nx = 1\n" && got != "[profile]\nx = 1\n" {
		t.Fatalf("left part of the server behind:\n%s", got)
	}
}

func TestDeleteTOMLRefusesUnclosedArray(t *testing.T) {
	src := "[mcp_servers.a]\nargs = [\n  \"x\",\n\n[profile]\nx = 1\n"
	if _, err := deleteTOMLServer(src, "a"); err == nil {
		t.Fatal("unclosed array swallowed [profile]")
	}
}
