// Package accounts keeps the local account profiles this app can switch
// between. Credentials are copied only when the user enables switching;
// the live agent store is always the source of the active account.
package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/credentialid"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

// ErrDisabled means account switching was not enabled; the app must stay
// read-only.
var ErrDisabled = errors.New("account switching is off")

// Profile is one saved account. The credential itself is kept beside the
// manifest, never in it.
type Profile struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Label          string `json:"label"`
	Email          string `json:"email,omitempty"`
	StableID       string `json:"stable_id,omitempty"`
	Plan           string `json:"plan,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Name           string `json:"name,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	LastUsedAt     int64  `json:"last_used_at"`
	ModifiedAt     int64  `json:"credential_modified_at,omitempty"`
	NameModifiedAt int64  `json:"name_modified_at,omitempty"`
	Imported       bool   `json:"imported_backup,omitempty"`
	Credentials    string `json:"-"` // last saved document hash; sync credTime advances only when this changes
	Active         bool   `json:"-"`
	Writable       bool   `json:"-"`
	Reason         string `json:"-"`
	Hidden         bool   `json:"-"`
}

// DisplayName is the user's rename, else the detected label.
func (p Profile) DisplayName() string {
	if strings.TrimSpace(p.Name) != "" {
		return p.Name
	}
	return p.Label
}

type manifest struct {
	Version  int              `json:"version"`
	Enabled  bool             `json:"enabled"`
	Profiles []Profile        `json:"profiles"`
	Deleted  map[string]int64 `json:"deleted_accounts,omitempty"`
}

type journalFile struct {
	Version    int    `json:"version"`
	Provider   string `json:"provider"`
	Target     string `json:"target"`
	BeforeHash string `json:"before_hash"`
	TargetHash string `json:"target_hash"`
	At         int64  `json:"at"`
}

// Store owns the manifest and credential copies. One process-wide lock
// serializes reconciliation and switches.
type Store struct {
	mu     sync.Mutex
	dir    string
	loaded bool
	data   manifest
	active map[string]string
	// suspect holds provider/ID keys of records that carry the live
	// sign-in's tokens under another account's name; they cannot switch.
	suspect map[string]bool
}

// Open returns a store rooted at the app config directory.
func Open() *Store {
	d, _ := os.UserConfigDir()
	return &Store{dir: filepath.Join(d, "devin-usage-metrics"), active: map[string]string{}, suspect: map[string]bool{}}
}

func (s *Store) manifestPath() string { return filepath.Join(s.dir, "accounts.json") }
func (s *Store) journalPath() string  { return filepath.Join(s.dir, "accounts-switch.json") }
func (s *Store) credentialPath(provider, id string) string {
	return filepath.Join(s.dir, "accounts", provider, id+".json")
}

// CredentialPath returns the path to a saved profile's credentials.
func (s *Store) CredentialPath(provider, id string) string {
	return s.credentialPath(provider, id)
}

func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	b, err := os.ReadFile(s.manifestPath())
	if err != nil {
		return
	}
	var data manifest
	if json.Unmarshal(b, &data) == nil && data.Version == 1 {
		s.data = data
	}
}

// Enabled reports whether the user opted in to writing account credentials.
func (s *Store) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.data.Enabled
}

// SetEnabled turns switching on or off. Turning it off keeps the saved
// profiles; it only stops future captures and switches.
func (s *Store) SetEnabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.data.Version = 1
	s.data.Enabled = on
	return s.saveLocked()
}

