package compaction

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

var configMu sync.Mutex

type target struct {
	path, journal, name string
	claude              bool
}
type undoRecord struct {
	Before   *uint64 `json:"before"`
	Written  uint64  `json:"written"`
	Previous *uint64 `json:"previous_written"`
}

var native = regexp.MustCompile(`(?:^|[-./])claude-(opus|sonnet|haiku|fable)[-.]([0-9]+)(?:[-.]([0-9]{1,2})(?:[^0-9]|$))?`)

func local(agent model.Agent, name string) (target, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return target{}, e
	}
	dir, e := os.UserConfigDir()
	if e != nil {
		return target{}, e
	}
	t := target{journal: filepath.Join(dir, "devin-usage-metrics", "compaction-undo.json")}
	root := ""
	switch agent {
	case model.Claude:
		t.claude = true
		m := native.FindStringSubmatch(strings.ToLower(name))
		if m == nil {
			return t, errors.New("unsupported Claude model")
		}
		t.name = "claude-" + m[1] + "-" + m[2]
		if m[3] != "" {
			t.name += "-" + m[3]
		}
		root = os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		t.path = filepath.Join(root, "settings.json")
	case model.Codex:
		root = os.Getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		t.path = filepath.Join(root, "config.toml")
	default:
		return t, errors.New("unsupported agent")
	}
	if p, e := filepath.EvalSymlinks(t.path); e == nil {
		t.path = p
	}
	return t, nil
}
func readText(path string) (string, error) {
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return "", nil
	}
	return string(b), e
}

type span struct {
	key                string
	start, end, member int
}

// objectSpans validates JSON and records exact value offsets, leaving all other bytes untouched.
func objectSpans(s string) ([]span, error) {
	if strings.TrimSpace(s) == "" {
		s = "{}"
	}
	var obj map[string]json.RawMessage
	if e := json.Unmarshal([]byte(s), &obj); e != nil {
		return nil, e
	}
	if obj == nil {
		return nil, errors.New("settings must be an object")
	}
	d := json.NewDecoder(strings.NewReader(s))
	if _, e := d.Token(); e != nil {
		return nil, e
	}
	var out []span
	seen := map[string]bool{}
	for d.More() {
		member := int(d.InputOffset())
		tok, e := d.Token()
		if e != nil {
			return nil, e
		}
		key := tok.(string)
		if seen[key] {
			return nil, errors.New("duplicate settings key")
		}
		seen[key] = true
		i := int(d.InputOffset())
		for i < len(s) && (s[i] == ':' || s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t') {
			i++
		}
		var raw json.RawMessage
		if e = d.Decode(&raw); e != nil {
			return nil, e
		}
		out = append(out, span{key, i, int(d.InputOffset()), member})
	}
	return out, nil
}
func jsonField(s string, path []string, value *uint64, edit bool) (string, *uint64, error) {
	if strings.TrimSpace(s) == "" {
		s = "{}"
	}
	sp, e := objectSpans(s)
	if e != nil {
		return "", nil, e
	}
	for i, p := range sp {
		if p.key != path[0] {
			continue
		}
		if len(path) > 1 {
			child, old, e := jsonField(s[p.start:p.end], path[1:], value, edit)
			if e != nil {
				return "", nil, e
			}
			if edit && value == nil && old != nil {
				members, err := objectSpans(child)
				if err == nil && len(members) == 0 {
					return removeMember(s, sp, i), old, nil
				}
			}
			return s[:p.start] + child + s[p.end:], old, nil
		}
		var old uint64
		if e = json.Unmarshal([]byte(s[p.start:p.end]), &old); e != nil {
			return "", nil, errors.New("threshold must be a nonnegative integer")
		}
		if !edit {
			return s, &old, nil
		}
		if value != nil {
			return s[:p.start] + strconv.FormatUint(*value, 10) + s[p.end:], &old, nil
		}
		return removeMember(s, sp, i), &old, nil
	}
	if !edit || value == nil {
		return s, nil, nil
	}
	raw := strconv.FormatUint(*value, 10)
	for i := len(path) - 1; i >= 0; i-- {
		k, _ := json.Marshal(path[i])
		raw = "{" + string(k) + ":" + raw + "}"
	}
	raw = raw[1 : len(raw)-1]
	at := strings.LastIndex(s, "}")
	prefix := ""
	if len(sp) > 0 {
		prefix = ","
	}
	return s[:at] + prefix + raw + s[at:], nil, nil
}

