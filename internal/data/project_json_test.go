package data

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func referenceJSONProjection(v any, p *jsonProjection) any {
	if p == nil {
		return v
	}
	if p.title {
		return text(v)
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return v
	}
	out := obj{}
	for key, value := range m {
		if child, ok := p.fields[key]; ok {
			out[key] = referenceJSONProjection(value, child)
		}
	}
	return out
}

func FuzzAgentJSONProjection(f *testing.F) {
	for _, s := range []string{
		`{"message":{"content":"` + strings.Repeat("a ", 50) + `tail"}}`,
		`{}`, `null`, `[]`, `{"unknown":[{"n":1e999}]}`,
		`{"type":"message","message":{"role":"user","content":[{"text":" "},{"text":"标题 😀"}]}}`,
		`{"type":"assistant","message":{"usage":{"input":3,"cacheRead":4,"cost":{"total":0.5}},"provider":"p","model":"m"}}`,
		`{"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":4},"total_token_usage":{"input_tokens":5}}}}`,
		`{"providerData":{"usage":{"inputTokensDetails":[{"cachedTokens":2}]},"rawUsage":{"prompt_tokens_details":{"cached_tokens":3}}}}`,
		`{"params":{"update":{"sessionUpdate":"turn_completed","usage":{"modelUsage":{"model-z":{"inputTokens":1},"model-a":{"outputTokens":2}}}}}}`,
		`{"message_id":"m","metadata":{"metrics":{"ttft_ms":null,"input_tokens":"7"},"request_id":"r"}}`,
		`{"role":"assistant","model":{"providerID":"p","id":"m"},"tokens":{"input":4,"cache":{"read":2}},"time":{"created":1,"completed":4}}`,
		`{"payload":{"source":{"subagent":{"guardian":"x"}},"collaboration_mode":"plan","content":"title"},"ignored":"\q"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		schemas := []*jsonProjection{devinMessageProjection, sqlMessageProjection}
		for _, p := range transcriptProjections {
			schemas = append(schemas, p)
		}
		for _, p := range schemas {
			var full obj
			wantErr := json.Unmarshal(raw, &full)
			got, err := decodeProjectedJSON(raw, p)
			if (wantErr != nil) != (err != nil) {
				t.Fatalf("error mismatch: %v vs %v", wantErr, err)
			}
			if err != nil {
				continue
			}
			if !reflect.DeepEqual(referenceJSONProjection(full, p), referenceJSONProjection(got, p)) {
				t.Fatal("projected fields differ from generic JSON")
			}
		}
	})
}
