// Package updater implements the stable GitHub release channel. Checksums detect
// corruption, not publisher authenticity. Apply starts a helper; callers must exit
// after it succeeds so the helper can replace and restart the packaged app.
package updater

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// CurrentVersion is set at startup from mygo.json, the single source of the
// app's version; the packaged bundle carries the same value.
var CurrentVersion = "0.0.0"

const latestURL = "https://api.github.com/repos/meimingqi222/devin-usage-metrics/releases/latest"
const maxArchive = 512 << 20

// URL is the release page, not a download URL. Private asset metadata is supplied
// by Check; a hand-constructed Release cannot bypass asset selection.
type Release struct {
	Version, URL, Notes         string
	assetURL, assetName, target string
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func assetName(goos, arch string) (string, error) {
	suffix := ""
	switch {
	case goos == "windows" && arch == "amd64":
		suffix = "windows-x64"
	case goos == "darwin" && arch == "amd64":
		suffix = "macos-x64"
	case goos == "darwin" && arch == "arm64":
		suffix = "macos-arm64"
	default:
		return "", errors.New("unsupported platform for app update")
	}
	return "devin-usage-metrics-" + suffix + ".zip", nil
}
func version(s string) ([3]uint64, error) {
	var v [3]uint64
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return v, errors.New("invalid stable version")
	}
	for i, p := range parts {
		n, e := strconv.ParseUint(p, 10, 64)
		if e != nil {
			return v, errors.New("invalid stable version")
		}
		v[i] = n
	}
	return v, nil
}
func get(ctx context.Context, url string) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", url, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("User-Agent", "AgentUsageMetrics-Updater/"+CurrentVersion)
	req.Header.Set("Accept", "application/vnd.github+json")
	r, e := httpClient.Do(req)
	if e != nil {
		return nil, e
	}
	if r.StatusCode != 200 {
		r.Body.Close()
		return nil, fmt.Errorf("update HTTP %d", r.StatusCode)
	}
	return r, nil
}
func bounded(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e == nil && int64(len(b)) > limit {
		e = errors.New("update response exceeds size limit")
	}
	return b, e
}

// Check returns a zero Release when no newer stable release exists.
func Check(ctx context.Context, current string) (Release, error) {
	return check(ctx, current, latestURL, runtime.GOOS, runtime.GOARCH)
}
func check(ctx context.Context, current, url, goos, arch string) (Release, error) {
	name, e := assetName(goos, arch)
	if e != nil {
		return Release{}, e
	}
	if current == "" {
		current = CurrentVersion
	}
	cv, e := version(current)
	if e != nil {
		return Release{}, e
	}
	r, e := get(ctx, url)
	if e != nil {
		return Release{}, e
	}
	defer r.Body.Close()
	b, e := bounded(r.Body, 4<<20)
	if e != nil {
		return Release{}, e
	}
	var d struct {
		Tag               string `json:"tag_name"`
		URL               string `json:"html_url"`
		Body              string `json:"body"`
		Draft, Prerelease bool
		Assets            []struct {
			Name string
			URL  string `json:"browser_download_url"`
		}
	}
	if e = json.Unmarshal(b, &d); e != nil {
		return Release{}, e
	}
	if d.Draft || d.Prerelease {
		return Release{}, nil
	}
	nv, e := version(d.Tag)
	if e != nil {
		return Release{}, e
	}
	newer := false
	for i := range cv {
		if nv[i] != cv[i] {
			newer = nv[i] > cv[i]
			break
		}
	}
	if !newer {
		return Release{}, nil
	}
	for _, a := range d.Assets {
		if a.Name == name && a.URL != "" {
			return Release{strings.TrimPrefix(d.Tag, "v"), d.URL, d.Body, a.URL, name, goos}, nil
		}
	}
	return Release{}, errors.New("no matching asset for this platform")
}

