package data

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

// Part is one piece of a conversation as the agent recorded it.
type Part struct {
	Kind PartKind
	// Name is a tool call's tool.
	Name string
	Text string
	// Args are a tool call's named arguments, each the bytes of Text that
	// hold its value, so the panel lays them out and search still marks
	// its hits in them.
	Args []Arg
}

// Arg is a tool call's argument: its value is Text[At:End].
type Arg struct {
	Name    string
	At, End int
}

type PartKind int

const (
	PartUser PartKind = iota
	PartAssistant
	PartThinking
	PartToolCall
	PartToolResult
)

// Transcript limits: a conversation keeps its last maxParts parts, each cut
// to maxPartRunes, so a long session cannot stall the native UI.
const (
	maxParts     = 400
	maxPartRunes = 4000
)

// ErrNoTranscript: the session's conversation cannot be read on this device.
var ErrNoTranscript = errors.New("no readable conversation for this session")

// TranscriptSupported reports whether Transcript can read agent's sessions.
func TranscriptSupported(a model.Agent) bool {
	return a == model.Claude || a == model.Codex || a == model.Devin
}

// Transcript reads a local session's conversation from its source, read-only
// and on demand: nothing is cached or synchronized. files is the loaded
// data's FileSessions (path → session key). It returns the last parts and
// whether earlier ones were left out.
func Transcript(ctx context.Context, s model.Session, files map[string]string) ([]Part, bool, error) {
	var parts []Part
	var err error
	switch s.Agent {
	case model.Claude, model.Codex:
		p := transcriptFile(s, files)
		if p == "" {
			return nil, false, ErrNoTranscript
		}
		parts, err = jsonlTranscript(s.Agent, p)
	case model.Devin:
		parts, err = devinTranscript(ctx, s)
	default:
		return nil, false, ErrNoTranscript
	}
	if err != nil {
		return nil, false, err
	}
	cut := len(parts) > maxParts
	if cut {
		parts = parts[len(parts)-maxParts:]
	}
	for i := range parts {
		if parts[i].Kind == PartToolCall {
			parts[i].Text, parts[i].Args = toolArgs(parts[i].Text)
		}
		if r := []rune(parts[i].Text); len(r) > maxPartRunes {
			kept := string(r[:maxPartRunes])
			parts[i].Text = kept + "…"
			args := parts[i].Args[:0]
			for _, a := range parts[i].Args {
				if a.At < len(kept) {
					if a.End > len(kept) {
						a.End = len(parts[i].Text) // the cut value ends with the ellipsis
					}
					args = append(args, a)
				}
			}
			parts[i].Args = args
		}
	}
	return parts, cut, nil
}

