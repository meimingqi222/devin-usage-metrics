package data

import (
	"bytes"
	"encoding/json"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// A nil projection keeps the complete value (including dynamically named
// model/usage fields). A title projection preserves the input needed by text().
// These schemas are immutable after initialization and follow the importer.
type jsonProjection struct {
	fields map[string]*jsonProjection
	title  bool
}

func keepJSON(keys ...string) *jsonProjection {
	p := &jsonProjection{fields: make(map[string]*jsonProjection, len(keys))}
	for _, key := range keys {
		p.fields[key] = nil
	}
	return p
}

var transcriptProjections = func() map[model.Agent]*jsonProjection {
	result := map[model.Agent]*jsonProjection{}
	title := &jsonProjection{title: true}
	for _, a := range []model.Agent{model.Codex, model.Pi, model.WorkBuddy} {
		p := keepJSON("type", "sessionId", "session_id", "timestamp", "cwd", "aiTitle", "customTitle", "id", "thread_name", "thinkingLevel")
		p.fields["message"] = keepJSON("role", "model", "provider", "usage")
		p.fields["message"].fields["content"] = title
		switch a {
		case model.Codex:
			p.fields["payload"] = keepJSON("id", "session_id", "cwd", "source", "model", "collaboration_mode", "role", "type", "message", "info")
			p.fields["payload"].fields["content"] = title
			p.fields["payload"].fields["info"] = keepJSON("last_token_usage", "total_token_usage")
		case model.WorkBuddy:
			p.fields["providerData"] = keepJSON("usage", "rawUsage", "agent", "messageId", "model", "requestModelId", "requestModelName")
		}
		result[a] = p
	}
	grok := keepJSON("timestamp")
	grok.fields["_meta"] = keepJSON("agentTimestampMs")
	grok.fields["params"] = keepJSON()
	grok.fields["params"].fields["update"] = keepJSON("sessionUpdate", "usage")
	result[model.Grok] = grok
	return result
}()

var devinMessageProjection = func() *jsonProjection {
	p := keepJSON("message_id")
	p.fields["metadata"] = keepJSON("metrics", "started_generation_at", "created_at", "request_id", "generation_model")
	return p
}()

var sqlMessageProjection = keepJSON("role", "model", "providerID", "modelID", "tokens", "cost", "time", "mode")

func decodeProjectedJSON(raw []byte, p *jsonProjection) (obj, error) {
	raw = bytes.Trim(raw, " \t\r\n")
	fallback := func() (obj, error) {
		var v obj
		err := json.Unmarshal(raw, &v)
		return v, err
	}
	if !json.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
		return fallback()
	}
	v, err := projectJSONValue(raw, p)
	if err != nil {
		return fallback()
	}
	return v.(obj), nil
}

func projectJSONValue(raw []byte, p *jsonProjection) (any, error) {
	decode := func(raw []byte) (any, error) {
		var v any
		err := json.Unmarshal(raw, &v)
		return v, err
	}
	if p != nil && p.title {
		var err error
		title := contentTitlePrefix(raw, func(raw []byte) any {
			var v any
			v, err = decode(trimLongStrings(raw))
			return v
		})
		return title, err
	}
	if p == nil || raw[0] != '{' {
		return decode(raw)
	}
	v := obj{}
	var err error
	overflow := visitJSONFields(raw, func(key string, value []byte) {
		if child, ok := p.fields[key]; ok {
			var e error
			v[key], e = projectJSONValue(value, child)
			if e != nil {
				err = e
			}
		}
	})
	if overflow {
		return decode(raw)
	}
	return v, err
}
