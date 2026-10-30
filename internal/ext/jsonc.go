package ext

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// jsoncMember is one object key/value in the original bytes.
type jsoncMember struct {
	key                    string
	keyStart, keyEnd       int
	valueStart, valueEnd   int
	memberStart, memberEnd int
}

var errJSONC = errors.New("settings must be an object")

func skipJSONC(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		case '/':
			if i+1 >= len(s) {
				return i
			}
			switch s[i+1] {
			case '/':
				i += 2
				for i < len(s) && s[i] != '\n' {
					i++
				}
			case '*':
				i += 2
				for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
					i++
				}
				if i+1 < len(s) {
					i += 2
				}
			default:
				return i
			}
		default:
			return i
		}
	}
	return i
}

func scanJSONCString(s string, i int) (int, string, error) {
	if i >= len(s) || s[i] != '"' {
		return i, "", errors.New("expected string")
	}
	start := i
	i++
	for i < len(s) {
		switch s[i] {
		case '\\':
			i++
			if i < len(s) {
				i++
			}
		case '"':
			raw := s[start : i+1]
			var v string
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				return i, "", err
			}
			return i + 1, v, nil
		default:
			i++
		}
	}
	return i, "", errors.New("unterminated string")
}

func scanJSONCValue(s string, i int) (int, error) {
	i = skipJSONC(s, i)
	if i >= len(s) {
		return i, errors.New("expected value")
	}
	switch s[i] {
	case '"':
		end, _, err := scanJSONCString(s, i)
		return end, err
	case '{':
		_, end, err := jsoncObjectMembers(s, i)
		return end, err
	case '[':
		return scanJSONCArray(s, i)
	case 't':
		if strings.HasPrefix(s[i:], "true") {
			return i + 4, nil
		}
	case 'f':
		if strings.HasPrefix(s[i:], "false") {
			return i + 5, nil
		}
	case 'n':
		if strings.HasPrefix(s[i:], "null") {
			return i + 4, nil
		}
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		i++
		for i < len(s) {
			c := s[i]
			if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				i++
				continue
			}
			break
		}
		return i, nil
	}
	return i, errors.New("invalid JSONC value")
}

func scanJSONCArray(s string, i int) (int, error) {
	if i >= len(s) || s[i] != '[' {
		return i, errors.New("expected array")
	}
	i++
	for {
		i = skipJSONC(s, i)
		if i >= len(s) {
			return i, errors.New("unterminated array")
		}
		if s[i] == ']' {
			return i + 1, nil
		}
		end, err := scanJSONCValue(s, i)
		if err != nil {
			return i, err
		}
		i = skipJSONC(s, end)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == ']' {
			return i + 1, nil
		}
		return i, errors.New("expected comma or array end")
	}
}

func jsoncObjectMembers(s string, open int) ([]jsoncMember, int, error) {
	if open >= len(s) || s[open] != '{' {
		return nil, open, errJSONC
	}
	i := open + 1
	var out []jsoncMember
	seen := map[string]bool{}
	for {
		i = skipJSONC(s, i)
		if i >= len(s) {
			return nil, i, errors.New("unterminated object")
		}
		if s[i] == '}' {
			return out, i + 1, nil
		}
		if s[i] == ',' {
			i++
			continue
		}
		if s[i] != '"' {
			return nil, i, errors.New("expected object key")
		}
		memberStart := i
		keyEnd, key, err := scanJSONCString(s, i)
		if err != nil {
			return nil, i, err
		}
		if seen[key] {
			return nil, i, errors.New("duplicate settings key")
		}
		seen[key] = true
		i = skipJSONC(s, keyEnd)
		if i >= len(s) || s[i] != ':' {
			return nil, i, errors.New("expected colon")
		}
		i++
		i = skipJSONC(s, i)
		valueStart := i
		valueEnd, err := scanJSONCValue(s, i)
		if err != nil {
			return nil, i, err
		}
		out = append(out, jsoncMember{key: key, keyStart: memberStart, keyEnd: keyEnd, valueStart: valueStart, valueEnd: valueEnd, memberStart: memberStart, memberEnd: valueEnd})
		i = skipJSONC(s, valueEnd)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == '}' {
			return out, i + 1, nil
		}
		return nil, i, errors.New("expected comma or object end")
	}
}

