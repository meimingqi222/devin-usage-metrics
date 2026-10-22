package pricing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// literal reads only document data literals, never JavaScript expressions.
type literal struct {
	s string
	i int
}

func (l *literal) space() {
	for l.i < len(l.s) {
		if unicode.IsSpace(rune(l.s[l.i])) {
			l.i++
			continue
		}
		if strings.HasPrefix(l.s[l.i:], "//") {
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
			continue
		}
		break
	}
}
func (l *literal) value() (any, error) {
	l.space()
	if l.i >= len(l.s) {
		return nil, fmt.Errorf("missing literal")
	}
	c := l.s[l.i]
	l.i++
	if c == '\'' || c == '"' {
		var b strings.Builder
		for l.i < len(l.s) {
			v := l.s[l.i]
			l.i++
			if v == c {
				return b.String(), nil
			}
			if v == '\\' {
				if l.i == len(l.s) {
					break
				}
				v = l.s[l.i]
				l.i++
				switch v {
				case 'n':
					v = '\n'
				case 'r':
					v = '\r'
				case 't':
					v = '\t'
				}
			}
			b.WriteByte(v)
		}
		return nil, fmt.Errorf("unterminated string")
	}
	if c == '{' {
		m := map[string]any{}
		for {
			l.space()
			if l.i < len(l.s) && l.s[l.i] == '}' {
				l.i++
				return m, nil
			}
			var key string
			if l.i < len(l.s) && (l.s[l.i] == '\'' || l.s[l.i] == '"') {
				v, e := l.value()
				if e != nil {
					return nil, e
				}
				key = v.(string)
			} else {
				start := l.i
				for l.i < len(l.s) && (unicode.IsLetter(rune(l.s[l.i])) || unicode.IsDigit(rune(l.s[l.i])) || l.s[l.i] == '_') {
					l.i++
				}
				key = l.s[start:l.i]
			}
			l.space()
			if key == "" || l.i >= len(l.s) || l.s[l.i] != ':' {
				return nil, fmt.Errorf("invalid key")
			}
			l.i++
			v, e := l.value()
			if e != nil {
				return nil, e
			}
			m[key] = v
			l.space()
			if l.i < len(l.s) && l.s[l.i] == ',' {
				l.i++
				continue
			}
			if l.i >= len(l.s) || l.s[l.i] != '}' {
				return nil, fmt.Errorf("invalid object")
			}
		}
	}
	return nil, fmt.Errorf("unsupported document literal")
}
func documentObject(text, name string) (map[string]any, bool, error) {
	_, tail, ok := strings.Cut(text, "const "+name+" =")
	if !ok {
		return nil, false, nil
	}
	v, e := (&literal{s: tail}).value()
	if e != nil {
		return nil, true, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, true, fmt.Errorf("expected object")
	}
	return m, true, nil
}
func parsePro(b []byte) (map[string]Price, error) {
	_, tail, ok := strings.Cut(string(b), "export const modelCostData = ")
	if !ok {
		return nil, fmt.Errorf("missing modelCostData")
	}
	var rows []struct {
		Tier   string   `json:"tier"`
		Model  string   `json:"model_uid"`
		Label  string   `json:"label"`
		Input  *float64 `json:"input_cost_per_million_usd"`
		Output *float64 `json:"output_cost_per_million_usd"`
		Read   *float64 `json:"cache_read_cost_per_million_usd"`
		Write  *float64 `json:"cache_write_cost_per_million_usd"`
	}
	if e := json.NewDecoder(strings.NewReader(tail)).Decode(&rows); e != nil {
		return nil, e
	}
	out := map[string]Price{}
	for _, r := range rows {
		if r.Tier != "TEAMS_TIER_PRO" {
			continue
		}
		id := strings.ToLower(r.Model)
		if r.Input == nil || r.Output == nil || r.Read == nil || r.Write == nil || strings.TrimSpace(id) == "" || !valid(*r.Input) || !valid(*r.Output) || !valid(*r.Read) || !valid(*r.Write) {
			return nil, fmt.Errorf("invalid Pro price")
		}
		if _, ok := out[id]; ok {
			return nil, fmt.Errorf("duplicate Pro price")
		}
		out[id] = Price{Input: *r.Input, Output: *r.Output, CacheRead: *r.Read, CacheWrite: *r.Write, Label: r.Label}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty Pro prices")
	}
	families, hasF, e := documentObject(string(b), "longContextFamilies")
	if e != nil {
		return nil, e
	}
	models, hasM, e := documentObject(string(b), "longContextModelFamilies")
	if e != nil {
		return nil, e
	}
	if hasF != hasM {
		return nil, fmt.Errorf("incomplete context tables")
	}
	for id, f := range models {
		id = strings.ToLower(id)
		p, ok := out[id]
		if !ok {
			continue
		}
		family, ok := f.(string)
		if !ok {
			return nil, fmt.Errorf("invalid family")
		}
		rates, ok := families[family].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("missing context family")
		}
		threshold, _ := rates["threshold"].(string)
		threshold = strings.ToUpper(strings.TrimSpace(threshold))
		mult := 1.0
		if strings.HasSuffix(threshold, "K") {
			mult = 1000
			threshold = strings.TrimSuffix(threshold, "K")
		} else if strings.HasSuffix(threshold, "M") {
			mult = 1e6
			threshold = strings.TrimSuffix(threshold, "M")
		}
		n, e := strconv.ParseFloat(threshold, 64)
		n *= mult
		if e != nil || !valid(n) || n <= 0 || n >= float64(^uint64(0)) || float64(uint64(n)) != n {
			return nil, fmt.Errorf("invalid context threshold")
		}
		rate := func(key string) (float64, error) {
			s, ok := rates[key].(string)
			if !ok {
				return 0, fmt.Errorf("missing context rate")
			}
			v, e := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(s), "$"), 64)
			if e != nil || !valid(v) {
				return 0, fmt.Errorf("invalid context rate")
			}
			return v, nil
		}
		i, e := rate("input")
		if e != nil {
			return nil, e
		}
		o, e := rate("output")
		if e != nil {
			return nil, e
		}
		r, e := rate("cacheInput")
		if e != nil {
			return nil, e
		}
		w := p.CacheWrite
		if _, ok := rates["cacheWrite"]; ok {
			w, e = rate("cacheWrite")
			if e != nil {
				return nil, e
			}
		}
		p.Tiers = append(p.Tiers, Tier{Above: uint64(n), Input: i, Output: o, CacheRead: r, CacheWrite: w})
		out[id] = p
	}
	return out, nil
}
