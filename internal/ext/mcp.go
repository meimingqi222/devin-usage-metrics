package ext

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

var ErrSSEUnsupported = errors.New("remote SSE MCP is not supported on this agent")

func AcceptsSSE(agent model.Agent) bool {
	switch agent {
	case model.Antigravity, model.Codex:
		return false
	default:
		return true
	}
}

func InventoryMCP() ([]Row, error) {
	var places []Placement
	var errs []error
	for _, t := range matrixTargets() {
		if t.cap != CapMCPSkills {
			continue
		}
		got, err := readAgentMCP(t)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.agent.Label(), err))
		}
		places = append(places, got...)
	}
	return buildRows(places), errors.Join(errs...)
}

func readAgentMCP(t target) ([]Placement, error) {
	paths := []string{t.mcpPath}
	paths = append(paths, extraMCPFiles(t)...)
	var out []Placement
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		dialect := t.mcpDialect
		key := t.mcpKey
		if strings.HasSuffix(path, "mcp.json") && t.agent == model.OpenCode {
			dialect, key = dialectServers, "mcpServers"
		}
		servers, err := parseMCPFile(text, dialect, key)
		for _, m := range servers {
			id := string(t.agent) + ":" + m.Name
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, Placement{Agent: t.agent, Path: path, Dialect: dialect, MCP: m, Disabled: m.Disabled})
		}
		if err != nil {
			return out, fmt.Errorf("%s: %w", path, err)
		}
	}
	return out, nil
}