type progressWriter struct {
	w           io.Writer
	done, total int64
	fn          func(int64, int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, e := p.w.Write(b)
	p.done += int64(n)
	if p.fn != nil {
		p.fn(p.done, p.total)
	}
	return n, e
}

// Download verifies the sidecar before extracting into an exclusive private
// staging directory. On success the caller owns the returned payload's staging.
// Unknown total length is reported as -1.
func Download(ctx context.Context, release Release, onProgress func(done, total int64)) (payload string, err error) {
	if release.assetURL == "" || release.target == "" {
		return "", errors.New("release must come from Check")
	}
	cache, e := os.UserCacheDir()
	if e != nil {
		return "", e
	}
	cache = filepath.Join(cache, "devin-usage-metrics", "update-cache")
	if e = os.MkdirAll(cache, 0700); e != nil {
		return "", e
	}
	stage, e := os.MkdirTemp(cache, "staging-")
	if e != nil {
		return "", e
	}
	defer func() {
		if err != nil {
			os.RemoveAll(stage)
		}
	}()
	r, e := get(ctx, release.assetURL)
	if e != nil {
		return "", e
	}
	defer r.Body.Close()
	if r.ContentLength > maxArchive {
		return "", errors.New("archive exceeds size limit")
	}
	file, e := os.OpenFile(filepath.Join(stage, "archive.zip"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	hash := sha256.New()
	pw := &progressWriter{w: io.MultiWriter(file, hash), total: r.ContentLength, fn: onProgress}
	n, e := io.Copy(pw, io.LimitReader(r.Body, maxArchive+1))
	ce := file.Close()
	if e != nil {
		return "", e
	}
	if ce != nil {
		return "", ce
	}
	if n > maxArchive {
		return "", errors.New("archive exceeds size limit")
	}
	side, e := get(ctx, release.assetURL+".sha256")
	if e != nil {
		return "", e
	}
	defer side.Body.Close()
	b, e := bounded(side.Body, 4096)
	if e != nil {
		return "", e
	}
	fields := strings.Fields(strings.TrimPrefix(string(b), "\ufeff"))
	if len(fields) == 0 {
		return "", errors.New("checksum sidecar is malformed")
	}
	digest, e := hex.DecodeString(fields[0])
	if e != nil || len(digest) != 32 {
		return "", errors.New("checksum sidecar is malformed")
	}
	if !strings.EqualFold(fields[0], hex.EncodeToString(hash.Sum(nil))) {
		return "", errors.New("checksum mismatch")
	}
	if e = ctx.Err(); e != nil {
		return "", e
	}
	payload, e = extract(filepath.Join(stage, "archive.zip"), filepath.Join(stage, "payload"), release.target)
	if e != nil {
		return "", e
	}
	os.Remove(filepath.Join(stage, "archive.zip"))
	return payload, nil
}
func extract(archive, dest, target string) (string, error) {
	z, e := zip.OpenReader(archive)
	if e != nil {
		return "", e
	}
	defer z.Close()
	if len(z.File) > 10000 {
		return "", errors.New("too many zip entries")
	}
	seen := map[string]bool{}
	files := map[string]bool{}
	var total uint64
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		clean := path.Clean(name)
		key := strings.ToLower(clean)
		if name == "" || clean != name || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || seen[key] {
			return "", errors.New("unsafe or duplicate zip path")
		}
		for _, p := range strings.Split(name, "/") {
			base := strings.ToUpper(strings.SplitN(p, ".", 2)[0])
			reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9')
			if strings.HasSuffix(p, ".") || strings.HasSuffix(p, " ") || reserved {
				return "", errors.New("unsafe zip path")
			}
		}
		seen[key] = true
		files[key] = !f.FileInfo().IsDir()
		if f.Mode()&os.ModeSymlink != 0 || (!f.Mode().IsRegular() && !f.FileInfo().IsDir()) {
			return "", errors.New("unsupported zip entry type")
		}
		if f.UncompressedSize64 > 1<<30 || total > 1<<30-f.UncompressedSize64 {
			return "", errors.New("expanded zip exceeds size limit")
		}
		total += f.UncompressedSize64
	}
	for key := range seen {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if files[parent] {
				return "", errors.New("zip file/directory collision")
			}
		}
	}
	if e = os.MkdirAll(dest, 0700); e != nil {
		return "", e
	}
	for _, f := range z.File {
		p := filepath.Join(dest, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			e = os.MkdirAll(p, 0700)
		} else {
			if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
				return "", e
			}
			var in io.ReadCloser
			in, e = f.Open()
			if e != nil {
				return "", e
			}
			var out *os.File
			out, e = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|f.Mode().Perm()&0111)
			if e != nil {
				in.Close()
				return "", e
			}
			_, e = io.Copy(out, io.LimitReader(in, int64(f.UncompressedSize64)+1))
			ce := out.Close()
			in.Close()
			if e == nil {
				e = ce
			}
		}
		if e != nil {
			return "", e
		}
	}
	name := "Agent Usage Metrics.exe"
	if target == "darwin" {
		name = "Agent Usage Metrics.app"
	} else if target != "windows" {
		return "", errors.New("unsupported platform for app update")
	}
	p := filepath.Join(dest, name)
	if target == "windows" {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			p = filepath.Join(dest, "devin-usage-metrics.exe")
		}
	}
	s, e := os.Stat(p)
	if e != nil {
		return "", errors.New("archive does not contain packaged payload")
	}
	if target == "darwin" {
		if !s.IsDir() {
			return "", errors.New("app bundle missing")
		}
		if s, e = os.Stat(filepath.Join(p, "Contents", "MacOS", "Agent Usage Metrics")); e != nil || !s.Mode().IsRegular() {
			if s, e = os.Stat(filepath.Join(p, "Contents", "MacOS", "devin-usage-metrics")); e != nil || !s.Mode().IsRegular() {
				return "", errors.New("app executable missing")
			}
		}
	} else if !s.Mode().IsRegular() {
		return "", errors.New("exe payload missing")
	}
	return p, nil
}
