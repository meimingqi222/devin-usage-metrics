package data

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// decodeClaudeLine retains only fields consumed by loadFiles. In particular,
// progress records can embed whole transcripts and tool results. Materializing
// those trees dominates both decode time and the decoded-file cache budget.
// Validate the entire record first, including fields we will not retain.
func decodeClaudeLine(raw []byte) (obj, error) {
	raw = bytes.Trim(raw, " \t\r\n")
	if !json.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
		var v obj
		err := json.Unmarshal(raw, &v)
		return v, err
	}
	v := obj{}
	var message []byte
	var decodeErr error
	decode := func(raw []byte) any {
		var value any
		if err := json.Unmarshal(trimLongStrings(raw), &value); err != nil {
			decodeErr = err
		}
		return value
	}
	overflow := visitJSONFields(raw, func(key string, value []byte) {
		switch key {
		case "type", "sessionId", "session_id", "timestamp", "cwd", "aiTitle", "customTitle", "entrypoint", "effort", "requestId":
			v[key] = decode(value)
		case "message":
			message = value
		}
	})
	if overflow {
		// Map decoding reports float64 overflow even in ignored metadata.
		var full obj
		err := json.Unmarshal(raw, &full)
		return full, err
	}
	if len(message) > 0 && message[0] == '{' {
		m := obj{}
		var content []byte
		visitJSONFields(message, func(key string, value []byte) {
			switch key {
			case "role", "model", "id", "usage":
				m[key] = decode(value)
			case "content":
				content = value
			}
		})
		if str(v["type"]) == "user" || str(m["role"]) == "user" {
			m["content"] = contentTitlePrefix(content, decode)
		}
		v["message"] = m
	}
	return v, decodeErr
}

// Keep a title input, not compact(title): compact truncation can end in a
// space, and compacting twice would drop that space and change the title.
// Only string content or a block's top-level text contributes a title. Tool
// results, image payloads and nested messages can be skipped without decoding.
func contentTitlePrefix(raw []byte, decode func([]byte) any) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		return titlePrefix(str(decode(raw)))
	}
	if raw[0] != '[' {
		return ""
	}
	for i := skipJSONSpace(raw, 1); raw[i] != ']'; {
		end, _ := jsonValueEnd(raw, i)
		var value []byte
		if raw[i] == '{' {
			visitJSONFields(raw[i:end], func(key string, raw []byte) {
				if key == "text" {
					value = raw
				}
			})
		}
		if len(value) != 0 {
			if s := str(decode(value)); strings.TrimSpace(s) != "" {
				return titlePrefix(s)
			}
		}
		i = skipJSONSpace(raw, end)
		if raw[i] == ',' {
			i = skipJSONSpace(raw, i+1)
		}
	}
	return ""
}

// These scanners accept only JSON already validated by encoding/json. Slices
// are borrowed for this call only; every retained value is decoded into owned
// storage. Exact key spelling and last duplicate-key wins match map decoding.
func visitJSONFields(raw []byte, visit func(string, []byte)) (overflow bool) {
	for i := skipJSONSpace(raw, 1); raw[i] != '}'; {
		end := jsonStringEnd(raw, i)
		var key string
		if bytes.IndexByte(raw[i+1:end-1], '\\') < 0 {
			key = string(raw[i+1 : end-1])
		} else {
			_ = json.Unmarshal(raw[i:end], &key)
		}
		i = skipJSONSpace(raw, end)
		i = skipJSONSpace(raw, i+1) // colon
		var largeNumber bool
		end, largeNumber = jsonValueEnd(raw, i)
		overflow = overflow || largeNumber
		visit(key, raw[i:end])
		i = skipJSONSpace(raw, end)
		if raw[i] == ',' {
			i = skipJSONSpace(raw, i+1)
		}
	}
	return overflow
}

func skipJSONSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\n' || raw[i] == '\r' || raw[i] == '\t') {
		i++
	}
	return i
}

func jsonStringEnd(raw []byte, i int) int {
	for i++; ; {
		quote := i + bytes.IndexByte(raw[i:], '"')
		backslashes := 0
		for j := quote - 1; j >= i && raw[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return quote + 1
		}
		i = quote + 1
	}
}

func jsonValueEnd(raw []byte, i int) (end int, overflow bool) {
	if raw[i] == '"' {
		return jsonStringEnd(raw, i), false
	}
	if raw[i] != '{' && raw[i] != '[' {
		start := i
		for i < len(raw) && raw[i] != ',' && raw[i] != '}' && raw[i] != ']' && raw[i] > ' ' {
			i++
		}
		token := raw[start:i]
		if (token[0] == '-' || token[0] >= '0' && token[0] <= '9') && (len(token) > 308 || bytes.ContainsAny(token, "eE")) {
			_, err := strconv.ParseFloat(string(token), 64)
			return i, err != nil
		}
		return i, false
	}
	depth := 0
	for {
		switch raw[i] {
		case '"':
			i = jsonStringEnd(raw, i)
			continue
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, overflow
			}
		default:
			if raw[i] == '-' || raw[i] >= '0' && raw[i] <= '9' {
				var largeNumber bool
				i, largeNumber = jsonValueEnd(raw, i)
				overflow = overflow || largeNumber
				continue
			}
		}
		i++
	}
}
