package data

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestDecodedTranscriptsAreForgottenAfterDeletion(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "s.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := model.Data{FileMtimes: map[string]int64{}}
	readLines(&d, model.Claude, p, false)
	if _, ok := decodedFiles.Load(p); !ok {
		t.Fatal("transcript was not cached")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	files(&d, model.Claude, []string{root}, ".jsonl")
	if _, ok := decodedFiles.Load(p); ok {
		t.Fatal("deleted transcript stayed cached")
	}
}

func TestReloadReusesUnchangedDecodedFiles(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, "cache"))
	p := filepath.Join(h, ".pi/agent/sessions/s.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"message","timestamp":100,"message":{"role":"assistant","usage":{"input":1}}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	first := Load(model.Pi, 0, 200, true)
	if len(first.Errors) != 0 || len(first.Turns) != 1 {
		t.Fatal("initial parse failed")
	}
	old, ok := decodedFiles.Load(p)
	if !ok {
		t.Fatal("decoded transcript missing")
	}
	pointer := reflect.ValueOf(old.(lineCache).Lines[0]).Pointer()
	again := Reload(model.Pi, 0, 200)
	if len(again.Errors) != 0 || !reflect.DeepEqual(first.Turns, again.Turns) {
		t.Fatal("unchanged reload changed records")
	}
	current, _ := decodedFiles.Load(p)
	if reflect.ValueOf(current.(lineCache).Lines[0]).Pointer() != pointer {
		t.Fatal("unchanged reload decoded transcript again")
	}
	if err := os.WriteFile(p, []byte(line+line), 0600); err != nil {
		t.Fatal(err)
	}
	changed := Reload(model.Pi, 0, 200)
	if len(changed.Errors) != 0 || len(changed.Turns) != 2 {
		t.Fatal("reload reused stale TTL snapshot after source changed")
	}
	Load(model.Pi, 0, 200, true)
	current, _ = decodedFiles.Load(p)
	if reflect.ValueOf(current.(lineCache).Lines[0]).Pointer() == pointer {
		t.Fatal("full rebuild reused decoded source")
	}
}

// Slimming cached transcripts must never change a parsed value: titles are
// compact()'s first 100 runes and content blocks are chosen by blankness.
func TestSlimKeepsTitlesAndBlankness(t *testing.T) {
	long := func(prefix string, fill rune) string { return prefix + strings.Repeat(string(fill), 4000) }
	cases := []string{
		long("", 'x'),
		long(strings.Repeat(" \n\t", 600), 'y'),  // leading whitespace past the threshold
		long("标题 "+strings.Repeat("汉", 50), '字'), // multi-byte runes
		long(strings.Repeat("a ", 60), 'b'),      // whitespace inside the first 100 runes
		strings.Repeat(" ", 3000),                // blank but long
	}
	for i, s := range cases {
		line := map[string]any{"message": map[string]any{"content": []any{map[string]any{"text": s}}}}
		slim(line)
		got := line["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if compact(got) != compact(s) || (strings.TrimSpace(got) == "") != (strings.TrimSpace(s) == "") {
			t.Fatalf("case %d: slimming changed the title or blankness", i)
		}
		if len(s) > slimThreshold && len(got) >= len(s) && strings.TrimSpace(s) != "" {
			t.Fatalf("case %d: long string was not shortened", i)
		}
	}
}

func TestDecodedCacheEvictsLeastRecentlyUsedBeyondBudget(t *testing.T) {
	half := int64(decodedBudget/2 + 1)
	rememberDecoded("/old.jsonl", lineCache{Mtime: 1, Weight: half})
	rememberDecoded("/new.jsonl", lineCache{Mtime: 2, Weight: half})
	t.Cleanup(func() { forgetDecoded("/old.jsonl"); forgetDecoded("/new.jsonl") })
	if _, ok := decodedFiles.Load("/old.jsonl"); ok {
		t.Fatal("over budget kept the oldest file")
	}
	if _, ok := decodedFiles.Load("/new.jsonl"); !ok {
		t.Fatal("evicted the file just stored")
	}
	decodedWeight.Lock()
	total := decodedWeight.total
	decodedWeight.Unlock()
	if total > decodedBudget {
		t.Fatalf("cache weight %d exceeds budget", total)
	}
}

func TestDecodedAppendAndRewrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	t.Cleanup(func() { forgetDecoded(p) })
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []obj {
		t.Helper()
		d := empty(0, 200)
		lines := readLines(&d, model.Pi, p, false)
		if len(d.Errors) != 0 {
			t.Fatalf("read errors: %v", d.Errors)
		}
		return lines
	}
	// Keep the old tail identical when rewriting the first record.
	body := "{\"timestamp\":1}\n" + strings.Repeat("{}\n", 2000)
	write(body)
	first := read()
	pointer := reflect.ValueOf(first[0]).Pointer()
	write(body + "{\"timestamp\":2}\n")
	appended := read()
	if len(appended) != len(first)+1 || reflect.ValueOf(appended[0]).Pointer() != pointer {
		t.Fatal("append did not retain old decoded records")
	}
	if len(first) != 2001 {
		t.Fatal("append mutated the previous result")
	}
	write(strings.Replace(body, `"timestamp":1`, `"timestamp":3`, 1) + "{\"timestamp\":2}\n{\"timestamp\":4}\n")
	rewritten := read()
	if rewritten[0]["timestamp"] != float64(3) || len(rewritten) != len(first)+2 {
		t.Fatal("matching tail hid a rewritten prefix")
	}
	// An incomplete final record must be re-decoded when its suffix arrives.
	write("{\"timestamp\":5}")
	read()
	write("{\"timestamp\":5}\n{\"timestamp\":6}\n")
	if got := read(); len(got) != 2 || got[1]["timestamp"] != float64(6) {
		t.Fatal("unterminated final line was reused")
	}
}

func TestDecodeAheadReportsMissingTranscripts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.jsonl")
	r := decodeAhead(context.Background(), model.Claude, []string{p}, 0, 200, false)(0)
	if len(r.part.Errors) == 0 {
		t.Fatal("missing transcript was silently ignored")
	}
	r = decodeAhead(context.Background(), model.Grok, []string{p}, 0, 200, false)(0)
	if !r.missing || len(r.part.Errors) != 0 {
		t.Fatal("optional Grok log is not optional")
	}
}

func TestDecodedCacheKeepsRecentlyReadHistory(t *testing.T) {
	paths := []string{"/lru-a.jsonl", "/lru-b.jsonl", "/lru-c.jsonl"}
	t.Cleanup(func() {
		for _, p := range paths {
			forgetDecoded(p)
		}
	})
	weight := int64(decodedBudget/3 + 1)
	rememberDecoded(paths[0], lineCache{Mtime: 1, Weight: weight})
	rememberDecoded(paths[1], lineCache{Mtime: 100, Weight: weight})
	cachedDecoded(paths[0])
	rememberDecoded(paths[2], lineCache{Mtime: 200, Weight: weight})
	if _, ok := decodedFiles.Load(paths[0]); !ok {
		t.Fatal("recently read old history was evicted")
	}
	if _, ok := decodedFiles.Load(paths[1]); ok {
		t.Fatal("unused newer history stayed cached")
	}
}

func TestOversizedDecodedFileDoesNotFlushCache(t *testing.T) {
	p, huge := "/hot.jsonl", "/oversized.jsonl"
	t.Cleanup(func() { forgetDecoded(p); forgetDecoded(huge) })
	rememberDecoded(p, lineCache{Mtime: 1, Weight: 1024})
	rememberDecoded(huge, lineCache{Mtime: 2, Weight: decodedBudget + 1})
	if _, ok := decodedFiles.Load(p); !ok {
		t.Fatal("oversized source flushed useful history")
	}
	if _, ok := decodedFiles.Load(huge); ok {
		t.Fatal("oversized source exceeded the cache budget")
	}
}
