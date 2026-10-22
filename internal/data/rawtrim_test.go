package data

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func slimDecode(t *testing.T, line []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		t.Fatalf("decode %d bytes: %v", len(line), err)
	}
	v, _ = slim(v)
	return v
}

// Trimming raw literals must leave every slimmed value unchanged, whatever
// the escapes, scripts and whitespace in the string.
func TestTrimLongStringsKeepsSlimmedValues(t *testing.T) {
	pieces := []string{"a", "Z", " ", "\n", "\t", "中", "文", "😀", "é", `"`, `\`, "\u00e9", "\u2028", "\x01", "{}", "[1,2]"}
	r := rand.New(rand.NewPCG(1, 2))
	body := func(n int, mix []string) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(mix[r.IntN(len(mix))])
		}
		return b.String()
	}
	cases := []string{
		body(200<<10, pieces),
		body(90<<10, []string{" ", "\n", "\t"}) + body(80<<10, pieces), // visible runes come late
		body(70<<10, []string{"中", "😀", " "}) + body(70<<10, []string{"x"}),
		body(300<<10, []string{`\`, `"`, "a"}),
		strings.Repeat(" ", 200<<10),          // never 101 visible runes: kept whole
		body(70<<10, []string{"é", "\u2028"}), // no unescaped ASCII to count: kept whole
	}
	keptWhole := map[int]bool{4: true, 5: true}
	for i, s := range cases {
		for _, wrap := range []func(string) any{
			func(s string) any { return map[string]any{"type": "user", "message": map[string]any{"content": s}} },
			func(s string) any { return []any{s, map[string]any{"k": s, "n": 1.5}, "short"} },
		} {
			line, _ := json.Marshal(wrap(s))
			// encoding/json escapes <, >, & and U+2028 as \u sequences;
			// a hand-written escape-heavy form exercises \uXXXX too.
			for j, raw := range [][]byte{line, []byte(strings.ReplaceAll(string(line), "a", `\u0061`))} {
				trimmed := trimLongStrings(raw)
				// Only unescaped ASCII counts as visible, so the escaped form
				// may conservatively stay whole; its value must still match.
				if j == 0 && len(raw) > rawKeep && len(trimmed) >= len(raw) && !keptWhole[i] {
					t.Errorf("case %d: %d-byte line was not shortened", i, len(raw))
				}
				if want, got := slimDecode(t, raw), slimDecode(t, trimmed); !reflect.DeepEqual(want, got) {
					t.Fatalf("case %d: trimmed line decodes differently", i)
				}
			}
		}
	}
	short := []byte(`{"a":"b"}`)
	if got := trimLongStrings(short); &got[0] != &short[0] {
		t.Fatal("short lines must not be copied")
	}
}

func TestTrimLongStringsKeepsKeysAndInvalidInput(t *testing.T) {
	key := strings.Repeat("key", rawKeep)
	line, err := json.Marshal(map[string]any{key: strings.Repeat("value", rawKeep)})
	if err != nil {
		t.Fatal(err)
	}
	if want, got := slimDecode(t, line), slimDecode(t, trimLongStrings(line)); !reflect.DeepEqual(want, got) {
		t.Fatal("trimming changed an object key")
	}
	// Invalid escapes after the retained prefix must not become valid JSON.
	invalid := []byte(`{"text":"` + strings.Repeat("x", rawKeep+100) + `\q"}`)
	if got := trimLongStrings(invalid); !reflect.DeepEqual(got, invalid) {
		t.Fatal("trimming hid invalid JSON")
	}
}

func BenchmarkDecodeLongTranscript(b *testing.B) {
	line, err := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": strings.Repeat("tool output with Unicode 中文 and escaped quotes \"\n", 24000)},
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, trim := range []bool{false, true} {
		name := "full"
		if trim {
			name = "trimmed"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			for b.Loop() {
				raw := line
				if trim {
					raw = trimLongStrings(raw)
				}
				var v obj
				if err := json.Unmarshal(raw, &v); err != nil {
					b.Fatal(err)
				}
				slim(v)
			}
		})
	}
}