// Add stores a credential document obtained from an in-app sign-in. The
// current live store is not touched.
func (s *Store) Add(provider string, doc map[string]any, kind string) (Profile, error) {
	if !supported(provider) {
		return Profile{}, fmt.Errorf("%s accounts can't be switched", provider)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if !s.data.Enabled {
		return Profile{}, ErrDisabled
	}
	if !accountDocument(provider, doc) {
		return Profile{}, errors.New("this is not a switchable account")
	}
	hash := docHash(doc)
	p, err := s.upsertLocked(provider, doc, kind, hash)
	if err != nil {
		return Profile{}, err
	}
	entry := BackupEntry{Provider: provider, Credential: doc}
	key := syncIdentity(entry)
	if key == "" {
		key = backupIdentity(entry)
	}
	if deletedAt := s.data.Deleted[key]; deletedAt > 0 {
		p.ModifiedAt = max(time.Now().UnixNano(), deletedAt+1)
		for i := range s.data.Profiles {
			if s.data.Profiles[i].ID == p.ID && s.data.Profiles[i].Provider == provider {
				s.data.Profiles[i].ModifiedAt = p.ModifiedAt
			}
		}
		stamp := time.Unix(0, p.ModifiedAt)
		if err := os.Chtimes(s.credentialPath(provider, p.ID), stamp, stamp); err != nil {
			return Profile{}, err
		}
		delete(s.data.Deleted, key)
	}
	if err := s.saveLocked(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// List returns saved profiles with the active flag from the last
// reconciliation. It does not touch agent credentials.
func (s *Store) List() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.repairLocked() {
		_ = s.saveLocked()
	}
	return s.decorateLocked(append([]Profile(nil), s.data.Profiles...))
}

// decorateLocked fills the display fields of a profile copy and retires
// switchability from records whose saved copy is gone: without the file such
// a record can only fail a switch, and the failure must not look like a
// working button.
func (s *Store) decorateLocked(out []Profile) []Profile {
	for i := range out {
		out[i].Active = out[i].ID == s.active[out[i].Provider]
		out[i].Writable, out[i].Reason = writable(out[i])
		if out[i].Writable {
			if _, err := os.Stat(s.credentialPath(out[i].Provider, out[i].ID)); err != nil {
				out[i].Writable = false
				out[i].Reason = "This account has no saved sign-in on this device"
			} else if s.suspect[out[i].Provider+"/"+out[i].ID] {
				out[i].Writable = false
				out[i].Reason = "This copy holds another account's sign-in; sign in again to refresh it"
			}
		}
	}
	sortProfiles(out)
	return out
}

// flagContaminatedLocked marks records that hold the live sign-in's tokens
// while claiming another account. Reconcile just filed the live login under
// the active record, so that record is the authority here; a record carrying
// the same tokens under a different name is a stale copy that would only
// switch back to the account it copied, which reads as "switching does
// nothing".
func (s *Store) flagContaminatedLocked(provider string, doc map[string]any) {
	for key := range s.suspect {
		if strings.HasPrefix(key, provider+"/") {
			delete(s.suspect, key)
		}
	}
	p := s.activeProfileLocked(provider)
	if p == nil || p.StableID == "" {
		return
	}
	saved, err := s.readCredential(*p)
	if err != nil {
		return
	}
	_, liveRefresh := tokenPair(provider, doc)
	_, anchorRefresh := tokenPair(provider, saved)
	if liveRefresh == "" || liveRefresh != anchorRefresh {
		return
	}
	for i := range s.data.Profiles {
		q := &s.data.Profiles[i]
		if q.Provider != provider || q.ID == p.ID || q.StableID == "" || q.StableID == p.StableID {
			continue
		}
		qDoc, err := s.readCredential(*q)
		if err != nil {
			continue
		}
		if _, qRefresh := tokenPair(provider, qDoc); qRefresh == liveRefresh {
			s.suspect[provider+"/"+q.ID] = true
		}
	}
}

// Reconcile reads each live account and, when switching is enabled,
// captures it into its profile. It also finishes an interrupted switch.
// This is the only path that writes credentials on a normal refresh.
func (s *Store) Reconcile(live []quota.Account) ([]Profile, error) {
	return s.ReconcileContext(context.Background(), live)
}

// ReconcileContext is Reconcile with a caller's cancellation and timeout.
func (s *Store) ReconcileContext(ctx context.Context, live []quota.Account) ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	changed := s.repairLocked()
	seen := map[string]bool{}
	for _, a := range live {
		if !supported(a.Provider) || seen[a.Provider] {
			continue
		}
		seen[a.Provider] = true
		doc, err := a.CredentialDoc()
		if err != nil {
			continue
		}
		if !accountDocument(a.Provider, doc) {
			if a.Provider == "codex" {
				delete(s.active, a.Provider)
			}
			continue
		}
		if credentialid.Of(a.Provider, doc).Email == "" {
			// The active profile, the agent's own label or our saved copy
			// may name the account even when the credential has no identity
			// claims. Keep the email in our copy only; a switch strips it.
			if a.Provider == "claude" {
				if email := claudeProfileEmail(); email != "" {
					doc["email"] = email
				}
			}
			if credentialid.Of(a.Provider, doc).Email == "" {
				// The active profile may name the document's account only
				// when the two actually share tokens: after a failed
				// switch the live store can hold the previous login, and
				// stamping the selected account's name onto it rewrote
				// that account's saved sign-in with the old one.
				if p := s.activeProfileLocked(a.Provider); p != nil && p.Email != "" && s.sameSavedLocked(*p, a.Provider, doc) {
					doc["email"] = p.Email
				} else if email := s.savedEmailLocked(a.Provider, doc); email != "" {
					doc["email"] = email
				} else if a.Provider == "antigravity" {
					// Antigravity's agent-facing label is its UI's auth
					// status, which runs ahead of or behind the token row
					// while an account switch settles; stamping it here
					// filed tokens under the wrong account. Ask Google
					// whom the access token belongs to instead.
					if email := googleAccountEmail(ctx, accessTokenOf(doc)); email != "" {
						doc["email"] = email
					}
				} else if strings.Contains(a.Label, "@") {
					doc["email"] = a.Label
				}
			}
		}
		kind := a.CredentialKind()
		hash := docHash(doc)
		// A normal quota refresh must not recreate an account removed by sync.
		// Signing in/capturing explicitly goes through Add and restores it.
		entry := BackupEntry{Provider: a.Provider, Credential: doc}
		key := syncIdentity(entry)
		if key == "" {
			key = backupIdentity(entry)
		}
		if s.data.Deleted[key] > 0 {
			for _, p := range s.data.Profiles {
				if p.Provider != a.Provider {
					continue
				}
				if saved, err := s.readCredential(p); err == nil && backupIdentity(BackupEntry{Provider: p.Provider, Credential: saved}) == key {
					s.active[a.Provider] = p.ID
					break
				}
			}
			continue
		}
		if s.recoverLocked(a.Provider, doc, hash) {
			changed = true
		}
		captured := ""
		if s.data.Enabled && a.Writable() {
			if s.unattributableLocked(a.Provider, doc) {
				// The document carries no identity and shares no token with
				// the named active profile: it is the leftover of a different
				// login (a dying client writing its previous sign-in back
				// after a switch), not a refresh of the selected account.
				// Capturing it here replaced the active profile's saved
				// sign-in with the other account's tokens.
			} else if p, err := s.upsertLocked(a.Provider, doc, kind, hash); err == nil {
				captured = p.ID
				changed = true
			}
		}
		id := s.matchLocked(a.Provider, doc, hash)
		if id == "" {
			id = captured
		}
		if id != "" {
			if s.active[a.Provider] != id {
				s.active[a.Provider] = id
				changed = true
			}
			for i := range s.data.Profiles {
				if s.data.Profiles[i].ID == id {
					s.data.Profiles[i].LastUsedAt = time.Now().Unix()
				}
			}
		}
		s.flagContaminatedLocked(a.Provider, doc)
	}
	for _, provider := range []string{"claude", "codex", "antigravity", "grok"} {
		if s.hideLegacyDuplicatesLocked(provider) {
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
	return s.decorateLocked(append([]Profile(nil), s.data.Profiles...)), nil
}

// Switch moves the target profile's credential into the live store. The
// current live credential is captured first, and a journal lets the next
// refresh finish or discard an interrupted switch.
func (s *Store) Switch(provider, id string, live quota.Account) error {
	if !supported(provider) {
		return fmt.Errorf("%s accounts can't be switched", provider)
	}
	if !live.Writable() {
		return errors.New("the agent's sign-in store is read-only here")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if !s.data.Enabled {
		return ErrDisabled
	}
	var target *Profile
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == provider && s.data.Profiles[i].ID == id {
			target = &s.data.Profiles[i]
			break
		}
	}
	if target == nil {
		return errors.New("account not found")
	}
	if ok, reason := writable(*target); !ok {
		return errors.New(reason)
	}
	before, err := live.CredentialDoc()
	if err != nil {
		return fmt.Errorf("read the current sign-in: %w", err)
	}
	beforeHash := docHash(before)
	// An API-key mode or a signed-out store is not an account; do not
	// capture it, but it is still a writable place to put the target.
	if accountDocument(provider, before) {
		if _, err := s.upsertLocked(provider, before, live.CredentialKind(), beforeHash); err != nil {
			return err
		}
		if err := s.saveLocked(); err != nil {
			return err
		}
	}
	if s.active[provider] == id {
		return nil
	}
	targetDoc, err := s.readCredential(*target)
	if err != nil {
		return errors.New("this account has no saved sign-in; sign in again")
	}
	if !accountDocument(provider, targetDoc) {
		return errors.New("this profile is not a switchable account; sign in again")
	}
	if id := credentialid.Of(provider, targetDoc); target.StableID != "" && id.Stable != "" && id.Stable != target.StableID {
		return errors.New("the saved sign-in belongs to a different account; sign in again")
	}
	// A copy whose tokens already are the live sign-in cannot deliver this
	// switch when another record holding those same tokens claims a
	// different account: the write would succeed and only restore the
	// account it copied, which reads as "switching does not work".
	if _, liveRefresh := tokenPair(provider, before); liveRefresh != "" {
		if _, targetRefresh := tokenPair(provider, targetDoc); targetRefresh == liveRefresh {
			if id := credentialid.Of(provider, targetDoc); id.Stable != "" {
				for _, p := range s.data.Profiles {
					if p.Provider != provider || p.StableID == "" || p.StableID == id.Stable {
						continue
					}
					saved, err := s.readCredential(p)
					if err != nil {
						continue
					}
					if _, savedRefresh := tokenPair(provider, saved); savedRefresh == liveRefresh {
						return errors.New("this saved copy holds the current account's sign-in; sign in again to refresh it")
					}
				}
			}
		}
	}
	targetDoc = switchDocument(provider, targetDoc)
	targetHash := docHash(targetDoc)
	j := journalFile{Version: 1, Provider: provider, Target: id, BeforeHash: beforeHash, TargetHash: targetHash, At: time.Now().Unix()}
	targetDoc = adaptCredential(provider, targetDoc, target.Kind, live.CredentialKind())
	targetHash = docHash(targetDoc)
	j.TargetHash = targetHash
	if err := s.writeJournal(j); err != nil {
		return err
	}
	// Compare-and-swap right before the write: the agent may have
	// refreshed or another login may have replaced the document while the
	// target was being read.
	if current, err := live.CredentialDoc(); err != nil || docHash(current) != beforeHash {
		_ = os.Remove(s.journalPath())
		return errors.New("the live sign-in changed while switching; try again")
	}
	if err := live.WriteCredential(targetDoc); err != nil {
		// Put the previous login back so a failed write does not leave a
		// half-switched account. A failed restore keeps the journal.
		if live.WriteCredential(before) == nil {
			_ = os.Remove(s.journalPath())
		}
		return fmt.Errorf("write the account sign-in: %w", err)
	}
	after, err := live.CredentialDoc()
	if err != nil || readBackHash(provider, live.CredentialKind(), after) != readBackHash(provider, live.CredentialKind(), targetDoc) {
		return errors.New("the sign-in did not read back as the selected account")
	}
	_ = os.Remove(s.journalPath())
	target.LastUsedAt = time.Now().Unix()
	s.active[provider] = id
	return s.saveLocked()
}

// Reapply writes the target profile again without capturing the live
// document. Hot-switching Antigravity kills its language server and needs
// the new credential to win any write from the old child process.
func (s *Store) Reapply(provider, id string, live quota.Account) error {
	if !live.Writable() {
		return errors.New("the agent's sign-in store is read-only here")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	var target *Profile
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == provider && s.data.Profiles[i].ID == id {
			target = &s.data.Profiles[i]
			break
		}
	}
	if target == nil {
		return errors.New("account not found")
	}
	doc, err := s.readCredential(*target)
	if err != nil {
		return errors.New("this account has no saved sign-in; sign in again")
	}
	if !accountDocument(provider, doc) {
		return errors.New("this profile is not a switchable account; sign in again")
	}
	doc = switchDocument(provider, doc)
	doc = adaptCredential(provider, doc, target.Kind, live.CredentialKind())
	if err := live.WriteCredential(doc); err != nil {
		return err
	}
	after, err := live.CredentialDoc()
	if err != nil || readBackHash(provider, live.CredentialKind(), after) != readBackHash(provider, live.CredentialKind(), doc) {
		return errors.New("the sign-in did not read back as the selected account")
	}
	target.LastUsedAt = time.Now().Unix()
	s.active[provider] = id
	return s.saveLocked()
}

// ProfileCredential is a copy of a profile's saved credential for the
// switch path; callers must not log it.
func (s *Store) ProfileCredential(provider, id string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for _, p := range s.data.Profiles {
		if p.Provider == provider && p.ID == id {
			return s.readCredential(p)
		}
	}
	return nil, errors.New("account not found")
}

// ProfileEmail is the saved address for a profile, "" when unknown.
func (s *Store) ProfileEmail(provider, id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for _, p := range s.data.Profiles {
		if p.Provider == provider && p.ID == id {
			return p.Email
		}
	}
	return ""
}

func (s *Store) Rename(provider, id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == provider && s.data.Profiles[i].ID == id {
			s.data.Profiles[i].Name = strings.TrimSpace(name)
			s.data.Profiles[i].NameModifiedAt = time.Now().UnixNano()
			return s.saveLocked()
		}
	}
	return errors.New("account not found")
}

// Remove deletes this app's copy and metadata. It never signs the agent out
// or revokes anything upstream.
func (s *Store) Remove(provider, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for i := range s.data.Profiles {
		p := s.data.Profiles[i]
		if p.Provider == provider && p.ID == id {
			if s.active[provider] == id {
				return errors.New("switch to another account before removing the active one")
			}
			s.suppressBackupLocked(p)
			_ = os.Remove(s.credentialPath(provider, id))
			s.data.Profiles = append(s.data.Profiles[:i], s.data.Profiles[i+1:]...)
			return s.saveLocked()
		}
	}
	return errors.New("account not found")
}

// RemoveAll deletes every saved credential copy. It does not touch the
// agents' live stores.
func (s *Store) RemoveAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for _, p := range s.data.Profiles {
		s.suppressBackupLocked(p)
		_ = os.Remove(s.credentialPath(p.Provider, p.ID))
	}
	s.data.Profiles = nil
	s.active = map[string]string{}
	return s.saveLocked()
}

func (s *Store) activeProfileLocked(provider string) *Profile {
	id := s.active[provider]
	if id == "" {
		return nil
	}
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == provider && s.data.Profiles[i].ID == id {
			return &s.data.Profiles[i]
		}
	}
	return nil
}

