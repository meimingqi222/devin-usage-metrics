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

	"github.com/meimingqi222/devin-usage-metrics/internal/credentialid"
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
func keyReadItem(service, account string) (object, error) {
	d, e := keyReadFullItem(service, account)
	if e != nil {
		return nil, e
	}
	if service == "gemini" {
		return obj(d["token"]), nil
	}
	return d, nil
}

// keyReadFullItem returns the whole item, including Antigravity's wrapper
// fields. Switching must put the same shape back.
func keyReadFullItem(service, account string) (object, error) {
	args := []string{"find-generic-password", "-s", service}
	if account != "" {
		args = append(args, "-a", account)
	}
	args = append(args, "-w")
	b, e := security(args...)
	if e != nil {
		return nil, errors.New("keychain credentials unavailable")
	}
	if service == "gemini" {
		raw := strings.TrimSpace(string(b))
		if strings.HasPrefix(raw, "go-keyring-base64:") {
			raw = strings.TrimPrefix(raw, "go-keyring-base64:")
			b, _ = base64.StdEncoding.DecodeString(raw)
		} else {
			b = []byte(raw)
		}
	}
	return decodeDoc(b)
}

// keychainAccountFor is the account name each provider's item uses. A user
// name is enough to create Claude's item; Antigravity's go-keyring item uses
// "antigravity".
func keychainAccountFor(provider string) string {
	if provider == "antigravity" {
		return "antigravity"
	}
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		return user
	}
	if home, err := os.UserHomeDir(); err == nil {
		if base := filepath.Base(home); base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return "claude-code-user"
}

// keyWriteDoc updates or creates a provider's keychain item. Claude Code
// stores compact JSON; Antigravity's go-keyring item is base64 with a prefix.
func keyWriteDoc(provider, service string, d object) error {
	value, err := keychainValue(provider, d)
	if err != nil {
		return err
	}
	account := keychainAccountFor(provider)
	if provider == "claude" {
		// Update the item Claude Code created rather than adding a second
		// one under a different account name.
		if existing := keychainAccountName(service); existing != "" {
			account = existing
		}
	}
	_, err = security("add-generic-password", "-U", "-s", service, "-a", account, "-w", value)
	if err != nil {
		return errors.New("keychain update failed")
	}
	return nil
}

func keychainValue(provider string, d object) (string, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return "", errors.New("keychain credentials encode failed")
	}
	if provider == "antigravity" {
		return "go-keyring-base64:" + base64.StdEncoding.EncodeToString(data), nil
	}
	return string(data), nil
}

func keychainAccountName(service string) string {
	b, err := security("find-generic-password", "-s", service)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, `"acct"<blob>=`) {
			account, _ := strconv.Unquote(strings.TrimPrefix(l, `"acct"<blob>=`))
			return account
		}
	}
	return ""
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