func removeMember(s string, sp []span, i int) string {
	p := sp[i]
	a, b := p.member, p.end
	if i == 0 {
		a = strings.Index(s, "{") + 1
		if len(sp) > 1 {
			b = sp[1].member + 1
		}
	}
	return s[:a] + s[b:]
}

var tomlKey = regexp.MustCompile(`^([ \t]*model_auto_compact_token_limit[ \t]*=[ \t]*)([0-9][0-9_]*)([ \t]*(?:#.*)?)(\r?\n)?$`)

func (t target) field(s string, value *uint64, edit bool) (string, *uint64, error) {
	if t.claude {
		return jsonField(s, []string{"modelSettings", t.name, "autoCompactWindow"}, value, edit)
	}
	lines := strings.SplitAfter(s, "\n")
	offset := 0
	var found *uint64
	result := s
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		// Refuse ambiguous constructs rather than risk editing text inside a
		// multiline string or shadowing a quoted/dotted spelling of the key.
		if !strings.HasPrefix(trim, "#") && (strings.Contains(trim, `"""`) || strings.Contains(trim, "'''")) {
			return "", nil, errors.New("multiline TOML requires a preserving TOML parser")
		}
		if strings.HasPrefix(trim, "[") {
			break
		}
		if trim != "" && !strings.HasPrefix(trim, "#") {
			key, _, ok := strings.Cut(trim, "=")
			if !ok {
				return "", nil, errors.New("invalid TOML assignment")
			}
			key = strings.TrimSpace(key)
			if strings.Contains(key, "model_auto_compact_token_limit") && key != "model_auto_compact_token_limit" {
				return "", nil, errors.New("quoted or dotted TOML threshold requires a preserving TOML parser")
			}
		}
		if strings.HasPrefix(trim, "model_auto_compact_token_limit") {
			m := tomlKey.FindStringSubmatch(line)
			if m == nil || found != nil {
				return "", nil, errors.New("unsupported or duplicate TOML threshold")
			}
			n, e := strconv.ParseUint(strings.ReplaceAll(m[2], "_", ""), 10, 64)
			if e != nil {
				return "", nil, e
			}
			found = &n
			if edit {
				replacement := ""
				if value != nil {
					replacement = m[1] + strconv.FormatUint(*value, 10) + m[3] + m[4]
				}
				result = s[:offset] + replacement + s[offset+len(line):]
			}
		}
		offset += len(line)
	}
	if found == nil && edit && value != nil {
		result = "model_auto_compact_token_limit = " + strconv.FormatUint(*value, 10) + "\n" + s
	}
	return result, found, nil
}
func (t target) locked(s string) bool {
	if !t.claude {
		return false
	}
	var root map[string]any
	if json.Unmarshal([]byte(s), &root) != nil {
		return false
	}
	env, _ := root["env"].(map[string]any)
	for _, k := range []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"} {
		v := env[k]
		if os.Getenv(k) != "" || (v != nil && v != "") {
			return true
		}
	}
	return false
}
func equal(a, b *uint64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func atomicWrite(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".compaction-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if st, e := os.Stat(path); e == nil {
		if e = f.Chmod(st.Mode().Perm()); e != nil {
			return e
		}
	}
	if _, e = f.Write(b); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(path))
}
func Apply(agent model.Agent, modelName string, threshold uint64) error {
	return change(agent, modelName, &threshold, nil)
}
func Undo(agent model.Agent, modelName string) error { return change(agent, modelName, nil, nil) }

// Prepare captures the configuration shown before confirmation. The returned
// operation refuses to overwrite a file edited while the dialog was open.
func Prepare(agent model.Agent, modelName string, threshold uint64) (func() error, error) {
	t, err := local(agent, modelName)
	if err != nil {
		return nil, err
	}
	text, err := readText(t.path)
	if err != nil {
		return nil, err
	}
	return func() error { return change(agent, modelName, &threshold, &text) }, nil
}

// Pending keeps undo discoverable after restart even without transcript data.
func Pending(agent model.Agent) ([]string, error) {
	t, err := local(agent, "claude-opus-4-6")
	if err != nil {
		return nil, err
	}
	text, err := readText(t.journal)
	if err != nil || text == "" {
		return nil, err
	}
	var records map[string]undoRecord
	if err := json.Unmarshal([]byte(text), &records); err != nil {
		return nil, err
	}
	var names []string
	for id := range records {
		key, ok := strings.CutPrefix(id, t.path+":")
		if !ok {
			continue
		}
		if t.claude {
			name, ok := strings.CutPrefix(key, "modelSettings.")
			if ok && strings.HasSuffix(name, ".autoCompactWindow") {
				names = append(names, strings.TrimSuffix(name, ".autoCompactWindow"))
			}
		} else if key == "model_auto_compact_token_limit" {
			names = append(names, "")
		}
	}
	return names, nil
}