// unattributableLocked reports whether a credential document without
// identity claims is known to belong to a different login than the active
// profile. The active profile naming its account makes the document a
// leftover of another sign-in (see ReconcileContext); a profile without
// identity cannot tell, and the capture keeps working as before.
func (s *Store) unattributableLocked(provider string, doc map[string]any) bool {
	if id := credentialid.Of(provider, doc); id.Stable != "" || id.Email != "" {
		return false
	}
	p := s.activeProfileLocked(provider)
	if p == nil || (p.StableID == "" && p.Email == "") {
		return false
	}
	return !s.sameSavedLocked(*p, provider, doc)
}

// sameAccountClaims says a credential document's identity claims do not
// contradict a profile's own. A document without claims stays compatible
// only with a profile without them: once both name an account, the names
// must agree.
func sameAccountClaims(id credentialid.Identity, p Profile) bool {
	if id.Stable == "" && id.Email == "" {
		return p.StableID == "" && p.Email == ""
	}
	if id.Email != "" && p.Email != "" && !strings.EqualFold(id.Email, p.Email) {
		return false
	}
	if id.Stable != "" && p.StableID != "" && id.Stable != p.StableID &&
		!strings.HasPrefix(p.StableID, id.Stable+"|") && !strings.HasPrefix(id.Stable, p.StableID+"|") {
		return false
	}
	return true
}

