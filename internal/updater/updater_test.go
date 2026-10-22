package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func archiveFixture(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, name := range names {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		w.Write([]byte("fixture"))
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestReleaseDownload(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	b := archiveFixture(t, "devin-usage-metrics.exe", "assets/icon.png")
	bad := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.2.12","html_url":"https://example.test/release","body":"notes","assets":[{"name":"devin-usage-metrics-windows-x64.zip","browser_download_url":%q}]}`, server.URL+"/asset")
		case "/asset":
			w.Write(b)
		case "/asset.sha256":
			if bad {
				fmt.Fprint(w, "junk")
			} else {
				fmt.Fprintf(w, "\ufeff%x  file.zip\n", sha256.Sum256(b))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	r, e := check(context.Background(), CurrentVersion, server.URL+"/latest", "windows", "amd64")
	if e != nil || r.Version != "0.2.12" || r.Notes != "notes" {
		t.Fatalf("%+v %v", r, e)
	}
	var done int64
	p, e := Download(context.Background(), r, func(d, total int64) { done = d })
	if e != nil {
		t.Fatal(e)
	}
	if done != int64(len(b)) {
		t.Fatal(done)
	}
	if content, e := os.ReadFile(p); e != nil || string(content) != "fixture" {
		t.Fatal(e)
	}
	bad = true
	if _, e = Download(context.Background(), r, nil); e == nil {
		t.Fatal("malformed checksum accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = Download(ctx, r, nil); e == nil {
		t.Fatal("cancellation ignored")
	}
	if r, e = check(context.Background(), "0.2.12", server.URL+"/latest", "windows", "amd64"); e != nil || r.Version != "" {
		t.Fatal(r, e)
	}
}
func TestUnsafeArchives(t *testing.T) {
	for _, names := range [][]string{{"../escape"}, {"/absolute"}, {"C:/evil"}, {"a\\b"}, {"a", "A"}, {"devin-usage-metrics"}, {"a/../b"}, {"a", "a/b"}, {"NUL.txt"}} {
		t.Run(names[0], func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "test.zip")
			os.WriteFile(p, archiveFixture(t, names...), 0600)
			if _, e := extract(p, filepath.Join(d, "out"), "windows"); e == nil {
				t.Fatal("unsafe/missing payload accepted")
			}
		})
	}
}
func TestSupportedPackaging(t *testing.T) {
	for _, pair := range [][2]string{{"linux", "amd64"}, {"windows", "arm64"}, {"darwin", "386"}} {
		if _, e := assetName(pair[0], pair[1]); e == nil {
			t.Fatal(pair)
		}
	}
	if runtime.GOOS == "linux" {
		if e := Apply("/nonexistent"); e == nil {
			t.Fatal("Linux apply accepted")
		}
	}
}

func TestMacBundleAndSymlink(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "mac.zip")
	os.WriteFile(p, archiveFixture(t, "Agent Usage Metrics.app/Contents/Info.plist", "Agent Usage Metrics.app/Contents/MacOS/devin-usage-metrics"), 0600)
	if _, err := extract(p, filepath.Join(d, "out"), "darwin"); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: "devin-usage-metrics.exe"}
	h.SetMode(os.ModeSymlink | 0777)
	w, err := z.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("/etc/passwd"))
	z.Close()
	os.WriteFile(p, b.Bytes(), 0600)
	if _, err = extract(p, filepath.Join(d, "unsafe"), "windows"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestSkippedVersionIgnoresPrefix(t *testing.T) {
	if !IsSkipped("0.2.0", "v0.2.0") || !IsSkipped("v0.3.1", "0.3.1") || IsSkipped("0.3.1", "") || IsSkipped("0.3.2", "0.3.1") {
		t.Fatal("skip comparison differs from Rust")
	}
}

func TestDevelopmentBuildIsNotPackaged(t *testing.T) {
	if Packaged() {
		t.Fatal("test binary must not count as a packaged install")
	}
	if err := RemoveBackups(); err != nil {
		t.Fatal(err)
	}
}
