package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

type dialect string

const (
	dialectServers  dialect = "mcpServers"
	dialectOpenCode dialect = "opencode"
	dialectTOML     dialect = "toml"
)

// MCP is the portable server description used to compare and translate
// across agent config files. Secret values stay in Env/Headers for writes
// and never appear in Summary.
type MCP struct {
	Name      string
	Command   string
	Args      []string
	URL       string
	Env       map[string]string
	Headers   map[string]string
	Transport string
	// Disabled is the agent's own off switch: "disabled": true, OpenCode
	// "enabled": false, or Codex enabled = false. It is not part of Fingerprint.
	// Writes always set the target's switch from it.
	Disabled bool
	// Extra holds keys of a pasted snippet this tool does not translate
	// (timeoutSeconds, disabledTools…). JSON targets get them as given.
	Extra map[string]any
}

func (m MCP) SSE() bool {
	if t := strings.TrimSpace(m.Transport); t != "" {
		return strings.EqualFold(t, "sse")
	}
	u, err := url.Parse(m.URL)
	if err != nil || u.Path == "" {
		return false
	}
	seg := path.Base(strings.TrimRight(u.Path, "/"))
	return strings.EqualFold(seg, "sse")
}

func (m MCP) Fingerprint() string {
	type wire struct {
		Command   string            `json:"command,omitempty"`
		Args      []string          `json:"args,omitempty"`
		URL       string            `json:"url,omitempty"`
		Env       map[string]string `json:"env,omitempty"`
		Headers   map[string]string `json:"headers,omitempty"`
		Transport string            `json:"transport,omitempty"`
	}
	b, _ := json.Marshal(wire{Command: m.Command, Args: m.Args, URL: m.URL, Env: m.Env, Headers: m.Headers, Transport: canonicalTransport(m.Transport, m.URL)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func canonicalTransport(t, rawURL string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "", "stdio", "local":
		if rawURL != "" {
			return "http"
		}
		return "stdio"
	case "http", "streamable-http", "streamablehttp", "remote":
		return "http"
	case "sse":
		return "sse"
	default:
		return t
	}
}

func (m MCP) Summary() string {
	var parts []string
	if m.Command != "" {
		parts = append(parts, summarizeCommand(m.Command, m.Args))
	}
	if m.URL != "" {
		if u, err := url.Parse(m.URL); err == nil && u.Host != "" {
			parts = append(parts, u.Scheme+"://"+u.Host)
		} else {
			parts = append(parts, "remote")
		}
	}
	if n := len(m.Env); n > 0 {
		parts = append(parts, "env "+strings.Join(sortedKeys(m.Env), ","))
	}
	if n := len(m.Headers); n > 0 {
		parts = append(parts, "headers "+strings.Join(sortedKeys(m.Headers), ","))
	}
	if len(parts) == 0 {
		return m.Name
	}
	return strings.Join(parts, " · ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func summarizeCommand(command string, args []string) string {
	parts := []string{command}
	hideNext := false
	for _, a := range args {
		if hideNext {
			parts = append(parts, "***")
			hideNext = false
			continue
		}
		flag, val, hasVal := splitFlag(a)
		if secretFlag(flag) {
			if hasVal {
				parts = append(parts, flag+"=***")
				continue
			}
			parts = append(parts, a)
			hideNext = true
			continue
		}
		if hasVal {
			// --db=postgresql://user:pass@host/db: redact the value on its own.
			parts = append(parts, flag+"="+redactArgValue(val))
			continue
		}
		parts = append(parts, redactArgValue(a))
	}
	return strings.Join(parts, " ")
}

func splitFlag(a string) (flag, val string, hasVal bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	if i := strings.IndexByte(a, '='); i >= 0 {
		return a[:i], a[i+1:], true
	}
	return a, "", false
}

func secretFlag(flag string) bool {
	if flag == "" {
		return false
	}
	f := strings.ToLower(strings.TrimLeft(flag, "-"))
	if f == "header" || f == "h" || f == "cookie" || f == "token" || f == "api-key" || f == "apikey" || f == "api_key" ||
		f == "secret" || f == "password" || f == "authorization" || f == "auth" ||
		f == "e" || f == "env" || f == "key" || f == "pass" || f == "pwd" || f == "credential" {
		return true
	}
	return strings.Contains(f, "token") || strings.Contains(f, "secret") || strings.Contains(f, "password") ||
		strings.Contains(f, "api-key") || strings.Contains(f, "credential") || strings.Contains(f, "passwd")
}

func redactArgValue(a string) string {
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return a[:7] + "***"
	}
	if u, err := url.Parse(a); err == nil && u.Scheme != "" && u.Scheme != "file" && (u.Host != "" || u.User != nil) {
		host := u.Host
		if host == "" {
			host = u.Opaque
		}
		return u.Scheme + "://" + host
	}
	if key, _, ok := strings.Cut(a, "="); ok && looksSecretName(key) {
		return key + "=***"
	}
	if name, val, ok := strings.Cut(a, ":"); ok && looksSecretName(strings.TrimSpace(name)) && strings.TrimSpace(val) != "" {
		return strings.TrimSpace(name) + ": ***"
	}
	if looksSecretValue(a) {
		return "***"
	}
	return a
}

func looksSecretName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimLeft(n, "-")
	return strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "password") ||
		strings.Contains(n, "passwd") || strings.Contains(n, "api-key") || strings.Contains(n, "apikey") ||
		strings.Contains(n, "api_key") || strings.Contains(n, "authorization") || strings.Contains(n, "credential") ||
		strings.Contains(n, "pass") || strings.HasSuffix(n, "key") || strings.HasSuffix(n, "pwd") || n == "auth" ||
		strings.Contains(n, "cookie") || strings.Contains(n, "session")
}