func antigravityStatePaths() []string {
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return []string{filepath.Join(appData, "Antigravity", "User", "globalStorage", "state.vscdb")}
		}
	case "darwin":
		return []string{homePath("Library/Application Support/Antigravity/User/globalStorage/state.vscdb")}
	default:
		return []string{homePath(".config/Antigravity/User/globalStorage/state.vscdb")}
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
		service := ""
		if p == "claude" {
			service = "Claude Code-credentials"
		}
		if p == "antigravity" {
			service = "gemini"
		}
		keychain := allowKeychain && service != "" && keyExists(service)
		if keychain {
			var kd object
			var ke error
			if p == "antigravity" {
				kd, ke = keyReadFullItem(service, "antigravity")
			} else {
				kd, ke = keyReadFullItem(service, keychainAccountFor(p))
			}
			if ke == nil {
				// The live Keychain value, not a possibly stale file or
				// state row, names the account the agent will use.
				d = kd
			}
		}
		email := str(d["email"])
		if p == "claude" {
			email = str(at(d, "claudeAiOauth", "emailAddress"))
		}
		if p == "grok" {
			_, g := grokEntry(d)
			email = str(g["email"])
		}
		if p == "antigravity" {
			email = str(d["id_token_email"])
			if email == "" {
				parts := strings.Split(str(d["id_token"]), ".")
				if len(parts) > 1 {
					b, _ := base64.RawURLEncoding.DecodeString(parts[1])
					var c object
					_ = json.Unmarshal(b, &c)
					email = str(c["email"])
				}
			}
		}
		if email != "" {
			label = email
		}
		if filepath.Ext(path) == ".vscdb" && (email == "" || label == "Antigravity") {
			if email := readAntigravityStateEmail(path); email != "" {
				label = email
			}
		}
		if p == "antigravity" && (email == "" || label == "Antigravity") {
			// The desktop app's state database names the signed-in account
			// even when the credential itself has no id_token.
			for _, state := range antigravityStatePaths() {
				if found := readAntigravityStateEmail(state); found != "" {
					label = found
					break
				}
			}
		}
		if keychain {
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
	d, err := loadAccount(a)
	if err != nil {
		return ""
	}
	if id := credentialid.Of(a.Provider, d).Stable; id != "" {
		return id
	}
	// A provider's document without identity claims: bind the cache to the
	// tokens it was read with, so another login in the same file cannot
	// inherit the previous account's cache.
	switch a.Provider {
	case "claude":
		c := obj(d["claudeAiOauth"])
		return str(c["accessToken"]) + "\x00" + str(c["refreshToken"])
	case "codex":
		if token := str(at(d, "tokens", "refresh_token")); token != "" {
			return token
		}
		return str(d["refresh_token"])
	case "grok":
		_, entry := grokEntry(d)
		return str(entry["refresh_token"])
	case "antigravity":
		token := d
		if nested := obj(d["token"]); nested != nil {
			token = nested
		}
		return str(token["refresh_token"])
	}
	return ""
}
func loadAccount(a Account) (object, error) {
	d, _, e := loadAccountSource(a)
	return d, e
}
func loadAccountSource(a Account) (object, string, error) {
	if strings.HasPrefix(a.source, "keychain:") {
		service := strings.TrimPrefix(a.source, "keychain:")
		var d object
		var e error
		if a.Provider == "antigravity" {
			d, e = keyReadItem(service, "antigravity")
		} else {
			d, e = keyReadItem(service, keychainAccountFor(a.Provider))
			if e != nil {
				d, e = keyReadItem(service, "")
			}
		}
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

// CredentialDoc is the account's full credential document. It is meant for
// identity and for an in-process switch, never for logging. Unlike
// loadAccount, Antigravity's keychain value keeps its wrapper fields so a
// write puts the same shape back.
func (a Account) CredentialDoc() (object, error) {
	if strings.HasPrefix(a.source, "keychain:") {
		service := strings.TrimPrefix(a.source, "keychain:")
		if a.Provider == "antigravity" {
			return keyReadFullItem(service, "antigravity")
		}
		d, err := keyReadFullItem(service, keychainAccountFor(a.Provider))
		if err != nil {
			return keyReadFullItem(service, "")
		}
		return d, nil
	}
	return loadAccount(a)
}

// CredentialKind names where the credential is stored.
func (a Account) CredentialKind() string {
	switch {
	case strings.HasPrefix(a.source, "keychain:"):
		return "keychain"
	case strings.HasPrefix(a.source, "stored:"):
		return "stored"
	case filepath.Ext(a.source) == ".vscdb":
		return "vscdb"
	default:
		return "file"
	}
}

// Writable says this account's store can be replaced in place.
func (a Account) Writable() bool { return a.ReadOnlyReason() == "" }

// ReadOnlyReason says why this account's live store cannot be replaced, ""
// when it can. A provider may keep its sign-in somewhere this app does not
// write even though a file exists (Codex's keyring/auto store).
func (a Account) ReadOnlyReason() string {
	switch a.Provider {
	case "claude", "codex", "antigravity", "grok":
		if a.CredentialKind() != "file" && a.CredentialKind() != "keychain" {
			if a.CredentialKind() == "vscdb" {
				return "Antigravity keeps this sign-in in its database; switching is not supported here"
			}
			return "this account store is read-only"
		}
		if a.Provider == "codex" {
			return codexStoreReason()
		}
		return ""
	}
	return "this agent's accounts can't be switched"
}

// LoginNote says why the live document is not a switchable account while
// its store is still writable (Codex API-key mode, a signed-out file). It
// is nil for a real account.
func (a Account) LoginNote() string {
	d, err := loadAccount(a)
	if err != nil {
		return ""
	}
	switch a.Provider {
	case "codex":
		switch codexLoginMode(d) {
		case "apikey":
			return "Codex is in API-key mode; there is no account to switch"
		case "none":
			return "Codex is not signed in"
		}
	case "claude":
		if str(at(d, "claudeAiOauth", "accessToken")) == "" && str(at(d, "claudeAiOauth", "refreshToken")) == "" {
			return "Claude Code is not signed in"
		}
	}
	return ""
}

func codexLoginMode(d object) string {
	if str(d["auth_mode"]) == "apikey" {
		return "apikey"
	}
	tokens := obj(d["tokens"])
	if tokens == nil {
		tokens = d
	}
	if str(tokens["access_token"]) != "" || str(tokens["refresh_token"]) != "" {
		return "oauth"
	}
	if str(d["OPENAI_API_KEY"]) != "" {
		return "apikey"
	}
	return "none"
}

// codexStoreReason reads where Codex keeps its sign-in. We only write
// auth.json; keyring and auto modes may be read from a keychain or Codex's
// encrypted storage instead.
func codexStoreReason() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = homePath(".codex")
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	mode, secrets := "", false
	inFeatures := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(line, "[") {
			inFeatures = line == "[features]"
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"`)
		switch {
		case key == "cli_auth_credentials_store":
			mode = value
		case inFeatures && key == "secret_auth_storage":
			secrets = value == "true"
		}
	}
	if mode == "" || mode == "file" {
		return ""
	}
	if mode == "ephemeral" {
		return "Codex keeps this sign-in only in its own memory"
	}
	if mode == "keyring" || mode == "auto" || secrets {
		return "Codex keeps its sign-in in the system keychain or encrypted storage; switching auth.json isn't supported here"
	}
	return ""
}

// WriteCredential replaces the account's live credential document.
func (a Account) WriteCredential(d object) error {
	switch {
	case strings.HasPrefix(a.source, "keychain:"):
		return keyWriteDoc(a.Provider, strings.TrimPrefix(a.source, "keychain:"), d)
	case strings.HasPrefix(a.source, "stored:"), filepath.Ext(a.source) == ".vscdb":
		return errors.New("this account store is read-only")
	default:
		return atomicWrite(a.source, d)
	}
}