func jsoncRootObject(s string) ([]jsoncMember, int, int, error) {
	i := 0
	if strings.HasPrefix(s, "\ufeff") {
		i += len("\ufeff")
	}
	i = skipJSONC(s, i)
	if i >= len(s) {
		return nil, 0, 0, errJSONC
	}
	if s[i] != '{' {
		return nil, 0, 0, errJSONC
	}
	members, end, err := jsoncObjectMembers(s, i)
	if err != nil {
		return nil, 0, 0, err
	}
	if skipJSONC(s, end) != len(s) {
		// Trailing comments and space are fine; other tokens are not.
		j := skipJSONC(s, end)
		if j != len(s) {
			return nil, 0, 0, errors.New("trailing JSONC content")
		}
	}
	return members, i, end, nil
}

func jsoncPathObject(s string, path []string) (members []jsoncMember, open, close int, err error) {
	members, open, close, err = jsoncRootObject(s)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, key := range path {
		found := false
		for _, m := range members {
			if m.key != key {
				continue
			}
			found = true
			open = m.valueStart
			if open >= len(s) || s[open] != '{' {
				return nil, 0, 0, errors.New(key + " must be an object")
			}
			members, close, err = jsoncObjectMembers(s, open)
			if err != nil {
				return nil, 0, 0, err
			}
			break
		}
		if !found {
			return nil, 0, 0, errJSONCMissing
		}
	}
	return members, open, close, nil
}

var errJSONCMissing = errors.New("missing object path")

func jsoncIndentOf(s string, pos int) string {
	line := pos
	for line > 0 && s[line-1] != '\n' {
		line--
	}
	i := line
	for i < pos && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[line:i]
}

func jsoncMemberText(indent, key, raw string) string {
	return indent + strconv.Quote(key) + ": " + strings.TrimSpace(raw)
}

func upsertJSONC(s, key string, path []string, raw string) (string, error) {
	if strings.TrimSpace(s) == "" {
		body := jsoncMemberText("    ", key, raw)
		inner := jsoncMemberText("  ", path[0], "{\n"+body+"\n  }")
		if len(path) != 1 {
			return "", errors.New("unsupported JSONC path")
		}
		return "{\n" + inner + "\n}\n", nil
	}
	members, open, close, err := jsoncPathObject(s, path)
	if err == errJSONCMissing {
		root, _, rootClose, e := jsoncRootObject(s)
		if e != nil {
			return "", e
		}
		if len(path) != 1 {
			return "", errors.New("unsupported JSONC path")
		}
		indent := "  "
		if len(root) > 0 {
			indent = jsoncIndentOf(s, root[0].memberStart)
		}
		inner := jsoncMemberText(indent+"  ", key, raw)
		fragment := jsoncMemberText(indent, path[0], "{\n"+inner+"\n"+indent+"}")
		return jsoncInsertMember(s, root, rootClose, fragment), nil
	}
	if err != nil {
		return "", err
	}
	indent := "    "
	if len(members) > 0 {
		indent = jsoncIndentOf(s, members[0].memberStart)
	} else {
		indent = jsoncIndentOf(s, open) + "  "
	}
	fragment := jsoncMemberText(indent, key, raw)
	for _, m := range members {
		if m.key != key {
			continue
		}
		return s[:m.valueStart] + strings.TrimSpace(raw) + s[m.valueEnd:], nil
	}
	return jsoncInsertMember(s, members, close, fragment), nil
}