func (s *Store) savedEmailLocked(provider string, doc map[string]any) string {
	for _, p := range s.data.Profiles {
		if p.Provider != provider || p.Email == "" {
			continue
		}
		if s.sameSavedLocked(p, provider, doc) {
			return p.Email
		}
	}
	return ""
}

func accessTokenOf(doc map[string]any) string {
	t := obj(doc["token"])
	if t == nil {
		t = doc
	}
	return str(t["access_token"])
}

// sameSavedLocked recognizes a profile saved before its identity claims
// were read correctly by the tokens it still shares with the live login.
func (s *Store) sameSavedLocked(p Profile, provider string, doc map[string]any) bool {
	if p.Provider != provider {
		return false
	}
	saved, err := s.readCredential(p)
	if err != nil {
		return false
	}
	access, refresh := tokenPair(provider, doc)
	savedAccess, savedRefresh := tokenPair(provider, saved)
	return (access != "" && access == savedAccess) || (refresh != "" && refresh == savedRefresh)
}

func tokenPair(provider string, doc map[string]any) (access, refresh string) {
	switch provider {
	case "claude":
		c := obj(doc["claudeAiOauth"])
		return str(c["accessToken"]), str(c["refreshToken"])
	case "codex":
		t := obj(doc["tokens"])
		if t == nil {
			t = doc
		}
		return str(t["access_token"]), str(t["refresh_token"])
	case "antigravity":
		t := obj(doc["token"])
		if t == nil {
			t = doc
		}
		return str(t["access_token"]), str(t["refresh_token"])
	case "grok":
		for _, v := range doc {
			entry := obj(v)
			if entry != nil && entry["refresh_token"] != nil {
				return str(entry["key"]), str(entry["refresh_token"])
			}
		}
	}
	return "", ""
}

