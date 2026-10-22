package data

import (
	"os"
	"path/filepath"
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

func TestDecodedCacheEvictsLeastRecentlyModifiedBeyondBudget(t *testing.T) {
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
