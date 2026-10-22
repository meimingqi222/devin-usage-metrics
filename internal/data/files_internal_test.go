package data

import (
	"os"
	"path/filepath"
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