func parseMCPFile(text string, d dialect, key string) ([]MCP, error) {
	if d == dialectTOML {
		names, err := listTOMLServers(text)
		if err != nil {
			return nil, err
		}
		var out []MCP
		var errs []error
		for _, name := range names {
			m, ok, err := parseTOMLServer(text, name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			if ok {
				out = append(out, m)
			}
		}
		return out, errors.Join(errs...)
	}
	members, _, _, err := jsoncPathObject(text, []string{key})
	if err == errJSONCMissing {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []MCP
	for _, mem := range members {
		obj, err := decodeObject(text[mem.valueStart:mem.valueEnd])
		if err != nil {
			return nil, err
		}
		out = append(out, mcpFromMap(mem.key, obj))
	}
	return out, nil
}

// jsoncMemberObject is the current object for name under path, or nil.
func jsoncMemberObject(text string, path []string, name string) map[string]any {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	members, _, _, err := jsoncPathObject(text, path)
	if err != nil {
		return nil
	}
	for _, mem := range members {
		if mem.key == name {
			obj, _ := decodeObject(text[mem.valueStart:mem.valueEnd])
			return obj
		}
	}
	return nil
}

func decodeObject(raw string) (map[string]any, error) {
	raw = stripJSONC(raw)
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// ParsePaste accepts a community {"mcpServers":{...}} snippet, an OpenCode
// {"mcp":{...}} object, or a bare map of server objects.
func ParsePaste(s string) ([]MCP, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty MCP JSON")
	}
	obj, err := decodeObject(s)
	if err != nil {
		return nil, err
	}
	if v, ok := obj["mcpServers"]; ok {
		return serversFromAny(v)
	}
	if v, ok := obj["mcp"]; ok {
		return serversFromAny(v)
	}
	return serversFromAny(obj)
}

func serversFromAny(v any) ([]MCP, error) {
	mm, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("mcp servers must be an object")
	}
	var out []MCP
	for name, raw := range mm {
		o, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", name)
		}
		m := mcpFromMap(name, o)
		for k, v := range o {
			if !managedKeys[k] {
				if m.Extra == nil {
					m.Extra = map[string]any{}
				}
				m.Extra[k] = v
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// ApplyMCP copies src onto agents that lack it; the copy is turned on even
// when src is off in its own agent.
func ApplyMCP(name string, src MCP, agents []model.Agent) error {
	src.Name = name
	src.Disabled = false
	src.Extra = nil
	for _, agent := range agents {
		if src.SSE() && !AcceptsSSE(agent) {
			return fmt.Errorf("%s: %w", agent.Label(), ErrSSEUnsupported)
		}
	}
	_, err := applyMCPs([]MCP{src}, agents, false)
	return err
}

func ApplyMCPs(servers []MCP, agents []model.Agent) (skippedSSE bool, err error) {
	return applyMCPs(servers, agents, true)
}

func applyMCPs(servers []MCP, agents []model.Agent, skipSSE bool) (skippedSSE bool, err error) {
	err = withLock(func() error {
		var paths []string
		var targets []target
		for _, agent := range agents {
			t, ok := targetFor(agent)
			if !ok || t.cap != CapMCPSkills {
				return fmt.Errorf("%s does not support MCP writes", agent.Label())
			}
			targets = append(targets, t)
			paths = append(paths, t.mcpPath)
			paths = append(paths, extraMCPFiles(t)...)
		}
		snaps, err := captureFiles(paths)
		if err != nil {
			return err
		}
		wrote := false
		var names []string
		for _, src := range servers {
			if src.Name == "" {
				continue
			}
			names = append(names, src.Name)
			for _, t := range targets {
				if src.SSE() && !AcceptsSSE(t.agent) {
					if !skipSSE {
						_ = restoreSnaps(snaps)
						return fmt.Errorf("%s: %w", t.agent.Label(), ErrSSEUnsupported)
					}
					skippedSSE = true
					continue
				}
				if err := writeMCP(t, src); err != nil {
					_ = restoreSnaps(snaps)
					return err
				}
				wrote = true
			}
		}
		if !wrote {
			return nil
		}
		return saveSnapshot("mcp-upsert", strings.Join(names, ","), snaps)
	})
	return skippedSSE, err
}

// EnableMCP turns an existing, switched-off entry back on in the file that
// holds it, keeping the entry's other keys.
func EnableMCP(name string, agent model.Agent) error {
	return withLock(func() error {
		t, ok := targetFor(agent)
		if !ok || t.cap != CapMCPSkills {
			return fmt.Errorf("%s does not support MCP writes", agent.Label())
		}
		got, err := readAgentMCP(t)
		var place *Placement
		for i := range got {
			if got[i].MCP.Name == name {
				place = &got[i]
				break
			}
		}
		if place == nil {
			if err != nil {
				return err
			}
			return fmt.Errorf("%s: %s is not configured", agent.Label(), name)
		}
		if !place.Disabled {
			return nil
		}
		snaps, err := captureFiles([]string{place.Path})
		if err != nil {
			return err
		}
		tt := t
		tt.mcpPath, tt.mcpDialect = place.Path, place.Dialect
		if place.Dialect == dialectServers && agent == model.OpenCode {
			tt.mcpKey = "mcpServers"
		}
		m := place.MCP
		m.Disabled = false
		if err := writeOneMCP(tt, m); err != nil {
			_ = restoreSnaps(snaps)
			return err
		}
		return saveSnapshot("mcp-enable", name, snaps)
	})
}

func RemoveMCP(name string, agents []model.Agent) error {
	return withLock(func() error {
		var paths []string
		var targets []target
		for _, agent := range agents {
			t, ok := targetFor(agent)
			if !ok || t.cap != CapMCPSkills {
				continue
			}
			targets = append(targets, t)
			paths = append(paths, t.mcpPath)
			paths = append(paths, extraMCPFiles(t)...)
		}
		snaps, err := captureFiles(paths)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if err := deleteMCP(t, name); err != nil {
				_ = restoreSnaps(snaps)
				return err
			}
		}
		return saveSnapshot("mcp-remove", name, snaps)
	})
}

func writeOneMCP(t target, m MCP) error {
	text, err := readText(t.mcpPath)
	if err != nil {
		return err
	}
	var edited string
	switch t.mcpDialect {
	case dialectTOML:
		edited, err = upsertTOMLServer(text, m)
	case dialectOpenCode:
		raw, e := encodeOpenCodeValue(m)
		if e == nil {
			raw, e = jsonEntry(raw, m, jsoncMemberObject(text, []string{t.mcpKey}, m.Name), true)
		}
		if e != nil {
			return e
		}
		edited, err = upsertJSONC(text, m.Name, []string{t.mcpKey}, raw)
	default:
		encode := encodeServersValue
		if t.agent == model.Antigravity {
			encode = encodeAntigravityValue
		}
		raw, e := encode(m)
		if e == nil {
			raw, e = jsonEntry(raw, m, jsoncMemberObject(text, []string{t.mcpKey}, m.Name), false)
		}
		if e != nil {
			return e
		}
		edited, err = upsertJSONC(text, m.Name, []string{t.mcpKey}, raw)
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.mcpPath), 0700); err != nil {
		return err
	}
	return atomicWrite(t.mcpPath, []byte(edited))
}

func writeMCP(t target, m MCP) error {
	paths := []string{t.mcpPath}
	if t.agent == model.WorkBuddy {
		paths = append(paths, extraMCPFiles(t)...)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		tt := t
		tt.mcpPath = path
		if err := writeOneMCP(tt, m); err != nil {
			return err
		}
	}
	return nil
}

func deleteMCP(t target, name string) error {
	paths := append([]string{t.mcpPath}, extraMCPFiles(t)...)
	for _, path := range paths {
		text, err := readText(path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		var edited string
		if strings.HasSuffix(path, ".toml") || t.mcpDialect == dialectTOML && path == t.mcpPath {
			edited, err = deleteTOMLServer(text, name)
		} else {
			key := t.mcpKey
			if strings.HasSuffix(path, "mcp.json") && t.agent == model.OpenCode {
				key = "mcpServers"
			}
			edited, err = deleteJSONC(text, name, []string{key})
		}
		if err != nil {
			return err
		}
		if edited != text {
			if err := atomicWrite(path, []byte(edited)); err != nil {
				return err
			}
		}
	}
	return nil
}