// transcriptFile is the session's main transcript: of the files mapped to
// its key, the one named after its ID (Claude's subagent files map to their
// parent's key too), else the first.
func transcriptFile(s model.Session, files map[string]string) string {
	var found []string
	for p, key := range files {
		if key == s.Key {
			found = append(found, p)
		}
	}
	sort.Strings(found)
	for _, p := range found {
		if fileID(p) == s.ID || strings.HasSuffix(fileID(p), s.ID) {
			return p
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	return ""
}

func jsonlTranscript(a model.Agent, p string) ([]Part, error) {
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var parts []Part
	for sc.Scan() {
		v, ok := transcriptObject(sc.Bytes())
		if !ok {
			continue
		}
		if a == model.Claude {
			parts = append(parts, claudeParts(v)...)
		} else {
			parts = append(parts, codexParts(v)...)
		}
	}
	return parts, sc.Err()
}

func transcriptObject(raw []byte) (obj, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // Tool arguments may contain integers too large for float64.
	var v obj
	err := dec.Decode(&v)
	return v, err == nil && len(bytes.TrimSpace(raw[dec.InputOffset():])) == 0
}

func claudeParts(v obj) []Part {
	kind := str(v["type"])
	if (kind != "user" && kind != "assistant") || v["isMeta"] == true {
		return nil
	}
	content := get(v, "message", "content")
	if s, ok := content.(string); ok {
		if kind == "user" && strings.HasPrefix(strings.TrimSpace(s), "<") {
			return nil // command wrappers and injected context
		}
		return textPart(kind == "user", s)
	}
	var out []Part
	for _, b := range asList(content) {
		switch str(get(b, "type")) {
		case "text":
			out = append(out, textPart(kind == "user", str(get(b, "text")))...)
		case "thinking":
			if t := strings.TrimSpace(str(get(b, "thinking"))); t != "" {
				out = append(out, Part{Kind: PartThinking, Text: t})
			}
		case "tool_use":
			out = append(out, Part{Kind: PartToolCall, Name: str(get(b, "name")), Text: jsonText(get(b, "input"))})
		case "tool_result":
			out = append(out, Part{Kind: PartToolResult, Text: blockText(get(b, "content"))})
		}
	}
	return out
}

func codexParts(v obj) []Part {
	if str(v["type"]) != "response_item" {
		return nil
	}
	p := v["payload"]
	switch str(get(p, "type")) {
	case "message":
		role := str(get(p, "role"))
		if role != "user" && role != "assistant" {
			return nil
		}
		t := blockText(get(p, "content"))
		if role == "user" && strings.HasPrefix(strings.TrimSpace(t), "<") {
			return nil // environment context and instructions
		}
		return textPart(role == "user", t)
	case "reasoning":
		if t := blockText(get(p, "summary")); t != "" {
			return []Part{{Kind: PartThinking, Text: t}}
		}
	case "function_call":
		return []Part{{Kind: PartToolCall, Name: str(get(p, "name")), Text: str(get(p, "arguments"))}}
	case "custom_tool_call":
		return []Part{{Kind: PartToolCall, Name: str(get(p, "name")), Text: str(get(p, "input"))}}
	case "function_call_output", "custom_tool_call_output":
		out := get(p, "output")
		if s, ok := out.(string); ok {
			return []Part{{Kind: PartToolResult, Text: s}}
		}
		return []Part{{Kind: PartToolResult, Text: blockText(out)}}
	}
	return nil
}

// devinTranscript walks the session's latest branch of message_nodes from
// its newest node to the root. Devin stores a forest: each reply twice as
// sibling nodes and the context prefix again for each request, so a message
// is shown once, at its first place on the branch.
func devinTranscript(ctx context.Context, s model.Session) ([]Part, error) {
	source, _, _ := strings.Cut(s.Key, "/")
	if source != "cli" && source != "cli-next" {
		return nil, ErrNoTranscript
	}
	base := home(".local/share/devin")
	if runtime.GOOS == "windows" {
		base = filepath.Join(os.Getenv("APPDATA"), "devin")
	}
	p := filepath.Join(base, source, "sessions.db")
	if _, e := os.Stat(p); e != nil {
		return nil, ErrNoTranscript
	}
	db, e := sqlite.Open(ctx, p, sqlite.OpenOptions{ReadOnly: true, BusyTimeout: time.Second})
	if e != nil {
		return nil, e
	}
	defer db.Close()
	rows, e := db.Query(ctx, "SELECT node_id, parent_node_id, chat_message FROM message_nodes WHERE session_id = ? ORDER BY row_id", s.ID)
	if e != nil {
		return nil, e
	}
	type node struct {
		parent    int64
		hasParent bool
		msg       []byte
	}
	nodes := map[int64]node{}
	var leaf int64
	for _, r := range rows.Values {
		id, _ := r[0].(int64)
		n := node{msg: []byte(str(r[2]))}
		if b, ok := r[2].([]byte); ok {
			n.msg = b
		}
		n.parent, n.hasParent = r[1].(int64)
		nodes[id], leaf = n, id
	}
	if len(nodes) == 0 {
		return nil, ErrNoTranscript
	}
	var chain [][]byte
	seen := map[int64]bool{}
	for id, ok := leaf, true; ok && !seen[id]; {
		seen[id] = true
		n, found := nodes[id]
		if !found {
			break
		}
		chain = append(chain, n.msg)
		id, ok = n.parent, n.hasParent
	}
	var parts []Part
	shown := map[string]bool{}
	for i := len(chain) - 1; i >= 0; i-- {
		m, ok := transcriptObject(chain[i])
		if !ok {
			continue
		}
		if id := str(m["message_id"]); id != "" {
			if shown[id] {
				continue
			}
			shown[id] = true
		}
		switch str(m["role"]) {
		case "user":
			if get(m, "metadata", "is_user_input") == true {
				parts = append(parts, textPart(true, blockText(m["content"]))...)
			}
		case "assistant":
			if t := strings.TrimSpace(str(get(m, "thinking", "thinking"))); t != "" {
				parts = append(parts, Part{Kind: PartThinking, Text: t})
			}
			parts = append(parts, textPart(false, blockText(m["content"]))...)
			for _, c := range asList(m["tool_calls"]) {
				parts = append(parts, Part{Kind: PartToolCall, Name: str(get(c, "name")), Text: jsonText(get(c, "arguments"))})
			}
		case "tool":
			parts = append(parts, Part{Kind: PartToolResult, Text: blockText(m["content"])})
		}
	}
	return parts, nil
}

func textPart(user bool, s string) []Part {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if user {
		return []Part{{Kind: PartUser, Text: s}}
	}
	return []Part{{Kind: PartAssistant, Text: s}}
}

func asList(v any) []any { l, _ := v.([]any); return l }

// blockText joins the text of a string or a list of content blocks, each
// a paragraph of its own, as the Markdown the panel shows them as needs.
func blockText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b []string
	for _, x := range asList(v) {
		if s, ok := x.(string); ok {
			b = append(b, s)
		} else if s := str(get(x, "text")); s != "" {
			b = append(b, s)
		}
	}
	return strings.Join(b, "\n\n")
}

// toolArgs lays out named arguments in their recorded order, a line of
// "name: value" each, or the name on a line of its own above a value of
// several lines, and returns where each value is. String values show
// their text, nested data indented JSON. Non-object arguments (such as raw
// patches) stay as recorded, without Args.
func toolArgs(s string) (string, []Arg) {
	dec := json.NewDecoder(strings.NewReader(s))
	if t, e := dec.Token(); e != nil || t != json.Delim('{') {
		return s, nil
	}
	var b strings.Builder
	var args []Arg
	for dec.More() {
		k, e := dec.Token()
		var raw json.RawMessage
		if e != nil || dec.Decode(&raw) != nil {
			return s, nil
		}
		key, _ := k.(string)
		v := string(raw)
		if raw[0] == '"' {
			var text string
			if json.Unmarshal(raw, &text) == nil && text != "" {
				v = strings.TrimRight(text, "\n")
			}
		} else if raw[0] == '[' || raw[0] == '{' {
			var formatted bytes.Buffer
			if json.Indent(&formatted, raw, "", "  ") == nil {
				v = formatted.String()
			}
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(key)
		if strings.Contains(v, "\n") {
			b.WriteString(":\n")
		} else {
			b.WriteString(": ")
		}
		args = append(args, Arg{Name: key, At: b.Len(), End: b.Len() + len(v)})
		b.WriteString(v)
	}
	if _, e := dec.Token(); e != nil || strings.TrimSpace(s[dec.InputOffset():]) != "" || len(args) == 0 {
		return s, nil
	}
	return b.String(), args
}

func jsonText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, e := json.Marshal(v)
	if e != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