func looksSecretValue(a string) bool {
	prefixes := []string{"ghp_", "github_pat_", "sk-", "xox", "AQ.", "AIza", "ya29.", "eyJ"}
	for _, p := range prefixes {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

func mcpFromMap(name string, obj map[string]any) MCP {
	m := MCP{Name: name, Env: map[string]string{}, Headers: map[string]string{}}
	if t, _ := obj["type"].(string); t != "" {
		m.Transport = t
	}
	if t, _ := obj["transport"].(string); t != "" {
		m.Transport = t
	}
	if v, _ := obj["disabled"].(bool); v {
		m.Disabled = true
	}
	if v, ok := obj["enabled"].(bool); ok && !v {
		m.Disabled = true
	}
	switch cmd := obj["command"].(type) {
	case string:
		m.Command = cmd
	case []any:
		for i, v := range cmd {
			s, _ := v.(string)
			if i == 0 {
				m.Command = s
				continue
			}
			m.Args = append(m.Args, s)
		}
	}
	if args, ok := obj["args"].([]any); ok {
		for _, v := range args {
			s, _ := v.(string)
			m.Args = append(m.Args, s)
		}
	}
	if u, _ := obj["url"].(string); u != "" {
		m.URL = u
	}
	if u, _ := obj["serverUrl"].(string); u != "" && m.URL == "" {
		m.URL = u
	}
	readMap := func(v any) map[string]string {
		out := map[string]string{}
		mm, _ := v.(map[string]any)
		for k, val := range mm {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	if v, ok := obj["env"]; ok {
		m.Env = readMap(v)
	}
	if v, ok := obj["environment"]; ok {
		m.Env = readMap(v)
	}
	if v, ok := obj["headers"]; ok {
		m.Headers = readMap(v)
	}
	if v, ok := obj["http_headers"]; ok {
		m.Headers = readMap(v)
	}
	return m
}

func encodeServersValue(m MCP) (string, error) {
	obj := map[string]any{}
	if m.Command != "" {
		obj["command"] = m.Command
		if len(m.Args) > 0 {
			obj["args"] = m.Args
		}
	}
	if m.URL != "" {
		obj["url"] = m.URL
		if m.SSE() {
			obj["type"] = "sse"
		} else if t := canonicalTransport(m.Transport, m.URL); t != "stdio" && t != "local" {
			obj["type"] = t
		}
	} else if t := canonicalTransport(m.Transport, m.URL); t != "stdio" && t != "local" && m.Transport != "" {
		obj["type"] = t
	}
	if len(m.Env) > 0 {
		obj["env"] = m.Env
	}
	if len(m.Headers) > 0 {
		obj["headers"] = m.Headers
	}
	return marshalJSONValue(obj)
}

// encodeAntigravityValue follows Antigravity's bundled mcp_servers.md:
// remote servers use url (serverUrl is the legacy key) over Streamable HTTP
// only, with no type field.
func encodeAntigravityValue(m MCP) (string, error) {
	obj := map[string]any{}
	if m.Command != "" {
		obj["command"] = m.Command
		if len(m.Args) > 0 {
			obj["args"] = m.Args
		}
	}
	if m.URL != "" {
		obj["url"] = m.URL
	}
	if len(m.Env) > 0 {
		obj["env"] = m.Env
	}
	if len(m.Headers) > 0 {
		obj["headers"] = m.Headers
	}
	return marshalJSONValue(obj)
}

// Keys this tool translates, plus the on/off switches it sets from Disabled.
var managedKeys = map[string]bool{
	"command": true, "args": true, "url": true, "serverUrl": true, "type": true, "transport": true,
	"env": true, "environment": true, "headers": true, "http_headers": true,
	"disabled": true, "enabled": true,
}

// jsonEntry finishes an encoded entry: the switch from m.Disabled, then keys
// pasted with m, then the replaced entry's other keys (timeouts, tool lists,
// oauth…). An old switch is never carried over, so a write sets the state.
func jsonEntry(raw string, m MCP, old map[string]any, opencode bool) (string, error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", err
	}
	for _, src := range []map[string]any{m.Extra, old} {
		for k, v := range src {
			if managedKeys[k] {
				continue
			}
			if _, ok := obj[k]; !ok {
				obj[k] = v
			}
		}
	}
	if m.Disabled {
		if opencode {
			obj["enabled"] = false
		} else {
			obj["disabled"] = true
		}
	}
	return marshalJSONValue(obj)
}

func encodeOpenCodeValue(m MCP) (string, error) {
	obj := map[string]any{}
	if m.URL != "" {
		obj["type"] = "remote"
		obj["url"] = m.URL
		if len(m.Headers) > 0 {
			obj["headers"] = m.Headers
		}
	} else {
		obj["type"] = "local"
		cmd := []string{}
		if m.Command != "" {
			cmd = append(cmd, m.Command)
		}
		cmd = append(cmd, m.Args...)
		obj["command"] = cmd
		if len(m.Env) > 0 {
			obj["environment"] = m.Env
		}
	}
	return marshalJSONValue(obj)
}

// Placement is one name on one agent config file.
type Placement struct {
	Agent    model.Agent
	Path     string
	Dialect  dialect
	MCP      MCP
	Disabled bool
}

// Row is one MCP name across the matrix.
type Row struct {
	Name        string
	Summary     string
	Drift       bool
	Fingerprint string
	ByAgent     map[model.Agent]Placement
}

// Source is the copy a row shows, filters on and copies to other agents: the
// first entry in matrix order that is turned on, else the first entry.
func (r Row) Source() (MCP, bool) {
	agents := MatrixAgents()
	for _, wantOn := range []bool{true, false} {
		for _, agent := range agents {
			if p, ok := r.ByAgent[agent]; ok && p.Disabled != wantOn {
				return p.MCP, true
			}
		}
	}
	for _, p := range r.ByAgent {
		return p.MCP, true
	}
	return MCP{}, false
}

func buildRows(places []Placement) []Row {
	order := []string{}
	index := map[string]*Row{}
	for _, p := range places {
		r, ok := index[p.MCP.Name]
		if !ok {
			r = &Row{Name: p.MCP.Name, ByAgent: map[model.Agent]Placement{}, Fingerprint: p.MCP.Fingerprint(), Summary: p.MCP.Summary()}
			index[p.MCP.Name] = r
			order = append(order, p.MCP.Name)
		}
		r.ByAgent[p.Agent] = p
		if r.Fingerprint != p.MCP.Fingerprint() {
			r.Drift = true
		}
	}
	out := make([]Row, 0, len(order))
	for _, name := range order {
		r := *index[name]
		if src, ok := r.Source(); ok {
			r.Summary = src.Summary()
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
