package ext

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type tomlTable struct {
	header      string
	name        string // dotted name inside mcp_servers, e.g. "github" or "github.env"
	start, end  int    // byte range of the table, including header, excluding following table
	headerStart int
}

var errTOMLMultiline = errors.New("multiline TOML requires a preserving TOML parser")

func scanTOMLTables(s string) ([]tomlTable, error) {
	lines := strings.SplitAfter(s, "\n")
	offset := 0
	var tables []tomlTable
	var cur *tomlTable
	inML, depth := 0, 0
	finish := func(end int) {
		if cur == nil {
			return
		}
		cur.end = trimTOMLBlockEnd(s, cur.start, end)
		tables = append(tables, *cur)
		cur = nil
	}
	for _, line := range lines {
		// A line inside a multiline string or an open array value is never a header.
		if inML == 0 && depth == 0 {
			trim := strings.TrimSpace(line)
			if header, isArray, ok := tomlHeaderLine(trim); ok {
				finish(offset)
				if isArray {
					header = "[[" + header + "]]"
				}
				cur = &tomlTable{header: header, name: header, start: offset, headerStart: offset}
				inML, _ = tomlLineState(inML, 0, line)
				offset += len(line)
				continue
			}
		}
		inML, depth = tomlLineState(inML, depth, line)
		offset += len(line)
	}
	finish(len(s))
	return tables, nil
}

// tomlLineState advances the multiline-string state and the open [ / { depth
// of a value across one line, ignoring brackets inside strings and comments.
func tomlLineState(state, depth int, line string) (int, int) {
	i := 0
	for i < len(line) {
		if state == 1 {
			j := strings.Index(line[i:], `"""`)
			if j < 0 {
				return 1, depth
			}
			i += j + 3
			state = 0
			continue
		}
		if state == 2 {
			j := strings.Index(line[i:], "'''")
			if j < 0 {
				return 2, depth
			}
			i += j + 3
			state = 0
			continue
		}
		switch {
		case line[i] == '#':
			return 0, depth
		case strings.HasPrefix(line[i:], `"""`):
			state, i = 1, i+3
		case strings.HasPrefix(line[i:], "'''"):
			state, i = 2, i+3
		case line[i] == '"':
			i = skipTOMLQuoted(line, i, '"', true)
		case line[i] == '\'':
			i = skipTOMLQuoted(line, i, '\'', false)
		case line[i] == '[' || line[i] == '{':
			depth++
			i++
		case line[i] == ']' || line[i] == '}':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return state, depth
}

func skipTOMLQuoted(s string, i int, quote byte, escapes bool) int {
	i++
	for i < len(s) {
		if s[i] == '\n' {
			return i
		}
		if escapes && s[i] == '\\' {
			i++
			if i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if s[i] == quote {
			return i + 1
		}
		i++
	}
	return len(s)
}

// tomlBlockOpenMultiline reports a string or array left open at the block end,
// which would have swallowed the following tables.
func tomlBlockOpenMultiline(chunk string) bool {
	state, depth := 0, 0
	for _, line := range strings.SplitAfter(chunk, "\n") {
		state, depth = tomlLineState(state, depth, line)
	}
	return state != 0 || depth != 0
}

func trimTOMLBlockEnd(s string, start, end int) int {
	if end <= start {
		return end
	}
	body := s[start:end]
	cut := len(body)
	for cut > 0 {
		prev := strings.LastIndex(body[:cut], "\n")
		line := body
		if prev >= 0 {
			line = body[prev+1 : cut]
		}
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			if prev < 0 {
				return start
			}
			cut = prev
			continue
		}
		break
	}
	if cut < len(body) && body[cut] != '\n' {
		if nl := strings.IndexByte(body[cut:], '\n'); nl >= 0 {
			cut += nl + 1
		}
	} else if cut < len(body) && body[cut] == '\n' {
		cut++
	}
	return start + cut
}

func mcpTOMLName(header string) (name, rest string, ok bool) {
	const prefix = "mcp_servers."
	h := header
	if strings.HasPrefix(h, "mcp_servers.") {
		h = strings.TrimPrefix(h, prefix)
	} else {
		return "", "", false
	}
	name, rest, _ = splitTOMLDotted(h)
	return name, rest, name != ""
}

func splitTOMLDotted(h string) (head, rest string, err error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", "", errors.New("empty TOML name")
	}
	if h[0] == '"' || h[0] == '\'' {
		v, n, e := unquoteTOML(h)
		if e != nil {
			return "", "", e
		}
		rest = strings.TrimSpace(h[n:])
		rest = strings.TrimPrefix(rest, ".")
		rest = strings.TrimSpace(rest)
		return v, rest, nil
	}
	i := 0
	for i < len(h) && (h[i] >= 'A' && h[i] <= 'Z' || h[i] >= 'a' && h[i] <= 'z' || h[i] >= '0' && h[i] <= '9' || h[i] == '_' || h[i] == '-') {
		i++
	}
	if i == 0 {
		return "", "", errors.New("invalid TOML name")
	}
	rest = strings.TrimSpace(h[i:])
	rest = strings.TrimPrefix(rest, ".")
	rest = strings.TrimSpace(rest)
	return h[:i], rest, nil
}

func unquoteTOML(s string) (string, int, error) {
	if s == "" {
		return "", 0, errors.New("expected quoted TOML string")
	}
	if s[0] == '\'' {
		i := 1
		for i < len(s) && s[i] != '\'' {
			if s[i] == '\n' {
				return "", 0, errTOMLMultiline
			}
			i++
		}
		if i >= len(s) {
			return "", 0, errors.New("unterminated TOML string")
		}
		return s[1:i], i + 1, nil
	}
	if s[0] != '"' {
		return "", 0, errors.New("expected quoted TOML string")
	}
	i := 1
	var b strings.Builder
	for i < len(s) {
		c := s[i]
		if c == '"' {
			return b.String(), i + 1, nil
		}
		if c == '\n' {
			return "", 0, errTOMLMultiline
		}
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", 0, errors.New("unterminated TOML escape")
		}
		i++
		switch s[i] {
		case '"', '\\':
			b.WriteByte(s[i])
			i++
		case 'b':
			b.WriteByte('\b')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 'f':
			b.WriteByte('\f')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 'u':
			r, n, err := parseTOMLHexRune(s[i+1:], 4)
			if err != nil {
				return "", 0, err
			}
			b.WriteRune(r)
			i += 1 + n
		case 'U':
			r, n, err := parseTOMLHexRune(s[i+1:], 8)
			if err != nil {
				return "", 0, err
			}
			b.WriteRune(r)
			i += 1 + n
		default:
			return "", 0, fmt.Errorf("invalid TOML escape \\%c", s[i])
		}
	}
	return "", 0, errors.New("unterminated TOML string")
}