func change(agent model.Agent, name string, trigger *uint64, expected *string) error {
	configMu.Lock()
	defer configMu.Unlock()
	t, e := local(agent, name)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(t.journal), 0700); e != nil {
		return e
	}
	lock, e := os.OpenFile(strings.TrimSuffix(t.journal, ".json")+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	unlock, e := lockFile(lock)
	if e != nil {
		return e
	}
	defer unlock()
	text, e := readText(t.path)
	if e != nil {
		return e
	}
	if expected != nil && text != *expected {
		return errors.New("configuration changed; review the recommendation again")
	}
	_, old, e := t.field(text, nil, false)
	if e != nil {
		return e
	}
	journal := map[string]undoRecord{}
	j, e := readText(t.journal)
	if e != nil {
		return e
	}
	if j != "" {
		if e = json.Unmarshal([]byte(j), &journal); e != nil {
			return e
		}
		if journal == nil {
			return errors.New("invalid undo journal")
		}
	}
	key := "model_auto_compact_token_limit"
	if t.claude {
		key = "modelSettings." + t.name + ".autoCompactWindow"
	}
	id := t.path + ":" + key
	u, exists := journal[id]
	ours := exists && (equal(old, &u.Written) || u.Previous != nil && equal(old, u.Previous))
	if exists && !ours && !equal(old, u.Before) {
		return errors.New("configuration edited externally; undo retained")
	}
	var value *uint64
	if trigger != nil {
		if t.locked(text) {
			return errors.New("environment override takes precedence")
		}
		n := *trigger
		minimum := uint64(50000)
		if t.claude {
			if n > 1000000-CompactReserve {
				return errors.New("threshold out of range")
			}
			n += CompactReserve
			minimum = 100000
		}
		if n < minimum || n > 1000000 {
			return errors.New("threshold out of range")
		}
		value = &n
		if !exists {
			u.Before = old
		}
		u.Previous = nil
		if ours {
			u.Previous = old
		}
		u.Written = n
		journal[id] = u
	} else {
		if !exists {
			return errors.New("no saved change to undo")
		}
		value = u.Before
	}
	edited, _, e := t.field(text, value, true)
	if e != nil {
		return e
	}
	save := func() error {
		b, e := json.MarshalIndent(journal, "", "  ")
		if e != nil {
			return e
		}
		return atomicWrite(t.journal, b)
	}
	if e = save(); e != nil {
		return e
	}
	now, e := readText(t.path)
	if e != nil {
		return e
	}
	if now != text {
		return errors.New("configuration changed during save")
	}
	if e = atomicWrite(t.path, []byte(edited)); e != nil {
		return e
	}
	if trigger == nil {
		delete(journal, id)
	} else {
		u.Previous = nil
		journal[id] = u
	}
	return save()
}
func CurrentThresholds(agent model.Agent, deviceID string) map[[2]string]uint64 {
	out := map[[2]string]uint64{}
	name := ""
	if agent == model.Claude {
		name = "claude-opus-4"
	}
	t, e := local(agent, name)
	if e != nil {
		return out
	}
	s, e := readText(t.path)
	if e != nil || t.locked(s) {
		return out
	}
	if agent == model.Codex {
		_, v, e := t.field(s, nil, false)
		if e == nil && v != nil {
			out[[2]string{deviceID, CodexGroup}] = *v
		}
		return out
	}
	var root struct {
		Models map[string]json.RawMessage `json:"modelSettings"`
		Global *uint64                    `json:"autoCompactWindow"`
	}
	if json.Unmarshal([]byte(s), &root) != nil {
		return out
	}
	// The empty label is the global Claude fallback for models not enumerated
	// in modelSettings. Analyze resolves native aliases before consulting it.
	if root.Global != nil {
		out[[2]string{deviceID, ""}] = sub(*root.Global, CompactReserve)
	}
	for name := range root.Models {
		t.name = name
		_, v, e := t.field(s, nil, false)
		if e == nil {
			if v == nil {
				v = root.Global
			}
			if v != nil {
				out[[2]string{deviceID, strings.ToLower(name)}] = sub(*v, CompactReserve)
			}
		}
	}
	return out
}
