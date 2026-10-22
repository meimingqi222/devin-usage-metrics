package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func BenchmarkExistingImmutableUpload(b *testing.B) {
	s := newTestCore(b.TempDir(), 0)
	handler := http.HandlerFunc(s.object)
	body := strings.Repeat("payload-", 65536)
	name := "/v1/objects/v3-block-" + strings.Trim(quoteETag([]byte(body)), `"`) + ".zst"
	headers := map[string]string{"If-None-Match": "*"}
	if r := request(handler, http.MethodPut, name, body, headers); r.Code != http.StatusNoContent {
		b.Fatal(r.Code)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := request(handler, http.MethodPut, name, body, headers); r.Code != http.StatusPreconditionFailed {
			b.Fatal(r.Code)
		}
	}
}

func TestExistingImmutableUploadDoesNotTouchDirectory(t *testing.T) {
	s := newTestCore(t.TempDir(), 0)
	handler := http.HandlerFunc(s.object)
	body := "immutable content"
	name := "v3-block-" + strings.Trim(quoteETag([]byte(body)), `"`) + ".zst"
	target := "/v1/objects/" + name
	headers := map[string]string{"If-None-Match": "*"}
	if r := request(handler, http.MethodPut, target, body, headers); r.Code != http.StatusNoContent {
		t.Fatal(r.Code)
	}
	dir := filepath.Join(s.root, testTenant)
	old := time.Unix(1600000000, 0)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := request(handler, http.MethodPut, target, body, headers)
	if r.Code != http.StatusPreconditionFailed || r.Header().Get("ETag") != quoteETag([]byte(body)) {
		t.Fatal("existing object response changed")
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("rejected upload created/deleted a temporary file")
	}
	// A corrupted copy must expose its actual ETag, not the hash in its name.
	damaged := "broken"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(damaged), 0600); err != nil {
		t.Fatal(err)
	}
	r = request(handler, http.MethodPut, target, body, headers)
	if r.Code != http.StatusPreconditionFailed || r.Header().Get("ETag") != quoteETag([]byte(damaged)) {
		t.Fatal("damaged object was falsely confirmed")
	}
	r = request(handler, http.MethodPut, target, body, map[string]string{"If-Match": quoteETag([]byte(damaged))})
	if r.Code != http.StatusNoContent {
		t.Fatalf("repair failed: %d", r.Code)
	}
	if r = request(handler, http.MethodGet, target, "", nil); r.Body.String() != body {
		t.Fatal("repair content changed")
	}
	// Even if the destination exists, invalid incoming bytes still fail validation.
	if r = request(handler, http.MethodPut, target, "wrong", headers); r.Code != http.StatusBadRequest {
		t.Fatal("body validation bypassed")
	}
}
