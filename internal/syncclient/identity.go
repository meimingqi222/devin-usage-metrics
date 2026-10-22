package syncclient

import (
	"bytes"
	"encoding/json"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"math"
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