// repairLocked fills identity metadata captured before Antigravity's
// keychain wrapper was read correctly, and merges duplicate records for the
// same stable account.
func (s *Store) repairLocked() bool {
	changed := false
	for i := range s.data.Profiles {
		p := &s.data.Profiles[i]
		p.Hidden = false
		doc, err := s.readCredential(*p)
		if err != nil {
			continue
		}
		if !accountDocument(p.Provider, doc) {
			// Keep the file: an API-key or signed-out record is hidden,
			// not destroyed. Deleting it here lost real sign-ins.
			if !p.Hidden {
				p.Hidden, changed = true, true
			}
			continue
		}
		id := credentialid.Of(p.Provider, doc)
		if id.Stable == "" {
			continue
		}
		if p.StableID != id.Stable {
			p.StableID, changed = id.Stable, true
		}
		if id.Email != "" && !strings.EqualFold(p.Email, id.Email) {
			p.Email, changed = id.Email, true
		}
		if p.Name == "" {
			if label := profileLabel(p.Provider, id, p.ID); label != p.Label {
				p.Label, changed = label, true
			}
		}
	}
	for _, provider := range []string{"claude", "codex", "antigravity", "grok"} {
		if s.hideLegacyDuplicatesLocked(provider) {
			changed = true
		}
	}
	if s.healMissingCredentialsLocked() {
		changed = true
	}
	return changed
}

