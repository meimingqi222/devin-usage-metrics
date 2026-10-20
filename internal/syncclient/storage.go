// Package syncclient implements the desktop sync-server wire protocol.
package syncclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Enabled bool   `json:"enabled"`
	APIURL  string `json:"api_url"`
}

func path(name string) string {
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "devin-usage-metrics", name)
}
func atomic(name string, b []byte) error {
	p := path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".sync-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), p)
}
func ReadConfig() Config {
	var c Config
	b, _ := os.ReadFile(path("sync.json"))
	if json.Unmarshal(b, &c) != nil {
		return Config{}
	}
	if strings.TrimSpace(c.APIURL) == "" {
		var old struct {
			URL string `json:"sync_server_url"`
		}
		_ = json.Unmarshal(b, &old)
		c.APIURL = old.URL
	}
	return c
}
func SaveConfig(c Config) error {
	m := map[string]json.RawMessage{}
	b, e := os.ReadFile(path("sync.json"))
	if e == nil {
		if e = json.Unmarshal(b, &m); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m["enabled"], _ = json.Marshal(c.Enabled)
	m["api_url"], _ = json.Marshal(c.APIURL)
	b, e = json.Marshal(m)
	if e != nil {
		return e
	}
	return atomic("sync.json", b)
}
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func device() (string, error) {
	b, e := os.ReadFile(path("device.json"))
	if e != nil {
		return "", errors.New("device identity must be initialized before sync")
	}
	var d struct {
		ID string `json:"device_id"`
	}
	if e = json.Unmarshal(b, &d); e != nil {
		return "", e
	}
	if d.ID == "" {
		var m map[string]string
		_ = json.Unmarshal(b, &m)
		d.ID = m["id"]
	}
	if d.ID == "" {
		return "", errors.New("missing device identity")
	}
	return d.ID, nil
}

type session struct {
	Token   string `json:"sync_token"`
	Login   string `json:"login"`
	Expires int64  `json:"expires_at"`
}

func sessionCipher() (cipher.AEAD, error) {
	id, e := device()
	if e != nil {
		return nil, e
	}
	k := sha256.Sum256([]byte("devin-usage-metrics/github-sync-session/v1\x00" + id))
	b, e := aes.NewCipher(k[:])
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}
func loadSession() (session, error) {
	var s session
	a, e := sessionCipher()
	if e != nil {
		return s, e
	}
	b, e := os.ReadFile(path("github-sync-session.enc"))
	if e != nil {
		return s, e
	}
	if len(b) < 33 || string(b[:5]) != "DUMS1" {
		return s, errors.New("invalid encrypted session")
	}
	p, e := a.Open(nil, b[5:17], b[17:], nil)
	if e != nil {
		return s, errors.New("cannot decrypt session")
	}
	e = json.Unmarshal(p, &s)
	return s, e
}
func saveSession(s session) error {
	a, e := sessionCipher()
	if e != nil {
		return e
	}
	n := make([]byte, 12)
	if _, e = rand.Read(n); e != nil {
		return e
	}
	p, e := json.Marshal(s)
	if e != nil {
		return e
	}
	b := append([]byte("DUMS1"), n...)
	b = a.Seal(b, n, p, nil)
	if e = atomic("github-sync-session.enc", b); e != nil {
		return e
	}
	m, _ := json.Marshal(struct {
		Login   string `json:"login"`
		Expires int64  `json:"expires_at"`
	}{s.Login, s.Expires})
	return atomic("github-sync-session.meta.json", m)
}
func Logout() error {
	var result error
	for _, n := range []string{"github-sync-session.enc", "github-sync-session.meta.json"} {
		if e := os.Remove(path(n)); e != nil && !os.IsNotExist(e) {
			result = e
		}
	}
	return result
}
func SessionLabel() string {
	var m struct {
		Login string `json:"login"`
	}
	b, _ := os.ReadFile(path("github-sync-session.meta.json"))
	_ = json.Unmarshal(b, &m)
	return m.Login
}
