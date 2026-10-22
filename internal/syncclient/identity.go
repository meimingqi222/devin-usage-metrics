package syncclient

import (
	"bytes"
	"encoding/json"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"math"
	"strings"
	"unicode/utf8"
)

func slug(a model.Agent) string {
	switch a {
	case model.Claude:
		return "claude_code"
	case model.Grok:
		return "grok_build"
	case model.Pi:
		return "pi_agent"
	}
	return string(a)
}
func sessionID(s model.Session) string {
	return hash([]byte("session-v1\x00" + s.DeviceID + "\x00" + slug(s.Agent) + "\x00" + s.Key))
}
func v3SessionID(s model.Session) string {
	names := map[model.Agent]string{model.Devin: "Devin", "amp": "Amp", model.Claude: "Claude", model.Codex: "Codex", model.Antigravity: "Antigravity", model.Grok: "Grok", model.ZCode: "ZCode", model.OpenCode: "OpenCode", model.Pi: "Pi", model.MimoCode: "MimoCode", model.WorkBuddy: "WorkBuddy"}
	return hash([]byte("session-v1\x00" + s.DeviceID + "\x00" + names[s.Agent] + "\x00" + s.Key))
}

// turnIdentity holds exactly the fields turnID encodes, so two turns have
// equal identities if and only if they have equal turnIDs. Publication merges
// about two turns per record every cycle; hashing canonical JSON for each was
// over half of its CPU time.
type turnIdentity struct {
	device, session, model                        string
	agent                                         model.Agent
	created                                       int64
	input, output, read, create, five, hour, ttft uint64
	total, cost                                   uint64
	hasCost                                       bool
}

// jsonText returns s as encoding/json writes it: each byte of invalid UTF-8
// becomes U+FFFD. turnID hashes that text, so distinct invalid bytes collide
// there and must collide here too.
func jsonText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func identityOf(t model.Turn) turnIdentity {
	id := turnIdentity{
		device:  jsonText(t.DeviceID),
		session: jsonText(t.SessionKey),
		model:   jsonText(t.Model),
		agent:   model.Agent(jsonText(string(t.Agent))),
		created: t.CreatedAt,
		input:   math.Float64bits(t.InputTokens),
		output:  math.Float64bits(t.OutputTokens),
		read:    math.Float64bits(t.CacheReadTokens),
		create:  math.Float64bits(t.CacheCreationTokens),
		five:    math.Float64bits(t.CacheCreation5mTokens),
		hour:    math.Float64bits(t.CacheCreation1hTokens),
		ttft:    math.Float64bits(t.TTFTMs),
		total:   math.Float64bits(t.TotalTimeMs),
	}
	if t.RecordedCost != nil {
		id.cost, id.hasCost = math.Float64bits(*t.RecordedCost), true
	}
	return id
}

func turnID(t model.Turn) string {
	var cost *uint64
	if t.RecordedCost != nil {
		v := math.Float64bits(*t.RecordedCost)
		cost = &v
	}
	v := struct {
		Version int         `json:"version"`
		Device  string      `json:"device"`
		Agent   model.Agent `json:"agent"`
		Session string      `json:"session"`
		Created int64       `json:"created_at"`
		Input   uint64      `json:"input"`
		Output  uint64      `json:"output"`
		Read    uint64      `json:"cache_read"`
		Create  uint64      `json:"cache_create"`
		Five    uint64      `json:"cache_5m"`
		Hour    uint64      `json:"cache_1h"`
		Model   string      `json:"model"`
		TTFT    uint64      `json:"ttft"`
		Total   uint64      `json:"total_time"`
		Cost    *uint64     `json:"recorded_cost"`
	}{1, t.DeviceID, t.Agent, t.SessionKey, t.CreatedAt, math.Float64bits(t.InputTokens), math.Float64bits(t.OutputTokens), math.Float64bits(t.CacheReadTokens), math.Float64bits(t.CacheCreationTokens), math.Float64bits(t.CacheCreation5mTokens), math.Float64bits(t.CacheCreation1hTokens), t.Model, math.Float64bits(t.TTFTMs), math.Float64bits(t.TotalTimeMs), cost}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	// serde_json writes these Unicode separators literally; encoding/json
	// escapes them even with HTML escaping disabled.
	canonical := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	var out bytes.Buffer
	for i := 0; i < len(canonical); i++ {
		if canonical[i] == '\\' && i+1 < len(canonical) {
			if bytes.HasPrefix(canonical[i:], []byte(`\u2028`)) {
				out.WriteString("\u2028")
				i += 5
				continue
			}
			if bytes.HasPrefix(canonical[i:], []byte(`\u2029`)) {
				out.WriteString("\u2029")
				i += 5
				continue
			}
			out.WriteByte(canonical[i])
			i++ // Consume escaped backslashes together, preserving literal \\u2028.
		}
		out.WriteByte(canonical[i])
	}
	return hash(out.Bytes())
}
