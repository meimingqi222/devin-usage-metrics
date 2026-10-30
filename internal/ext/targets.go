package ext

import (
	"os"
	"path/filepath"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// Capability says what this tool writes for an agent.
type Capability int

const (
	CapNone Capability = iota
	CapSkills
	CapMCPSkills
)

type target struct {
	agent      model.Agent
	cap        Capability
	mcpPath    string
	mcpDialect dialect
	mcpKey     string // json object key: mcpServers or mcp
	skillDirs  []string
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func configHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

func homePath(elem ...string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(append([]string{home}, elem...)...)
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func existingDirs(paths ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	return homePath(".codex")
}

func opencodeConfigDir() string {
	return firstExisting(
		filepath.Join(configHome(), "opencode"),
		homePath(".config", "opencode"),
	)
}

func workbuddyRoots() []string {
	var roots []string
	for _, key := range []string{"WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR", "WORKBUDDY_DIR"} {
		if v := os.Getenv(key); v != "" {
			roots = append(roots, v)
		}
	}
	roots = append(roots, homePath(".workbuddy"), homePath(".workbuddy-ai"), homePath(".codebuddy"))
	var out []string
	seen := map[string]bool{}
	for _, r := range roots {
		if seen[r] {
			continue
		}
		if _, err := os.Stat(r); err == nil {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func matrixTargets() []target {
	codex := codexHome()
	open := opencodeConfigDir()
	openJSON := firstExisting(filepath.Join(open, "opencode.jsonc"), filepath.Join(open, "opencode.json"))
	var wbMCP []string
	var wbSkills []string
	for _, r := range workbuddyRoots() {
		wbMCP = append(wbMCP, filepath.Join(r, "mcp.json"))
		wbSkills = append(wbSkills, filepath.Join(r, "skills"))
	}
	wbPath := ""
	if len(wbMCP) > 0 {
		wbPath = firstExisting(wbMCP...)
	} else {
		wbPath = homePath(".workbuddy", "mcp.json")
	}
	antigravityMCP := antigravityPick(antigravityMCPFiles())
	antigravitySkills := antigravityPick(antigravitySkillDirs())
	return []target{
		{agent: model.Claude, cap: CapMCPSkills, mcpPath: homePath(".claude.json"), mcpDialect: dialectServers, mcpKey: "mcpServers", skillDirs: []string{homePath(".claude", "skills")}},
		{agent: model.Codex, cap: CapMCPSkills, mcpPath: filepath.Join(codex, "config.toml"), mcpDialect: dialectTOML, skillDirs: []string{filepath.Join(codex, "skills")}},
		{agent: model.Antigravity, cap: CapMCPSkills, mcpPath: antigravityMCP, mcpDialect: dialectServers, mcpKey: "mcpServers", skillDirs: []string{antigravitySkills}},
		{agent: model.OpenCode, cap: CapMCPSkills, mcpPath: openJSON, mcpDialect: dialectOpenCode, mcpKey: "mcp", skillDirs: []string{filepath.Join(open, "skills")}},
		{agent: model.WorkBuddy, cap: CapMCPSkills, mcpPath: wbPath, mcpDialect: dialectServers, mcpKey: "mcpServers", skillDirs: wbSkills},
		{agent: model.Pi, cap: CapSkills, skillDirs: []string{homePath(".pi", "agent", "skills")}},
	}
}

func sharedSkillDir() string {
	return homePath(".agents", "skills")
}

func targetFor(agent model.Agent) (target, bool) {
	for _, t := range matrixTargets() {
		if t.agent == agent {
			return t, true
		}
	}
	return target{}, false
}

func MCPSupported(agent model.Agent) bool {
	t, ok := targetFor(agent)
	return ok && t.cap == CapMCPSkills
}

func SkillSupported(agent model.Agent) bool {
	t, ok := targetFor(agent)
	return ok && t.cap != CapNone
}

func extraMCPFiles(t target) []string {
	if t.agent == model.OpenCode {
		return []string{filepath.Join(opencodeConfigDir(), "mcp.json")}
	}
	if t.agent == model.WorkBuddy {
		var extra []string
		for _, r := range workbuddyRoots() {
			p := filepath.Join(r, "mcp.json")
			if p != t.mcpPath {
				extra = append(extra, p)
			}
		}
		return extra
	}
	if t.agent == model.Antigravity {
		return otherPaths(t.mcpPath, antigravityMCPFiles())
	}
	return nil
}

func extraSkillDirs(t target) []string {
	if t.agent == model.Antigravity && len(t.skillDirs) > 0 {
		return otherPaths(t.skillDirs[0], antigravitySkillDirs())
	}
	return nil
}

// Antigravity app and CLI both read ~/.gemini/config; the per-client paths are
// older locations, which newer installs keep as symlinks into ~/.gemini/config.
func antigravityMCPFiles() []string {
	return []string{
		homePath(".gemini", "config", "mcp_config.json"),
		homePath(".gemini", "antigravity", "mcp_config.json"),
		homePath(".gemini", "antigravity-cli", "mcp_config.json"),
	}
}

func antigravitySkillDirs() []string {
	return []string{
		homePath(".gemini", "config", "skills"),
		homePath(".gemini", "antigravity", "skills"),
		homePath(".gemini", "antigravity-cli", "skills"),
	}
}

// antigravityPick uses ~/.gemini/config once that folder exists, even before
// Antigravity creates the file in it: a leftover legacy file must not win.
// Older installs without the folder keep their legacy path.
func antigravityPick(paths []string) string {
	if st, err := os.Stat(homePath(".gemini", "config")); err == nil && st.IsDir() {
		return paths[0]
	}
	return firstExisting(paths...)
}

// otherPaths drops main and every path that resolves to a file already kept,
// so a legacy symlink (even to a file not created yet) is not read, captured
// or deleted twice.
func otherPaths(main string, paths []string) []string {
	seen := map[string]bool{filepath.Clean(resolvePath(main)): true}
	var out []string
	for _, p := range paths {
		key := filepath.Clean(resolvePath(p))
		if p == "" || samePath(p, main) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// inheritedSkillDirs are locations the agent loads without a copy in skillDirs.
// Codex and Pi load ~/.agents/skills; OpenCode also loads ~/.claude/skills.
func inheritedSkillDirs(t target) []string {
	var dirs []string
	switch t.agent {
	case model.Codex, model.Pi:
		dirs = []string{sharedSkillDir()}
	case model.OpenCode:
		dirs = []string{sharedSkillDir(), homePath(".claude", "skills")}
	default:
		return nil
	}
	skip := map[string]bool{}
	for _, d := range t.skillDirs {
		skip[filepath.Clean(d)] = true
	}
	for _, d := range extraSkillDirs(t) {
		skip[filepath.Clean(d)] = true
	}
	var out []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		key := filepath.Clean(d)
		if skip[key] {
			continue
		}
		skip[key] = true
		out = append(out, d)
	}
	return out
}

// ConfiguredAgents are matrix agents whose MCP file or skill directory exists,
// so they get a column even before their first recorded session.
func ConfiguredAgents() map[model.Agent]bool {
	out := map[model.Agent]bool{}
	for _, t := range matrixTargets() {
		paths := []string{t.mcpPath}
		paths = append(paths, extraMCPFiles(t)...)
		paths = append(paths, t.skillDirs...)
		paths = append(paths, extraSkillDirs(t)...)
		for _, p := range paths {
			if p == "" {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				out[t.agent] = true
				break
			}
		}
	}
	return out
}

func MatrixAgents() []model.Agent {
	var out []model.Agent
	for _, t := range matrixTargets() {
		out = append(out, t.agent)
	}
	return out
}
