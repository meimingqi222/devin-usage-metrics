// Package model defines the portable records shared by importers, analytics and sync.
// JSON names retain the existing desktop application's on-disk contract.
package model

type Agent string

const (
	Devin       Agent = "devin"
	Claude      Agent = "claude"
	Codex       Agent = "codex"
	Antigravity Agent = "antigravity"
	Grok        Agent = "grok"
	ZCode       Agent = "zcode"
	OpenCode    Agent = "opencode"
	Pi          Agent = "pi"
	Omp         Agent = "omp"
	MimoCode    Agent = "mimocode"
	WorkBuddy   Agent = "workbuddy"
)

var Agents = []Agent{Devin, Claude, Codex, Antigravity, Grok, ZCode, OpenCode, Pi, Omp, MimoCode, WorkBuddy}

func (a Agent) Label() string {
	labels := map[Agent]string{Devin: "Devin", Claude: "Claude Code", Codex: "Codex", Antigravity: "Antigravity", Grok: "Grok Build", ZCode: "ZCode", OpenCode: "OpenCode", Pi: "pi-agent", Omp: "omp", MimoCode: "MimoCode", WorkBuddy: "WorkBuddy"}
	if s, ok := labels[a]; ok {
		return s
	}
	return string(a)
}

type Session struct {
	Agent                 Agent    `json:"agent"`
	Key                   string   `json:"key"`
	Source                string   `json:"source"`
	ID                    string   `json:"id"`
	Title                 string   `json:"title"`
	WorkingDirectory      string   `json:"working_directory"`
	SelectedModel         string   `json:"selected_model"`
	RealModel             *string  `json:"real_model"`
	AgentMode             string   `json:"agent_mode"`
	CreatedAt             int64    `json:"created_at"`
	LastActivityAt        int64    `json:"last_activity_at"`
	InputTokens           float64  `json:"input_tokens"`
	OutputTokens          float64  `json:"output_tokens"`
	CachedTokens          float64  `json:"cached_tokens"`
	CacheCreationTokens   float64  `json:"cache_creation_tokens"`
	CacheCreation5mTokens float64  `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens float64  `json:"cache_creation_1h_tokens"`
	AgentMessages         float64  `json:"agent_messages"`
	RecordedCost          *float64 `json:"recorded_cost"`
	DeviceID              string   `json:"device_id"`
	DeviceName            string   `json:"device_name"`
}

func (s Session) TotalTokens() float64 {
	return s.InputTokens + s.OutputTokens + s.CachedTokens + s.CacheCreationTokens
}
func (s Session) DisplayModel() string {
	if s.RealModel != nil && *s.RealModel != "" {
		return *s.RealModel
	}
	if s.SelectedModel != "" {
		return s.SelectedModel
	}
	return "unknown"
}

type Turn struct {
	Agent                 Agent    `json:"agent"`
	SessionKey            string   `json:"session_key"`
	CreatedAt             int64    `json:"created_at"`
	InputTokens           float64  `json:"input_tokens"`
	OutputTokens          float64  `json:"output_tokens"`
	CacheReadTokens       float64  `json:"cache_read_tokens"`
	CacheCreationTokens   float64  `json:"cache_creation_tokens"`
	CacheCreation5mTokens float64  `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens float64  `json:"cache_creation_1h_tokens"`
	Model                 string   `json:"model"`
	ContextStream         string   `json:"context_stream,omitempty"`
	ContextOrder          uint64   `json:"context_order"`
	TTFTMs                float64  `json:"ttft_ms"`
	TotalTimeMs           float64  `json:"total_time_ms"`
	RecordedCost          *float64 `json:"recorded_cost"`
	DeviceID              string   `json:"device_id"`
	DeviceName            string   `json:"device_name"`
	DedupKey              string   `json:"dedup_key,omitempty"`
}

type DataError struct {
	Agent   Agent  `json:"agent"`
	Message string `json:"message"`
}
type Data struct {
	Sessions        []Session         `json:"sessions"`
	Turns           []Turn            `json:"turns"`
	TurnsStart      int64             `json:"turns_start"`
	TurnsEnd        int64             `json:"turns_end"`
	Errors          []DataError       `json:"errors"`
	FileMtimes      map[string]int64  `json:"file_mtimes"`
	FileSessions    map[string]string `json:"file_sessions"`
	SyncContentHash string            `json:"sync_content_hash"`
}