func jsoncLastComma(s string) int {
	last := -1
	i := 0
	for i < len(s) {
		i = skipJSONC(s, i)
		if i >= len(s) {
			break
		}
		if s[i] == '"' {
			end, _, err := scanJSONCString(s, i)
			if err != nil {
				break
			}
			i = end
			continue
		}
		if s[i] == ',' {
			last = i
		}
		i++
	}
	return last
}

func jsoncGapHasComma(s string) bool {
	i := 0
	for i < len(s) {
		i = skipJSONC(s, i)
		if i >= len(s) {
			return false
		}
		if s[i] == ',' {
			return true
		}
		return false
	}
	return false
}

func jsoncInsertMember(s string, members []jsoncMember, close int, fragment string) string {
	insertAt := close - 1
	if insertAt < 0 {
		insertAt = 0
	}
	closeIndent := jsoncIndentOf(s, insertAt)
	if len(members) == 0 {
		prefix := ""
		if insertAt > 0 && s[insertAt-1] != '\n' {
			prefix = "\n"
		}
		suffix := ""
		if insertAt < len(s) && s[insertAt] == '}' && (insertAt == 0 || s[insertAt-1] != '\n') {
			suffix = "\n" + closeIndent
		}
		return s[:insertAt] + prefix + fragment + suffix + s[insertAt:]
	}
	last := members[len(members)-1]
	after := last.memberEnd
	gap := ""
	if after < insertAt {
		gap = s[after:insertAt]
	}
	out := s[:after]
	if !jsoncGapHasComma(gap) {
		out += ","
	}
	out += strings.TrimRight(gap, " \t\r\n")
	out += "\n" + fragment + "\n" + closeIndent
	return out + s[insertAt:]
}

func deleteJSONC(s, key string, path []string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return s, nil
	}
	members, _, _, err := jsoncPathObject(s, path)
	if err == errJSONCMissing {
		return s, nil
	}
	if err != nil {
		return "", err
	}
	for i, m := range members {
		if m.key != key {
			continue
		}
		return jsoncRemoveMember(s, members, i), nil
	}
	return s, nil
}

func jsoncRemoveMember(s string, members []jsoncMember, i int) string {
	m := members[i]
	a, b := m.memberStart, m.memberEnd
	j := skipJSONC(s, b)
	if j < len(s) && s[j] == ',' {
		b = j + 1
		k := b
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r') {
			k++
		}
		if k < len(s) && s[k] == '\n' {
			b = k + 1
			// The whole line goes, so its indent goes too.
			line := a
			for line > 0 && (s[line-1] == ' ' || s[line-1] == '\t') {
				line--
			}
			if line == 0 || s[line-1] == '\n' {
				a = line
			}
		}
		return s[:a] + s[b:]
	}
	if i > 0 {
		a = members[i-1].memberEnd
		prev := s[a:m.memberStart]
		if comma := jsoncLastComma(prev); comma >= 0 {
			a += comma
		}
	}
	return s[:a] + s[b:]
}

func stripJSONC(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '"' {
			end, _, err := scanJSONCString(s, i)
			if err != nil {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(s[i:end])
			i = end
			continue
		}
		if s[i] == '/' && i+1 < len(s) {
			n := skipJSONC(s, i)
			if n > i {
				b.WriteByte(' ')
				i = n
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	out := b.String()
	// Drop trailing commas before } or ].
	var cleaned strings.Builder
	cleaned.Grow(len(out))
	inStr := false
	esc := false
	for i := 0; i < len(out); i++ {
		if inStr {
			cleaned.WriteByte(out[i])
			if esc {
				esc = false
				continue
			}
			if out[i] == '\\' {
				esc = true
				continue
			}
			if out[i] == '"' {
				inStr = false
			}
			continue
		}
		if out[i] == '"' {
			inStr = true
			cleaned.WriteByte(out[i])
			continue
		}
		if out[i] == ',' {
			j := i + 1
			for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				continue
			}
		}
		cleaned.WriteByte(out[i])
	}
	return cleaned.String()
}

func marshalJSONValue(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("invalid UTF-8")
	}
	return string(b), nil
}
