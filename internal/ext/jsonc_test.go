package ext

import (
	"strings"
	"testing"
)

func TestJSONCUpsertPreservesComments(t *testing.T) {
	src := `{
  // keep me
  "other": 1,
  "mcpServers": {
    "a": { "command": "echo" },
  }
}
`
	got, err := upsertJSONC(src, "b", []string{"mcpServers"}, `{"command":"npx"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "// keep me") || !strings.Contains(got, `"other": 1`) {
		t.Fatalf("lost surrounding bytes:\n%s", got)
	}
	if !strings.Contains(got, `"a": { "command": "echo" }`) {
		t.Fatalf("rewrote sibling:\n%s", got)
	}
	if !strings.Contains(got, `"b": {"command":"npx"}`) {
		t.Fatalf("missing insert:\n%s", got)
	}
	del, err := deleteJSONC(got, "a", []string{"mcpServers"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(del, `"a"`) {
		t.Fatalf("member a remains:\n%s", del)
	}
	if !strings.Contains(del, "// keep me") || !strings.Contains(del, `"b":`) {
		t.Fatalf("delete touched the rest:\n%s", del)
	}
}

func TestJSONCUpsertBOM(t *testing.T) {
	src := "\ufeff{\n  \"a\": 1\n}\n"
	got, err := upsertJSONC(src, "x", []string{"mcpServers"}, `{"command":"npx"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "\ufeff") {
		t.Fatalf("lost BOM:\n%s", got)
	}
	stripped := strings.TrimPrefix(got, "\ufeff")
	if !strings.Contains(stripped, `"mcpServers"`) || !strings.Contains(stripped, `"x"`) || !strings.Contains(stripped, `"a": 1`) {
		t.Fatalf("bad insert:\n%s", got)
	}
}

func TestJSONCEmptyFile(t *testing.T) {
	got, err := upsertJSONC("", "github", []string{"mcpServers"}, `{"command":"npx"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"mcpServers"`) || !strings.Contains(got, `"github"`) {
		t.Fatalf("empty upsert: %s", got)
	}
}

func TestParsePasteMCPServers(t *testing.T) {
	list, err := ParsePaste(`{"mcpServers":{"github":{"command":"npx","args":["-y","server"]}}}`)
	if err != nil || len(list) != 1 || list[0].Name != "github" || list[0].Command != "npx" {
		t.Fatalf("%v %#v", err, list)
	}
}

func TestJSONCEditKeepsIndent(t *testing.T) {
	src := "{\n  \"mcpServers\": {\n    \"a\": 1\n  }\n}\n"
	got, err := upsertJSONC(src, "b", []string{"mcpServers"}, `2`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"mcpServers\": {\n    \"a\": 1,\n    \"b\": 2\n  }\n}\n"; got != want {
		t.Fatalf("insert indent:\n%s", got)
	}
	got, err = deleteJSONC(got, "a", []string{"mcpServers"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"mcpServers\": {\n    \"b\": 2\n  }\n}\n"; got != want {
		t.Fatalf("delete indent:\n%s", got)
	}
}
