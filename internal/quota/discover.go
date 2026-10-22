package quota

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func homePath(p string) string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, filepath.FromSlash(p))
}
func configPath(p string) string {
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "devin-usage-metrics", p)
}
func readDoc(p string) (object, error) {
	b, e := os.ReadFile(p)
	if e != nil {
		return nil, errors.New("credentials unavailable")
	}
	return decodeDoc(b)
}
func decodeDoc(b []byte) (object, error) {
	var d object
	if json.Unmarshal(b, &d) != nil {
		b, _ = hex.DecodeString(strings.TrimSpace(string(b)))
		if json.Unmarshal(b, &d) != nil {
			return nil, errors.New("invalid credentials JSON")
		}
	}
	return d, nil
}
func exists(p string) bool { s, e := os.Stat(p); return e == nil && !s.IsDir() }
func atomicWrite(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".quota-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	mode := os.FileMode(0600)
	if s, e := os.Stat(p); e == nil {
		mode = s.Mode().Perm()
	}
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
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
func security(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "security", args...).Output()
}
func keyExists(s string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, e := security("find-generic-password", "-s", s)
	return e == nil
}
func keyRead(s string) (object, error) {
	b, e := security("find-generic-password", "-s", s, "-w")
	if e != nil {
		return nil, errors.New("keychain credentials unavailable")
	}
	if s == "gemini" {
		b, _ = base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(b)), "go-keyring-base64:"))
		d, e := decodeDoc(b)
		return obj(d["token"]), e
	}
	return decodeDoc(b)
}
func keyWrite(s string, d object) error {
	b, e := security("find-generic-password", "-s", s)
	if e != nil {
		return errors.New("keychain item unavailable")
	}
	account := ""
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, `"acct"<blob>=`) {
			account, _ = strconv.Unquote(strings.TrimPrefix(l, `"acct"<blob>=`))
		}
	}
	if account == "" {
		return errors.New("keychain account unavailable")
	}
	data, _ := json.Marshal(d)
	_, e = security("add-generic-password", "-U", "-s", s, "-a", account, "-w", string(data))
	if e != nil {
		return errors.New("keychain update failed")
	}
	return nil
}

// Discover never performs network requests. Keychain is touched only with explicit consent.
func Discover(allowKeychain bool) []Account {
	out := []Account{}
	for _, p := range []string{"claude", "codex", "grok", "antigravity"} {
		rel := map[string]string{"claude": ".claude/.credentials.json", "codex": ".codex/auth.json", "grok": ".grok/auth.json", "antigravity": ".gemini/oauth_creds.json"}[p]
		path := homePath(rel)
		if p == "antigravity" && runtime.GOOS == "windows" && os.Getenv("APPDATA") != "" {
			state := filepath.Join(os.Getenv("APPDATA"), "Antigravity", "User", "globalStorage", "state.vscdb")
			if _, err := readAntigravityStateToken(state); err == nil {
				path = state
			}
		}
		label := map[string]string{"claude": "Claude", "codex": "Codex", "grok": "Grok", "antigravity": "Antigravity"}[p]
		d, _ := readDoc(path)
		email := str(d["email"])
		if p == "claude" {
			email = str(at(d, "claudeAiOauth", "emailAddress"))
		}
		if p == "grok" {
			_, g := grokEntry(d)
			email = str(g["email"])
		}
		if p == "antigravity" {
			parts := strings.Split(str(d["id_token"]), ".")
			if len(parts) > 1 {
				b, _ := base64.RawURLEncoding.DecodeString(parts[1])
				var c object
				_ = json.Unmarshal(b, &c)
				email = str(c["email"])
			}
		}
		if email != "" {
			label = email
		}
		if filepath.Ext(path) == ".vscdb" {
			if email := readAntigravityStateEmail(path); email != "" {
				label = email
			}
		}
		service := ""
		if p == "claude" {
			service = "Claude Code-credentials"
		}
		if p == "antigravity" {
			service = "gemini"
		}
		if allowKeychain && service != "" && keyExists(service) {
			out = append(out, Account{p + "-keychain", p, label, "keychain:" + service})
		} else if exists(path) {
			out = append(out, Account{p + "-local", p, label, path})
		}
	}
	if token, org := devinCLI(); token != "" && org != "" {
		out = append(out, Account{"devin-cli", "devin", "Devin CLI", homePath(".local/share/devin/credentials.toml")})
	}
	for i, d := range loadStore().Devin {
		label := d.Label
		if label == "" {
			label = "Devin"
		}
		out = append(out, Account{"devin-" + strconv.Itoa(i), "devin", label, "stored:" + strconv.Itoa(i)})
	}
	return out
}

