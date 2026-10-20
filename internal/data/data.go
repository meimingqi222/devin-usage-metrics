// Package data reads local agent histories without modifying their databases.
package data

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

var loadMu sync.Mutex
var deviceMu sync.Mutex

func home(p string) string { h, _ := os.UserHomeDir(); return filepath.Join(h, p) }
func configPath() string {
	p, _ := os.UserConfigDir()
	return filepath.Join(p, "devin-usage-metrics", "device.json")
}
func readDevice() map[string]string {
	v := map[string]string{}
	b, _ := os.ReadFile(configPath())
	_ = json.Unmarshal(b, &v)
	if v == nil {
		v = map[string]string{}
	}
	return v
}
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".aum-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	b, e := json.Marshal(v)
	if e == nil {
		_, e = f.Write(b)
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}

// DeviceID returns the persisted identity shared with the Rust application.
func DeviceID() string {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	unlock, e := lockDevice()
	if e != nil {
		if id := readDevice()["id"]; id != "" {
			return id
		}
		return fallbackDeviceID()
	}
	defer unlock()
	return deviceID()
}
func deviceID() string {
	v := readDevice()
	if v["id"] != "" {
		return v["id"]
	}
	b := make([]byte, 16)
	if _, e := rand.Read(b); e == nil {
		b[6] = b[6]&15 | 64
		b[8] = b[8]&63 | 128
		v["id"] = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
		if atomicJSON(configPath(), v) == nil {
			return v["id"]
		}
	}
	return fallbackDeviceID()
}
func fallbackDeviceID() string {
	h, _ := os.Hostname()
	var n uint64 = 0xcbf29ce484222325
	for _, c := range []byte(h) {
		n ^= uint64(c)
		n *= 0x100000001b3
	}
	return fmt.Sprintf("fallback-%x", n)
}
func DeviceName() string {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	if n := strings.TrimSpace(readDevice()["name"]); n != "" {
		return n
	}
	h, _ := os.Hostname()
	if h == "" {
		h = "This Device"
	}
	return h
}
func SetDeviceName(n string) error {
	n = strings.TrimSpace(n)
	if utf8.RuneCountInString(n) > 64 || strings.ContainsFunc(n, unicode.IsControl) {
		return fmt.Errorf("device name must be at most 64 characters and contain no controls")
	}
	deviceMu.Lock()
	defer deviceMu.Unlock()
	unlock, e := lockDevice()
	if e != nil {
		return e
	}
	defer unlock()
	id := deviceID()
	v := readDevice()
	v["id"] = id
	v["name"] = n
	return atomicJSON(configPath(), v)
}

type snapshot struct {
	Version int
	At      int64
	Data    model.Data
}

func empty(start, end int64) model.Data {
	return model.Data{TurnsStart: start, TurnsEnd: end, FileMtimes: map[string]int64{}, FileSessions: map[string]string{}}
}
func fail(d *model.Data, a model.Agent, e any) {
	d.Errors = append(d.Errors, model.DataError{Agent: a, Message: fmt.Sprint(e)})
}

// Load returns the selected agent's records. Refresh bypasses all cached records.
func Load(a model.Agent, start, end int64, refresh bool) model.Data {
	loadMu.Lock()
	defer loadMu.Unlock()
	valid := false
	for _, known := range model.Agents {
		if a == known {
			valid = true
			break
		}
	}
	if !valid || end <= start {
		d := empty(start, end)
		fail(&d, a, "unsupported agent or invalid time range")
		return d
	}
	cache, _ := os.UserCacheDir()
	path := filepath.Join(cache, "devin-usage-metrics", "go-data-"+string(a)+".json")
	var old snapshot
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &old)
	now := time.Now().Unix()
	covers := old.Version == 1 && old.Data.TurnsStart <= start && old.Data.TurnsEnd >= min(end, now)
	if !refresh && covers && now-old.At <= 300 {
		return filter(old.Data, start, end)
	}
	d := empty(start, end)
	switch a {
	case model.Claude, model.Codex, model.Pi, model.WorkBuddy, model.Grok:
		loadFiles(&d, a, refresh)
	case model.Devin, model.ZCode, model.OpenCode, model.MimoCode, model.Antigravity:
		loadDBs(&d, a, refresh)
	default:
		fail(&d, a, "unsupported agent")
	}
	sort.SliceStable(d.Turns, func(i, j int) bool { return d.Turns[i].CreatedAt < d.Turns[j].CreatedAt })
	sort.Slice(d.Sessions, func(i, j int) bool { return d.Sessions[i].Key < d.Sessions[j].Key })
	id, name := DeviceID(), DeviceName()
	for i := range d.Sessions {
		d.Sessions[i].DeviceID = id
		d.Sessions[i].DeviceName = name
	}
	for i := range d.Turns {
		d.Turns[i].DeviceID = id
		d.Turns[i].DeviceName = name
	}
	raw, _ := json.Marshal([]any{d.Sessions, d.Turns})
	d.SyncContentHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	if e := atomicJSON(path, snapshot{1, now, d}); e != nil {
		fail(&d, a, fmt.Errorf("save cache: %w", e))
	}
	return d
}
func filter(d model.Data, start, end int64) model.Data {
	turns := []model.Turn{}
	for _, t := range d.Turns {
		if t.CreatedAt >= start && t.CreatedAt < end {
			turns = append(turns, t)
		}
	}
	d.Turns = turns
	d.TurnsStart = start
	d.TurnsEnd = end
	return d
}
func LoadAll(start, end int64, refresh bool) model.Data {
	d := empty(start, end)
	for _, a := range model.Agents {
		p := Load(a, start, end, refresh)
		d.Sessions = append(d.Sessions, p.Sessions...)
		d.Turns = append(d.Turns, p.Turns...)
		d.Errors = append(d.Errors, p.Errors...)
		for k, v := range p.FileMtimes {
			d.FileMtimes[k] = v
		}
		for k, v := range p.FileSessions {
			d.FileSessions[k] = v
		}
	}
	sort.SliceStable(d.Turns, func(i, j int) bool { return d.Turns[i].CreatedAt < d.Turns[j].CreatedAt })
	return d
}