// healMissingCredentialsLocked rebuilds a record whose credential file went
// missing (a crashed write or an interrupted removal) from a sibling record
// of the same account whose copy survived. Without this the record keeps a
// switch button whose only possible outcome is "no saved sign-in".
func (s *Store) healMissingCredentialsLocked() bool {
	changed := false
	for i := range s.data.Profiles {
		p := &s.data.Profiles[i]
		if !supported(p.Provider) || p.StableID == "" {
			continue
		}
		if _, err := os.Stat(s.credentialPath(p.Provider, p.ID)); err == nil {
			continue
		}
		for j := range s.data.Profiles {
			q := &s.data.Profiles[j]
			if i == j || q.Provider != p.Provider || q.StableID != p.StableID {
				continue
			}
			doc, err := s.readCredential(*q)
			if err != nil || !accountDocument(p.Provider, doc) {
				continue
			}
			if err := s.writeCredential(*p, doc); err != nil {
				continue
			}
			p.Credentials = docHash(doc)
			changed = true
			break
		}
	}
	return changed
}

// hideLegacyDuplicatesLocked hides extra copies of the active login once it
// has an identity. A copy counts as a duplicate only when its saved tokens
// match the active profile's; saved accounts that are merely unnamed stay
// visible instead of disappearing from the list, and broken files stay
// visible so the user can remove them. Files stay on disk either way.
func (s *Store) hideLegacyDuplicatesLocked(provider string) bool {
	active := s.active[provider]
	if active == "" {
		return false
	}
	var current *Profile
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == provider && s.data.Profiles[i].ID == active {
			current = &s.data.Profiles[i]
			break
		}
	}
	if current == nil || (current.Email == "" && current.StableID == "") {
		return false
	}
	currentDoc, err := s.readCredential(*current)
	if err != nil {
		return false
	}
	changed := false
	for i := range s.data.Profiles {
		p := &s.data.Profiles[i]
		if p.Provider != provider || p.ID == active || p.Hidden {
			continue
		}
		if p.StableID != "" || p.Email != "" {
			continue
		}
		doc, err := s.readCredential(*p)
		if err != nil {
			continue
		}
		if sameSavedTokens(provider, currentDoc, doc) {
			p.Hidden, changed = true, true
		}
	}
	return changed
}

// sameSavedTokens reports whether two saved documents carry the same login
// by their token pair alone, without naming either profile.
func sameSavedTokens(provider string, a, b map[string]any) bool {
	access, refresh := tokenPair(provider, a)
	otherAccess, otherRefresh := tokenPair(provider, b)
	return (access != "" && access == otherAccess) || (refresh != "" && refresh == otherRefresh)
}

func (s *Store) upsertLocked(provider string, doc map[string]any, kind, hash string) (Profile, error) {
	id := credentialid.Of(provider, doc)
	pid := profileID(provider, id, doc, hash)
	now := time.Now().Unix()
	for i := range s.data.Profiles {
		p := &s.data.Profiles[i]
		if p.Provider != provider {
			continue
		}
		sameID := p.ID == pid
		sameStable := id.Stable != "" && p.StableID == id.Stable
		sameTokens := !sameID && !sameStable && s.sameSavedLocked(*p, provider, doc)
		// A legacy email-only identity may match an email|sub profile, but
		// never a different known account (Codex workspace, Claude org).
		sameEmail := !sameID && !sameStable && !sameTokens && provider == "antigravity" &&
			id.Email != "" && strings.EqualFold(p.Email, id.Email) &&
			(!strings.Contains(id.Stable, "|") || !strings.Contains(p.StableID, "|"))
		// The active-profile fallback never rewrites a profile with a
		// document of a different login: an unidentified document must not
		// replace the saved sign-in of a named account, and a named one
		// must not file itself under a differently named profile. Either
		// merge replaced the account the user just switched to.
		sameActive := !sameID && !sameStable && !sameTokens && !sameEmail && s.active[provider] == p.ID &&
			sameAccountClaims(id, *p) &&
			(id.Stable == "" || p.StableID == "" || (!strings.Contains(id.Stable, "|") && strings.HasPrefix(p.StableID, id.Stable+"|")))
		if !sameID && !sameStable && !sameTokens && !sameEmail && !sameActive {
			continue
		}
		p.Kind = kind
		if p.Credentials != hash {
			if p.Credentials != "" {
				p.ModifiedAt = time.Now().UnixNano()
			}
			p.Credentials = hash
		}
		if id.Email != "" && !strings.EqualFold(p.Email, id.Email) {
			p.Email = id.Email
		}
		if p.Name == "" && id.Stable != "" {
			p.Label = profileLabel(provider, id, p.ID)
		}
		if id.Plan != "" {
			p.Plan = id.Plan
		}
		if id.Stable != "" {
			p.StableID = id.Stable
		}
		if p.CreatedAt == 0 {
			p.CreatedAt = now
		}
		if err := s.writeCredential(*p, doc); err != nil {
			return Profile{}, err
		}
		return *p, nil
	}
	p := Profile{
		ID: pid, Provider: provider, Label: profileLabel(provider, id, pid),
		Email: id.Email, StableID: id.Stable, Plan: id.Plan, Kind: kind,
		CreatedAt: now, ModifiedAt: time.Now().UnixNano(), Credentials: hash,
	}
	if err := s.writeCredential(p, doc); err != nil {
		return Profile{}, err
	}
	s.data.Profiles = append(s.data.Profiles, p)
	return p, nil
}