func parseTOMLHexRune(s string, n int) (rune, int, error) {
	if len(s) < n {
		return 0, 0, errors.New("short TOML unicode escape")
	}
	var v rune
	for i := 0; i < n; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, 0, errors.New("invalid TOML unicode escape")
		}
	}
	if !utf8.ValidRune(v) {
		return 0, 0, errors.New("invalid TOML unicode rune")
	}
	return v, n, nil
}

func tomlHeaderLine(trim string) (header string, isArray, ok bool) {
	trim = stripTOMLLineComment(trim)
	trim = strings.TrimSpace(trim)
	if !strings.HasPrefix(trim, "[") {
		return "", false, false
	}
	if strings.HasPrefix(trim, "[[") {
		inner := trim[2:]
		if strings.HasSuffix(inner, "]]") {
			inner = inner[:len(inner)-2]
		}
		return strings.TrimSpace(inner), true, true
	}
	if !strings.HasSuffix(trim, "]") {
		return "", false, true
	}
	return strings.TrimSpace(trim[1 : len(trim)-1]), false, true
}

func stripTOMLLineComment(line string) string {
	inStr := false
	esc := false
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			if quote == '\'' {
				if c == '\'' {
					inStr = false
				}
				continue
			}
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = true
			quote = c
			continue
		}
		if c == '#' {
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

func tomlBare(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlHeaderName(name string) string {
	if tomlBare(name) {
		return name
	}
	return tomlQuote(name)
}

func mcpTOMLBlocks(s, name string) ([]tomlTable, error) {
	tables, err := scanTOMLTables(s)
	if err != nil {
		return nil, err
	}
	var out []tomlTable
	for _, t := range tables {
		n, rest, ok := mcpTOMLName(t.header)
		if !ok || n != name {
			continue
		}
		t.name = rest
		out = append(out, t)
	}
	return out, nil
}

func deleteTOMLServer(s, name string) (string, error) {
	if err := refuseInlineTOML(s, name); err != nil {
		return "", err
	}
	blocks, err := mcpTOMLBlocks(s, name)
	if err != nil {
		return "", err
	}
	if len(blocks) == 0 {
		return s, nil
	}
	for _, block := range blocks {
		if tomlBlockOpenMultiline(s[block.start:block.end]) {
			return "", errTOMLMultiline
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		a, b := blocks[i].start, blocks[i].end
		s = s[:a] + s[b:]
	}
	return s, nil
}

func upsertTOMLServer(s string, m MCP) (string, error) {
	if err := refuseInlineTOML(s, m.Name); err != nil {
		return "", err
	}
	blocks, err := mcpTOMLBlocks(s, m.Name)
	if err != nil {
		return "", err
	}
	for _, block := range blocks {
		chunk := s[block.start:block.end]
		if strings.Contains(chunk, `"""`) || strings.Contains(chunk, "'''") {
			return "", errTOMLMultiline
		}
	}
	if len(blocks) == 0 {
		return appendTOMLServer(s, m)
	}
	return patchTOMLServer(s, m, blocks)
}

func appendTOMLServer(s string, m MCP) (string, error) {
	block := encodeTOMLServer(m)
	if strings.TrimSpace(s) == "" {
		return block, nil
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if !strings.HasSuffix(s, "\n\n") {
		s += "\n"
	}
	return s + block, nil
}

func patchTOMLServer(s string, m MCP, blocks []tomlTable) (string, error) {
	var main, envBlk, hdrBlk *tomlTable
	for i := range blocks {
		b := &blocks[i]
		switch b.name {
		case "":
			main = b
		case "env":
			envBlk = b
		case "http_headers", "headers":
			hdrBlk = b
		}
	}
	if main == nil {
		edited, err := deleteTOMLServer(s, m.Name)
		if err != nil {
			return "", err
		}
		return appendTOMLServer(edited, m)
	}
	bodyStart, bodyEnd := tomlBodyRange(s, *main)
	newBody, hadInlineEnv, hadInlineHdr, err := patchTOMLMainBody(s[bodyStart:bodyEnd], m)
	if err != nil {
		return "", err
	}
	type edit struct {
		start, end int
		repl       string
	}
	edits := []edit{{bodyStart, bodyEnd, newBody}}
	existing := func(blk *tomlTable, vals map[string]string, drop bool) {
		if blk == nil {
			return
		}
		if drop || len(vals) == 0 {
			edits = append(edits, edit{blk.start, blk.end, ""})
			return
		}
		st, en := tomlBodyRange(s, *blk)
		edits = append(edits, edit{st, en, encodeTOMLMapBody(vals)})
	}
	existing(envBlk, m.Env, hadInlineEnv)
	existing(hdrBlk, m.Headers, hadInlineHdr)
	var insert strings.Builder
	if !hadInlineEnv && envBlk == nil && len(m.Env) > 0 {
		fmt.Fprintf(&insert, "\n[mcp_servers.%s.env]\n%s", tomlHeaderName(m.Name), encodeTOMLMapBody(m.Env))
	}
	if !hadInlineHdr && hdrBlk == nil && len(m.Headers) > 0 {
		fmt.Fprintf(&insert, "\n[mcp_servers.%s.http_headers]\n%s", tomlHeaderName(m.Name), encodeTOMLMapBody(m.Headers))
	}
	if insert.Len() > 0 {
		edits = append(edits, edit{main.end, main.end, insert.String()})
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start > edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	for _, e := range edits {
		s, err = splice(s, e.start, e.end, e.repl)
		if err != nil {
			return "", err
		}
	}
	return s, nil
}

func splice(s string, start, end int, repl string) (string, error) {
	if start < 0 || end < start || end > len(s) {
		return "", errors.New("invalid TOML splice")
	}
	return s[:start] + repl + s[end:], nil
}

func tomlBodyRange(s string, t tomlTable) (int, int) {
	body := s[t.headerStart:t.end]
	nl := strings.IndexByte(body, '\n')
	if nl < 0 {
		return t.end, t.end
	}
	return t.headerStart + nl + 1, t.end
}

func encodeTOMLMapBody(vals map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(vals) {
		fmt.Fprintf(&b, "%s = %s\n", tomlHeaderName(k), tomlQuote(vals[k]))
	}
	return b.String()
}

func patchTOMLMainBody(body string, m MCP) (string, bool, bool, error) {
	spans, err := scanTOMLAssigns(body)
	if err != nil {
		return "", false, false, err
	}
	replace := map[string]string{}
	drop := map[string]bool{"transport": true}
	if m.Command != "" {
		replace["command"] = "command = " + tomlQuote(m.Command) + "\n"
		if len(m.Args) > 0 {
			replace["args"] = "args = " + encodeTOMLArray(m.Args) + "\n"
		} else {
			drop["args"] = true
		}
	} else {
		drop["command"] = true
		drop["args"] = true
	}
	if m.URL != "" {
		replace["url"] = "url = " + tomlQuote(m.URL) + "\n"
	} else {
		drop["url"] = true
	}
	if m.Disabled {
		replace["enabled"] = "enabled = false\n"
	} else {
		drop["enabled"] = true
	}
	hadInlineEnv, hadInlineHdr := false, false
	for _, sp := range spans {
		if sp.key == "env" && sp.kind == "inline" {
			hadInlineEnv = true
			if len(m.Env) > 0 {
				replace["env"] = "env = " + encodeTOMLInline(m.Env) + "\n"
			} else {
				drop["env"] = true
			}
		}
		if (sp.key == "http_headers" || sp.key == "headers") && sp.kind == "inline" {
			hadInlineHdr = true
			if len(m.Headers) > 0 {
				replace[sp.key] = sp.key + " = " + encodeTOMLInline(m.Headers) + "\n"
			} else {
				drop[sp.key] = true
			}
		}
	}
	var b strings.Builder
	seen := map[string]bool{}
	cursor := 0
	for _, sp := range spans {
		if cursor < sp.start {
			b.WriteString(body[cursor:sp.start])
		}
		cursor = sp.end
		if drop[sp.key] {
			continue
		}
		if repl, ok := replace[sp.key]; ok {
			if seen[sp.key] {
				continue
			}
			seen[sp.key] = true
			b.WriteString(repl)
			continue
		}
		b.WriteString(body[sp.start:sp.end])
	}
	if cursor < len(body) {
		b.WriteString(body[cursor:])
	}
	if !strings.HasSuffix(b.String(), "\n") && b.Len() > 0 {
		b.WriteByte('\n')
	}
	for _, key := range []string{"command", "args", "url", "enabled", "env", "http_headers", "headers"} {
		if seen[key] {
			continue
		}
		repl, ok := replace[key]
		if !ok {
			continue
		}
		b.WriteString(repl)
	}
	return b.String(), hadInlineEnv, hadInlineHdr, nil
}

func encodeTOMLArray(args []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, a := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlQuote(a))
	}
	b.WriteByte(']')
	return b.String()
}

func encodeTOMLInline(vals map[string]string) string {
	var b strings.Builder
	b.WriteString("{ ")
	keys := sortedKeys(vals)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s = %s", tomlHeaderName(k), tomlQuote(vals[k]))
	}
	b.WriteString(" }")
	return b.String()
}

func encodeTOMLServer(m MCP) string {
	hdr := tomlHeaderName(m.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", hdr)
	if m.Command != "" {
		fmt.Fprintf(&b, "command = %s\n", tomlQuote(m.Command))
		if len(m.Args) > 0 {
			fmt.Fprintf(&b, "args = %s\n", encodeTOMLArray(m.Args))
		}
	}
	if m.URL != "" {
		fmt.Fprintf(&b, "url = %s\n", tomlQuote(m.URL))
	}
	if m.Disabled {
		b.WriteString("enabled = false\n")
	}
	if len(m.Env) > 0 {
		fmt.Fprintf(&b, "\n[mcp_servers.%s.env]\n", hdr)
		b.WriteString(encodeTOMLMapBody(m.Env))
	}
	if len(m.Headers) > 0 {
		fmt.Fprintf(&b, "\n[mcp_servers.%s.http_headers]\n", hdr)
		b.WriteString(encodeTOMLMapBody(m.Headers))
	}
	return b.String()
}

type tomlAssign struct {
	key        string
	start, end int
	kind       string // scalar, array, inline
	scalar     string
	array      []string
	table      map[string]string
}

func scanTOMLAssigns(body string) ([]tomlAssign, error) {
	var out []tomlAssign
	i := 0
	for i < len(body) {
		i = skipTOMLSpaceAndComments(body, i)
		if i >= len(body) {
			break
		}
		if body[i] == '[' {
			break
		}
		keyStart := i
		key, n, err := parseTOMLKey(body[i:])
		if err != nil {
			return nil, err
		}
		i += n
		i = skipTOMLHoriz(body, i)
		if i >= len(body) || body[i] != '=' {
			return nil, errors.New("invalid TOML assignment")
		}
		i++
		i = skipTOMLHoriz(body, i)
		if i >= len(body) {
			return nil, errors.New("invalid TOML assignment")
		}
		sp := tomlAssign{key: key, start: keyStart}
		switch body[i] {
		case '"', '\'':
			if strings.HasPrefix(body[i:], `"""`) || strings.HasPrefix(body[i:], "'''") {
				return nil, errTOMLMultiline
			}
			v, n, err := unquoteTOML(body[i:])
			if err != nil {
				return nil, err
			}
			sp.kind = "scalar"
			sp.scalar = v
			i += n
		case '[':
			arr, end, err := parseTOMLArray(body, i)
			if err != nil {
				return nil, err
			}
			sp.kind = "array"
			sp.array = arr
			i = end
		case '{':
			tab, end, err := parseTOMLInlineTable(body, i)
			if err != nil {
				return nil, err
			}
			sp.kind = "inline"
			sp.table = tab
			i = end
		default:
			if strings.HasPrefix(body[i:], `"""`) || strings.HasPrefix(body[i:], "'''") {
				return nil, errTOMLMultiline
			}
			end := i
			for end < len(body) && body[end] != '\n' && body[end] != '#' {
				end++
			}
			sp.kind = "scalar"
			sp.scalar = strings.TrimSpace(body[i:end])
			i = end
		}
		i = skipTOMLHoriz(body, i)
		if i < len(body) && body[i] == '#' {
			for i < len(body) && body[i] != '\n' {
				i++
			}
		}
		if i < len(body) && body[i] == '\n' {
			i++
		}
		sp.end = i
		out = append(out, sp)
	}
	return out, nil
}

func parseTOMLKey(s string) (string, int, error) {
	nspace := 0
	for nspace < len(s) && (s[nspace] == ' ' || s[nspace] == '\t') {
		nspace++
	}
	s = s[nspace:]
	if s == "" {
		return "", 0, errors.New("expected TOML key")
	}
	if s[0] == '"' || s[0] == '\'' {
		v, n, err := unquoteTOML(s)
		return v, nspace + n, err
	}
	i := 0
	for i < len(s) && (s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= '0' && s[i] <= '9' || s[i] == '_' || s[i] == '-') {
		i++
	}
	if i == 0 {
		return "", 0, errors.New("invalid TOML key")
	}
	return s[:i], nspace + i, nil
}

func skipTOMLHoriz(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func skipTOMLSpaceAndComments(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		case '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		default:
			return i
		}
	}
	return i
}

func parseTOMLArray(s string, i int) ([]string, int, error) {
	if i >= len(s) || s[i] != '[' {
		return nil, i, errors.New("expected TOML array")
	}
	i++
	var out []string
	for {
		i = skipTOMLSpaceAndComments(s, i)
		if i >= len(s) {
			return nil, i, errors.New("unterminated TOML array")
		}
		if s[i] == ']' {
			return out, i + 1, nil
		}
		if s[i] == ',' {
			i++
			continue
		}
		if s[i] != '"' && s[i] != '\'' {
			return nil, i, errors.New("TOML array values must be strings")
		}
		v, n, err := unquoteTOML(s[i:])
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
		i += n
	}
}

func parseTOMLInlineTable(s string, i int) (map[string]string, int, error) {
	if i >= len(s) || s[i] != '{' {
		return nil, i, errors.New("expected inline table")
	}
	i++
	out := map[string]string{}
	for {
		i = skipTOMLSpaceAndComments(s, i)
		if i >= len(s) {
			return nil, i, errors.New("unterminated inline table")
		}
		if s[i] == '}' {
			return out, i + 1, nil
		}
		if s[i] == ',' {
			i++
			continue
		}
		key, n, err := parseTOMLKey(s[i:])
		if err != nil {
			return nil, i, err
		}
		i += n
		i = skipTOMLHoriz(s, i)
		if i >= len(s) || s[i] != '=' {
			return nil, i, errors.New("expected = in inline table")
		}
		i++
		i = skipTOMLHoriz(s, i)
		if i >= len(s) {
			return nil, i, errors.New("unterminated inline table")
		}
		if s[i] != '"' && s[i] != '\'' {
			return nil, i, errors.New("inline table values must be strings")
		}
		v, n, err := unquoteTOML(s[i:])
		if err != nil {
			return nil, i, err
		}
		out[key] = v
		i += n
	}
}

func parseTOMLServer(s, name string) (MCP, bool, error) {
	blocks, err := mcpTOMLBlocks(s, name)
	if err != nil {
		return MCP{}, false, err
	}
	if len(blocks) == 0 {
		return MCP{}, false, nil
	}
	m := MCP{Name: name}
	parsed := false
	for _, block := range blocks {
		bodyStart, bodyEnd := tomlBodyRange(s, block)
		vals, err := parseTOMLAssignments(s[bodyStart:bodyEnd])
		if err != nil {
			return MCP{}, false, err
		}
		parsed = true
		if block.name == "env" {
			m.Env = mergeStr(m.Env, vals.scalars)
			continue
		}
		if block.name == "http_headers" || block.name == "headers" {
			m.Headers = mergeStr(m.Headers, vals.scalars)
			continue
		}
		if v, ok := vals.scalars["command"]; ok {
			m.Command = v
		}
		if v, ok := vals.arrays["args"]; ok {
			m.Args = v
		}
		if v, ok := vals.scalars["url"]; ok {
			m.URL = v
		}
		if v, ok := vals.scalars["transport"]; ok {
			m.Transport = v
		}
		if vals.scalars["enabled"] == "false" {
			m.Disabled = true
		}
		if t, ok := vals.tables["env"]; ok {
			m.Env = mergeStr(m.Env, t)
		}
		if t, ok := vals.tables["http_headers"]; ok {
			m.Headers = mergeStr(m.Headers, t)
		}
		if t, ok := vals.tables["headers"]; ok {
			m.Headers = mergeStr(m.Headers, t)
		}
	}
	if !parsed {
		return MCP{}, false, nil
	}
	return m, true, nil
}

type tomlVals struct {
	scalars map[string]string
	arrays  map[string][]string
	tables  map[string]map[string]string
}

func parseTOMLAssignments(body string) (tomlVals, error) {
	out := tomlVals{scalars: map[string]string{}, arrays: map[string][]string{}, tables: map[string]map[string]string{}}
	spans, err := scanTOMLAssigns(body)
	if err != nil {
		return out, err
	}
	for _, sp := range spans {
		switch sp.kind {
		case "array":
			out.arrays[sp.key] = sp.array
		case "inline":
			out.tables[sp.key] = sp.table
		default:
			out.scalars[sp.key] = sp.scalar
		}
	}
	return out, nil
}

func refuseInlineTOML(s, name string) error {
	keys, err := inlineMCPKeys(s)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == name {
			return fmt.Errorf("%q is an inline [mcp_servers] entry; convert it to [mcp_servers.%s] first", name, name)
		}
	}
	return refuseRootMCP(s, name)
}

func refuseRootMCP(s, name string) error {
	tables, err := scanTOMLTables(s)
	if err != nil {
		return err
	}
	end := len(s)
	if len(tables) > 0 {
		end = tables[0].start
	}
	for _, line := range strings.Split(s[:end], "\n") {
		trim := stripTOMLLineComment(strings.TrimSpace(line))
		key, _, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		head, _, err := splitTOMLDotted(key)
		if err != nil {
			continue
		}
		if head == "mcp_servers" {
			return fmt.Errorf("%q is a root mcp_servers assignment; convert it to [mcp_servers.%s] first", name, name)
		}
	}
	return nil
}

func inlineMCPKeys(s string) ([]string, error) {
	tables, err := scanTOMLTables(s)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range tables {
		if t.header != "mcp_servers" {
			continue
		}
		bodyStart, bodyEnd := tomlBodyRange(s, t)
		vals, err := parseTOMLAssignments(s[bodyStart:bodyEnd])
		if err != nil {
			return nil, err
		}
		for k := range vals.scalars {
			names = append(names, k)
		}
		for k := range vals.arrays {
			names = append(names, k)
		}
		for k := range vals.tables {
			names = append(names, k)
		}
	}
	return names, nil
}

func listTOMLServers(s string) ([]string, error) {
	tables, err := scanTOMLTables(s)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, t := range tables {
		n, _, ok := mcpTOMLName(t.header)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	return names, nil
}

func mergeStr(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