type storedDevin struct {
	Label  string `json:"label"`
	Cookie string `json:"cookie"`
	Org    string `json:"org_id"`
}
type store struct {
	Devin []storedDevin `json:"devin"`
}

var storeMu sync.Mutex

func loadStore() store {
	b, _ := os.ReadFile(configPath("quota-accounts.json"))
	var s store
	_ = json.Unmarshal(b, &s)
	return s
}
func RemoveDevinAccount(index int) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	s := loadStore()
	if index < 0 || index >= len(s.Devin) {
		return errors.New("account not found")
	}
	s.Devin = append(s.Devin[:index], s.Devin[index+1:]...)
	return atomicWrite(configPath("quota-accounts.json"), s)
}
func KeychainAllowed() bool {
	d, _ := readDoc(configPath("quota-prefs.json"))
	return d["keychain_allowed"] == true
}
func SetKeychainAllowed(b bool) error {
	return atomicWrite(configPath("quota-prefs.json"), object{"keychain_allowed": b})
}
func MacOSMayHaveKeychainAccounts() bool {
	return runtime.GOOS == "darwin" && (dirExists(homePath(".claude")) || dirExists(homePath(".gemini/antigravity")) || dirExists(homePath(".gemini/config")))
}
func dirExists(p string) bool { s, e := os.Stat(p); return e == nil && s.IsDir() }
func devinCLI() (string, string) {
	b, e := os.ReadFile(homePath(".local/share/devin/credentials.toml"))
	if e != nil {
		d, _ := os.UserConfigDir()
		b, _ = os.ReadFile(filepath.Join(d, "devin/credentials.toml"))
	}
	token := ""
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok && strings.TrimSpace(k) == "windsurf_api_key" {
			token, _ = strconv.Unquote(strings.TrimSpace(v))
		}
	}
	d, e := readDoc(homePath(".config/devin/config.json"))
	if e != nil {
		c, _ := os.UserConfigDir()
		d, _ = readDoc(filepath.Join(c, "devin/config.json"))
	}
	return token, str(at(d, "devin", "org_id"))
}
func credentialIdentity(a Account) string {
	if strings.HasPrefix(a.source, "stored:") {
		i, _ := strconv.Atoi(strings.TrimPrefix(a.source, "stored:"))
		s := loadStore()
		if i < len(s.Devin) {
			return s.Devin[i].Cookie + s.Devin[i].Org
		}
	}
	if a.Key == "devin-cli" {
		t, o := devinCLI()
		return t + o
	}
	return ""
}
func loadAccount(a Account) (object, error) {
	d, _, e := loadAccountSource(a)
	return d, e
}
func loadAccountSource(a Account) (object, string, error) {
	if strings.HasPrefix(a.source, "keychain:") {
		d, e := keyRead(strings.TrimPrefix(a.source, "keychain:"))
		if a.Provider == "claude" && (e != nil || strings.TrimSpace(str(at(d, "claudeAiOauth", "accessToken"))) == "") {
			p := homePath(".claude/.credentials.json")
			d, e = readDoc(p)
			return d, p, e
		}
		return d, a.source, e
	}
	if a.Provider == "antigravity" && filepath.Ext(a.source) == ".vscdb" {
		d, e := readAntigravityStateToken(a.source)
		return d, a.source, e
	}
	d, e := readDoc(a.source)
	return d, a.source, e
}