func (s *Store) matchLocked(provider string, doc map[string]any, hash string) string {
	id := credentialid.Of(provider, doc)
	for _, p := range s.data.Profiles {
		if p.Provider != provider {
			continue
		}
		if id.Stable != "" && p.StableID == id.Stable {
			return p.ID
		}
		if p.Credentials != "" && p.Credentials == hash {
			return p.ID
		}
	}
	active := s.active[provider]
	if active == "" {
		return ""
	}
	if id.Stable == "" {
		return active
	}
	if !strings.Contains(id.Stable, "|") {
		for _, p := range s.data.Profiles {
			if p.Provider == provider && p.ID == active && strings.HasPrefix(p.StableID, id.Stable+"|") {
				return p.ID
			}
		}
	}
	return ""
}

func (s *Store) matchID(provider string, doc map[string]any, hash string) string {
	id := credentialid.Of(provider, doc)
	for _, p := range s.data.Profiles {
		if p.Provider != provider {
			continue
		}
		if id.Stable != "" && p.StableID == id.Stable {
			return p.ID
		}
		if p.Credentials != "" && p.Credentials == hash {
			return p.ID
		}
	}
	return ""
}

func (s *Store) recoverLocked(provider string, doc map[string]any, hash string) bool {
	b, err := os.ReadFile(s.journalPath())
	if err != nil {
		return false
	}
	var j journalFile
	if json.Unmarshal(b, &j) != nil || j.Provider != provider {
		return false
	}
	if hash == j.TargetHash || s.matchID(provider, doc, hash) == j.Target {
		s.active[provider] = j.Target
		for i := range s.data.Profiles {
			if s.data.Profiles[i].ID == j.Target {
				s.data.Profiles[i].LastUsedAt = time.Now().Unix()
			}
		}
	}
	_ = os.Remove(s.journalPath())
	return true
}

func (s *Store) writeJournal(j journalFile) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(s.journalPath(), append(b, '\n'))
}

func (s *Store) readCredential(p Profile) (map[string]any, error) {
	b, err := os.ReadFile(s.credentialPath(p.Provider, p.ID))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return nil, errors.New("saved sign-in is unreadable")
	}
	return doc, nil
}

func (s *Store) writeCredential(p Profile, doc map[string]any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	// Reconciliation reads the same credential repeatedly. Preserve the
	// modification time so backups cannot make an old token look newer.
	if old, err := s.readCredential(p); err == nil && docHash(old) == docHash(doc) {
		return nil
	}
	if err := writePrivate(s.credentialPath(p.Provider, p.ID), append(b, '\n')); err != nil {
		return err
	}
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == p.Provider && s.data.Profiles[i].ID == p.ID {
			s.data.Profiles[i].ModifiedAt = time.Now().UnixNano()
			break
		}
	}
	return nil
}

func (s *Store) saveLocked() error {
	s.loadLocked()
	s.data.Version = 1
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(s.manifestPath(), append(b, '\n'))
}

func nested(v any, keys ...string) any {
	for _, key := range keys {
		v = obj(v)[key]
	}
	return v
}

func claudeProfileEmail() string {
	path := filepath.Join(os.Getenv("HOME"), ".claude.json")
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		path = filepath.Join(dir, ".claude.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return str(nested(doc, "oauthAccount", "emailAddress"))
}

func profileLabel(provider string, id credentialid.Identity, pid string) string {
	label := credentialid.Label(provider, id)
	if id.Stable == "" && pid != "" {
		short := pid
		if len(short) > 6 {
			short = short[:6]
		}
		label += " (" + short + ")"
	}
	return label
}

func accountDocument(provider string, doc map[string]any) bool {
	switch provider {
	case "codex":
		if str(doc["auth_mode"]) == "apikey" {
			return false
		}
		tokens := obj(doc["tokens"])
		if tokens == nil {
			tokens = doc
		}
		return str(tokens["access_token"]) != "" || str(tokens["refresh_token"]) != ""
	case "claude":
		c := obj(doc["claudeAiOauth"])
		return str(c["accessToken"]) != "" || str(c["refreshToken"]) != ""
	case "antigravity":
		tokens := obj(doc["token"])
		if tokens == nil {
			tokens = doc
		}
		return str(tokens["access_token"]) != "" || str(tokens["refresh_token"]) != ""
	case "grok":
		for _, v := range doc {
			if entry := obj(v); entry != nil && entry["refresh_token"] != nil {
				return true
			}
		}
	}
	return false
}

func profileID(provider string, id credentialid.Identity, doc map[string]any, hash string) string {
	key := id.Stable
	if key == "" {
		key = hash
	}
	sum := sha256.Sum256([]byte(provider + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}

func docHash(doc map[string]any) string {
	b, _ := json.Marshal(doc)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func supported(provider string) bool {
	switch provider {
	case "claude", "codex", "antigravity", "grok":
		return true
	}
	return false
}

func writable(p Profile) (bool, string) {
	if p.Kind == "stored" || p.Kind == "" {
		return false, "This account has no switchable sign-in"
	}
	return true, ""
}

// readBackHash hashes a credential the way its live store reports it after a
// write. Antigravity's state database round-trips only the opaque tokens and
// names the account itself, so the read-back comparison there covers just the
// tokens; everywhere else a written document reads back unchanged.
func readBackHash(provider, kind string, doc map[string]any) string {
	if provider == "antigravity" && kind == "vscdb" {
		token := obj(doc["token"])
		if token == nil {
			token = doc
		}
		out := map[string]any{}
		if v := str(token["access_token"]); v != "" {
			out["access_token"] = v
		}
		if v := str(token["refresh_token"]); v != "" {
			out["refresh_token"] = v
		}
		return docHash(out)
	}
	return docHash(switchDocument(provider, doc))
}

func sortProfiles(p []Profile) {
	order := map[string]int{"claude": 0, "codex": 1, "antigravity": 2, "grok": 3}
	sort.SliceStable(p, func(i, j int) bool {
		if p[i].Provider != p[j].Provider {
			return order[p[i].Provider] < order[p[j].Provider]
		}
		if p[i].Active != p[j].Active {
			return p[i].Active
		}
		if p[i].LastUsedAt != p[j].LastUsedAt {
			return p[i].LastUsedAt > p[j].LastUsedAt
		}
		return strings.ToLower(p[i].DisplayName()) < strings.ToLower(p[j].DisplayName())
	})
}

func writePrivate(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".accounts-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(name, path)
}

// switchDocument removes fields this app added for naming; switching must
// put back only what the agent itself writes.
func switchDocument(provider string, doc map[string]any) map[string]any {
	if _, ok := doc["email"]; !ok {
		return doc
	}
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		if k != "email" {
			out[k] = v
		}
	}
	return out
}

// adaptCredential converts Antigravity's two credential shapes when a
// profile was captured or signed in against a different store than the
// live one. Claude and Codex use one JSON shape everywhere.
func adaptCredential(provider string, doc map[string]any, from, to string) map[string]any {
	if provider != "antigravity" || from == to {
		return doc
	}
	token := obj(doc["token"])
	if token == nil {
		token = doc
	}
	access := str(token["access_token"])
	refresh := str(token["refresh_token"])
	idToken := str(token["id_token"])
	if idToken == "" {
		idToken = str(doc["id_token"])
	}
	expiry := token["expiry"]
	if expiry == nil {
		expiry = doc["expiry_date"]
	}
	exp, ok := expiryTime(expiry)
	if to == "keychain" {
		var value any = expiry
		if ok {
			value = exp.UTC().Format(time.RFC3339Nano)
		}
		return map[string]any{
			"token": map[string]any{
				"access_token": access, "token_type": "Bearer",
				"refresh_token": refresh, "expiry": value,
			},
			"auth_method": "consumer", "id_token": idToken,
		}
	}
	var value any = expiry
	if ok {
		value = exp.UnixMilli()
	}
	return map[string]any{
		"access_token": access, "token_type": "Bearer",
		"refresh_token": refresh, "expiry_date": value, "id_token": idToken,
	}
}

func expiryTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed, true
			}
		}
	case float64:
		if t > 1e12 {
			return time.UnixMilli(int64(t)), true
		}
		return time.Unix(int64(t), 0), true
	case int64:
		if t > 1e12 {
			return time.UnixMilli(t), true
		}
		return time.Unix(t, 0), true
	case int:
		return time.Unix(int64(t), 0), true
	}
	return time.Time{}, false
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
