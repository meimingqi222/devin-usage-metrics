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
	"sync/atomic"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/credentialid"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

// ErrDisabled means account switching was not enabled; the app must stay
// read-only.
var ErrDisabled = errors.New("account switching is off")

// errLiveChanged and errWriteFailed are the switch failures a plain rewrite
// may finish: the live store moved under the compare-and-swap, or the write
// itself failed. Every other Switch error is a refusal that must stand.
var (
	errLiveChanged = errors.New("the live sign-in changed while switching; try again")
	errWriteFailed = errors.New("write the account sign-in")
)

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
	// enabled mirrors data.Enabled once loaded, so the UI thread can read
	// it every frame without waiting on a refresh or switch holding mu.
	enabled, enabledReady atomic.Bool
	// reconciled is set once a refresh has bound the live logins; before
	// that, which saved records are in use is unknown.
	reconciled atomic.Bool
}

// Reconciled reports whether a refresh has bound the agents' live logins in
// this process. Until then the in-use records are unknown, and removing
// "every account but the ones in use" would remove those too.
func (s *Store) Reconciled() bool { return s.reconciled.Load() }

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
	defer func() {
		s.enabled.Store(s.data.Enabled)
		s.enabledReady.Store(true)
	}()
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
	if s.enabledReady.Load() {
		return s.enabled.Load()
	}
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
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.enabled.Store(on)
	return nil
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
	if err := s.clearTombstoneLocked(provider, key, &p); err != nil {
		return Profile{}, err
	}
	// A fresh sign-in is the repair the suspect message asks for: the copy
	// now holds new tokens. The next repair re-flags it if they are still
	// shared with another record.
	delete(s.suspect, provider+"/"+p.ID)
	if err := s.saveLocked(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// clearTombstoneLocked drops the deletion record for a key the live login
// still uses, and stamps the profile newer than the record so the next sync
// does not retire the account again. The credential file's modification time
// follows, since backups compare it.
func (s *Store) clearTombstoneLocked(provider, key string, p *Profile) error {
	deletedAt := s.data.Deleted[key]
	if deletedAt == 0 {
		return nil
	}
	p.ModifiedAt = max(time.Now().UnixNano(), deletedAt+1)
	for i := range s.data.Profiles {
		if s.data.Profiles[i].ID == p.ID && s.data.Profiles[i].Provider == provider {
			s.data.Profiles[i].ModifiedAt = p.ModifiedAt
		}
	}
	stamp := time.Unix(0, p.ModifiedAt)
	if err := os.Chtimes(s.credentialPath(provider, p.ID), stamp, stamp); err != nil {
		return err
	}
	delete(s.data.Deleted, key)
	return nil
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

// flagCrossedCopiesLocked marks a record whose saved sign-in is a token copy
// of a sibling's while its record claims a different account. The live-login
// comparison in flagContaminatedLocked only catches a copy of the account
// currently signed in; a crossed copy of another saved login looks normal to
// it, stays listed, and answers quota for the copied account under the wrong
// name. Two records sharing a token pair hold one login, so exactly one of
// the recorded names is right; the side with weaker evidence is the copy.
// The active record is exempt: it just read the live login back and is the
// authority for the token pair it shares. Both sides unknowable or equally
// evidenced stay visible - a wrong quarantine is worse than a duplicate.
func (s *Store) flagCrossedCopiesLocked(provider string) {
	type candidate struct {
		index int
		doc   map[string]any
		id    credentialid.Identity // claims the agent or issuer wrote
	}
	var candidates []candidate
	for i := range s.data.Profiles {
		p := &s.data.Profiles[i]
		if p.Provider != provider || p.Hidden {
			continue
		}
		doc, err := s.readCredential(*p)
		if err != nil || !accountDocument(provider, doc) {
			continue
		}
		if p.StableID == "" && p.Email == "" {
			continue
		}
		candidates = append(candidates, candidate{index: i, doc: doc, id: credentialid.Claims(provider, doc)})
	}
	for a := 0; a < len(candidates); a++ {
		for b := a + 1; b < len(candidates); b++ {
			ca, cb := candidates[a], candidates[b]
			if !sameSavedTokens(provider, ca.doc, cb.doc) {
				continue
			}
			pa, pb := &s.data.Profiles[ca.index], &s.data.Profiles[cb.index]
			if !crossedNameConflict(*pa, *pb) {
				continue
			}
			la, lb := crossedLevel(*pa, ca.id), crossedLevel(*pb, cb.id)
			loser := -1
			switch {
			case la != lb:
				// The weaker name is the copy; a claims-only edge over the
				// active record does not outrank it, so the active side is
				// exempt either way.
				loser = ca.index
				if lb < la {
					loser = cb.index
				}
			case s.active[provider] == pa.ID && s.active[provider] != pb.ID:
				// Equal evidence, but only one side just read the live login
				// back: the other carries that sign-in under a wrong name.
				loser = cb.index
			case s.active[provider] == pb.ID && s.active[provider] != pa.ID:
				loser = ca.index
			}
			if loser >= 0 && s.active[provider] != s.data.Profiles[loser].ID {
				s.suspect[provider+"/"+s.data.Profiles[loser].ID] = true
			}
		}
	}
}

// crossedLevel ranks how firmly a record's name is backed: claims the agent
// or issuer wrote outrank a stable key of the richer email|sub form (which
// could only come from claims), which outranks a bare recorded name. Claims
// contradicting their own record are as unreliable as a stamped name, so
// they leave only the record's form.
func crossedLevel(p Profile, id credentialid.Identity) int {
	if sameAccountClaims(id, p) && (id.Stable != "" || id.Email != "") {
		return 2
	}
	if strings.Contains(p.StableID, "|") {
		return 1
	}
	return 0
}

// crossedNameConflict reports whether two records claim recognizably
// different accounts. An unnamed side cannot be wrong, and an email|sub key
// never conflicts with its own prefix.
func crossedNameConflict(a, b Profile) bool {
	if a.Email != "" && b.Email != "" && !strings.EqualFold(a.Email, b.Email) {
		return true
	}
	if a.StableID == "" || b.StableID == "" || a.StableID == b.StableID {
		return false
	}
	return !strings.HasPrefix(a.StableID, b.StableID+"|") && !strings.HasPrefix(b.StableID, a.StableID+"|")
}

// Reconcile reads each live account and, when switching is enabled,
// captures it into its profile. It also finishes an interrupted switch.
// This is the only path that writes credentials on a normal refresh.
func (s *Store) Reconcile(live []quota.Account) ([]Profile, error) {
	return s.ReconcileContext(context.Background(), live)
}

// ReconcileContext is Reconcile with a caller's cancellation and timeout.
func (s *Store) ReconcileContext(ctx context.Context, live []quota.Account) ([]Profile, error) {
	ctx = syncdiag.WithRun(ctx)
	reads := readLiveAccounts(ctx, live)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	before := len(s.data.Profiles)
	changed := s.repairLocked()
	syncdiag.Log(ctx, "account_reconcile_start", "saved_before", before, "saved_after_repair", len(s.data.Profiles), "repaired", changed, "enabled", s.data.Enabled)
	for _, r := range reads {
		a, doc, err := r.account, r.doc, r.err
		accountCtx := syncdiag.WithFields(ctx, "provider", a.Provider, "source", syncdiag.ID(a.Key), "source_kind", a.CredentialKind())
		previous := s.active[a.Provider]
		if err != nil {
			syncdiag.Log(accountCtx, "account_reconcile_skipped", "reason", "credential_read_failed", "error", syncdiag.ErrorCode(err))
			continue
		}
		if !accountDocument(a.Provider, doc) {
			syncdiag.Log(accountCtx, "account_reconcile_skipped", "reason", "not_switchable_document")
			if a.Provider == "codex" {
				delete(s.active, a.Provider)
			}
			continue
		}
		identitySource := "credential_document"
		if credentialid.Of(a.Provider, doc).Email == "" {
			identitySource = "unresolved"
			// The active profile, the agent's own label or our saved copy
			// may name the account even when the credential has no identity
			// claims. Keep the email in our copy only; a switch strips it.
			if r.issuer != "" {
				// The issuer outranks every recorded name: a crossed
				// saved copy claims one account while holding these
				// tokens, and the auth-status label runs ahead of or
				// behind the token row. readLiveAccounts asked it
				// before the lock was taken.
				doc["email"] = r.issuer
				identitySource = r.issuerSource
			}
			if credentialid.Of(a.Provider, doc).Email == "" {
				// Recorded names stay a guess behind the issuer: the
				// active profile first - the most recent attribution -
				// then any other saved copy sharing the tokens. The
				// agent's label names Antigravity's UI, not its tokens,
				// so it is never used there.
				if email := s.unattestedSavedEmailLocked(a.Provider, doc); email != "" {
					doc["email"] = email
					identitySource = "saved_tokens"
				} else if a.Provider == "claude" {
					if email := claudeProfileEmail(); s.claudeEmailAcceptableLocked(email, doc) {
						doc["email"] = email
						identitySource = "claude_profile"
					} else if strings.Contains(a.Label, "@") && s.claudeEmailAcceptableLocked(a.Label, doc) {
						doc["email"] = a.Label
						identitySource = "agent_label"
					}
				} else if a.Provider != "antigravity" && strings.Contains(a.Label, "@") {
					doc["email"] = a.Label
					identitySource = "agent_label"
				}
			}
		}
		kind := a.CredentialKind()
		hash := docHash(doc)
		// A tombstone stops sync from filing a deleted copy back on every
		// refresh. It must not hide the sign-in this device is actually using:
		// that login is the account, not an imported copy, so a surviving
		// record is bound and the account is captured like any other live
		// login. Add used to be the only way back, which left the manual
		// capture button as the sole repair for an account the user never
		// stopped signing in to.
		entry := BackupEntry{Provider: a.Provider, Credential: doc}
		key := syncIdentity(entry)
		if key == "" {
			key = backupIdentity(entry)
		}
		tombstoned := s.data.Deleted[key] > 0
		bound := ""
		if tombstoned {
			for _, p := range s.data.Profiles {
				if p.Provider != a.Provider {
					continue
				}
				if saved, err := s.readCredential(p); err == nil && profileSyncKey(p, saved) == key {
					bound = p.ID
					break
				}
			}
		}
		if s.recoverLocked(a.Provider, doc, hash) {
			changed = true
		}
		captured := ""
		captureStatus := "disabled"
		// Saving a copy needs no write to the live store, so a read-only
		// store still lists and manages its account; whether the account can
		// be switched to is decided at switch time. Writable used to gate the
		// capture too, which made a read-only live login the one account the
		// manual capture button could add and the refresh could not.
		if s.data.Enabled {
			if s.unattributableLocked(a.Provider, doc) {
				captureStatus = "unattributable"
				// The document carries no identity and shares no token with
				// the named active profile: it is the leftover of a different
				// login (a dying client writing its previous sign-in back
				// after a switch), not a refresh of the selected account.
				// Capturing it here replaced the active profile's saved
				// sign-in with the other account's tokens.
			} else if p, err := s.upsertLocked(a.Provider, doc, kind, hash); err == nil {
				captureStatus = "saved"
				captured = p.ID
				changed = true
				if tombstoned {
					// The live login outranks a removal recorded on
					// another device: drop the tombstone and stamp the
					// profile newer, or the next sync retires the account
					// the user still signs in to.
					if err := s.clearTombstoneLocked(a.Provider, key, &p); err != nil {
						syncdiag.Log(accountCtx, "account_tombstone_clear_failed", "error", syncdiag.ErrorCode(err))
					}
				}
			} else {
				captureStatus = "write_failed"
				syncdiag.Log(accountCtx, "account_capture_failed", "error", syncdiag.ErrorCode(err))
			}
		} else if bound != "" {
			captureStatus = "tombstone_record"
		}
		id, matchReason := s.matchReasonLocked(a.Provider, doc, hash)
		if id == "" {
			id, matchReason = bound, "sync_tombstone_record"
		}
		if id == "" {
			id = captured
			if captured != "" {
				matchReason = "captured_profile"
			}
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
		syncdiag.Log(accountCtx, "account_reconcile_decision", "identity_source", identitySource,
			"match_reason", matchReason, "capture", captureStatus, "matched", id != "",
			"tombstoned", tombstoned, "bound_record", bound != "",
			"previous_profile", diagnosticRef(previous), "profile", diagnosticRef(id), "active_changed", previous != s.active[a.Provider])
	}
	for _, provider := range []string{"claude", "codex", "antigravity", "grok"} {
		if s.hideLegacyDuplicatesLocked(provider) {
			changed = true
		}
		s.flagCrossedCopiesLocked(provider)
	}
	if changed {
		if err := s.saveLocked(); err != nil {
			syncdiag.Log(ctx, "account_manifest_save_failed", "error", syncdiag.ErrorCode(err))
		}
	}
	out := s.decorateLocked(append([]Profile(nil), s.data.Profiles...))
	s.reconciled.Store(true)
	syncdiag.Log(ctx, "account_reconcile_end", "saved", len(out), "suspect", len(s.suspect))
	return out, nil
}

// liveRead is one live account read before the store lock is taken.
type liveRead struct {
	account      quota.Account
	doc          map[string]any
	err          error
	issuer       string // the account Google names for claims-less Antigravity tokens
	issuerSource string
}

// readLiveAccounts reads each supported provider's live login once and asks
// Google to name a claims-less Antigravity login. It runs without the store
// lock: the lookups can take seconds, and the UI thread reads the store
// every frame. An expired access token still answers through a refresh
// exchange.
func readLiveAccounts(ctx context.Context, live []quota.Account) []liveRead {
	var out []liveRead
	seen := map[string]bool{}
	for _, a := range live {
		if !supported(a.Provider) || seen[a.Provider] {
			continue
		}
		seen[a.Provider] = true
		r := liveRead{account: a}
		r.doc, r.err = a.CredentialDoc()
		if r.err == nil && a.Provider == "antigravity" && accountDocument(a.Provider, r.doc) && credentialid.Of(a.Provider, r.doc).Email == "" {
			if email := googleAccountEmail(ctx, accessTokenOf(r.doc)); email != "" {
				r.issuer, r.issuerSource = email, "google_userinfo"
			} else if email := googleRefreshedEmail(ctx, r.doc); email != "" {
				r.issuer, r.issuerSource = email, "google_refresh_exchange"
			}
		}
		out = append(out, r)
	}
	return out
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
	// The issuer settles what a copy's tokens actually are. A copy that
	// claims one account while holding another's would switch
	// "successfully" and only restore the copied sign-in, which reads as
	// "switching does not work". Ask before writing, and without the lock:
	// the UI thread reads the store every frame.
	issuer := s.targetIssuer(provider, id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	target, targetDoc, err := s.validateTargetLocked(provider, id)
	if err != nil {
		return err
	}
	if issuer != "" {
		if claimed := credentialid.Of(provider, targetDoc); claimed.Email != "" && !strings.EqualFold(issuer, claimed.Email) {
			s.suspect[provider+"/"+target.ID] = true
			return fmt.Errorf("this saved copy holds %s's sign-in; sign in again to refresh it", issuer)
		}
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
	// The capture above may have rewritten the target's file (a merge of
	// the same login); write the copy as it stands now.
	if targetDoc, err = s.readCredential(*target); err != nil {
		return errors.New("this account has no saved sign-in; sign in again")
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
	// Adapt before stripping our naming fields: the keychain shape carries
	// the account's email as id_token_email, as the client writes it.
	targetDoc = adaptCredential(provider, targetDoc, target.Kind, live.CredentialKind())
	targetDoc = switchDocument(provider, targetDoc)
	j := journalFile{Version: 1, Provider: provider, Target: id, BeforeHash: beforeHash, TargetHash: docHash(targetDoc), At: time.Now().Unix()}
	if err := s.writeJournal(j); err != nil {
		return err
	}
	// Compare-and-swap right before the write: the agent may have
	// refreshed or another login may have replaced the document while the
	// target was being read.
	if current, err := live.CredentialDoc(); err != nil || docHash(current) != beforeHash {
		_ = os.Remove(s.journalPath())
		return errLiveChanged
	}
	if err := live.WriteCredential(targetDoc); err != nil {
		// Put the previous login back so a failed write does not leave a
		// half-switched account. A failed restore keeps the journal.
		if live.WriteCredential(before) == nil {
			_ = os.Remove(s.journalPath())
		}
		return fmt.Errorf("%w: %w", errWriteFailed, err)
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

// targetIssuer asks Google whom an Antigravity copy's access token belongs
// to, "" when unknown or not Antigravity. It holds the lock only to read
// the copy.
func (s *Store) targetIssuer(provider, id string) string {
	if provider != "antigravity" {
		return ""
	}
	doc, err := s.ProfileCredential(provider, id)
	if err != nil {
		return ""
	}
	return googleAccountEmail(context.Background(), accessTokenOf(doc))
}

// validateTargetLocked is every check a target must pass before any write
// to the live store, shared by Switch and Reapply so no path skips them.
func (s *Store) validateTargetLocked(provider, id string) (*Profile, map[string]any, error) {
	if !s.data.Enabled {
		return nil, nil, ErrDisabled
	}
	var target *Profile
	for i := range s.data.Profiles {
		if s.data.Profiles[i].Provider == provider && s.data.Profiles[i].ID == id {
			target = &s.data.Profiles[i]
			break
		}
	}
	if target == nil {
		return nil, nil, errors.New("account not found")
	}
	if ok, reason := writable(*target); !ok {
		return nil, nil, errors.New(reason)
	}
	if s.suspect[provider+"/"+id] {
		return nil, nil, errors.New("this saved copy holds another account's sign-in; sign in again to refresh it")
	}
	doc, err := s.readCredential(*target)
	if err != nil {
		return nil, nil, errors.New("this account has no saved sign-in; sign in again")
	}
	if !accountDocument(provider, doc) {
		return nil, nil, errors.New("this profile is not a switchable account; sign in again")
	}
	if claimed := credentialid.Of(provider, doc); target.StableID != "" && claimed.Stable != "" && claimed.Stable != target.StableID {
		return nil, nil, errors.New("the saved sign-in belongs to a different account; sign in again")
	}
	return target, doc, nil
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
	target, doc, err := s.validateTargetLocked(provider, id)
	if err != nil {
		return err
	}
	doc = adaptCredential(provider, doc, target.Kind, live.CredentialKind())
	doc = switchDocument(provider, doc)
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
			// Capped here, where the user types it: the sync format allows
			// 512 bytes, and a longer name used to fail every later sync.
			name = strings.TrimSpace(name)
			if r := []rune(name); len(r) > MaxAccountNameRunes {
				name = strings.TrimSpace(string(r[:MaxAccountNameRunes]))
			}
			s.data.Profiles[i].Name = name
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

// RemoveAll deletes every saved credential copy except the records the
// agents are signed in to. An account in use is occupied: removing its copy
// only makes the next refresh capture it again, and leaving it keeps the
// roster naming the live sign-in. It does not touch the agents' live stores.
func (s *Store) RemoveAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	kept := make([]Profile, 0, len(s.data.Profiles))
	for _, p := range s.data.Profiles {
		if s.active[p.Provider] == p.ID {
			kept = append(kept, p)
			continue
		}
		s.suppressBackupLocked(p)
		_ = os.Remove(s.credentialPath(p.Provider, p.ID))
	}
	s.data.Profiles = kept
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

// unattestedSavedEmailLocked falls back to recorded names: the active
// profile first (the most recent attribution), then any other saved copy
// sharing the tokens.
func (s *Store) unattestedSavedEmailLocked(provider string, doc map[string]any) string {
	if p := s.activeProfileLocked(provider); p != nil && p.Email != "" && s.sameSavedLocked(*p, provider, doc) {
		return p.Email
	}
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

// SameSavedAccount reports whether this profile's saved credential shares
// a token with the given live document - it holds a copy of that login. It
// takes the store lock; callers must not pass it credentials they would not
// want compared.
func (s *Store) SameSavedAccount(p Profile, doc map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.sameSavedLocked(p, p.Provider, doc)
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
		if stableUpgrade(p.StableID, id.Stable) {
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
	if s.foldDuplicateAccountsLocked() {
		changed = true
	}
	if s.unsuppressSurvivorsLocked() {
		changed = true
	}
	for _, provider := range []string{"claude", "codex", "antigravity", "grok"} {
		s.flagCrossedCopiesLocked(provider)
	}
	return changed
}

// unsuppressSurvivorsLocked drops tombstones that collide with a profile
// still present: a surviving record means the account was never deleted,
// and the leftover key both keeps a live sign-in from binding to it and
// would remove the survivor on every device at the next sync.
//
// The survivor's stamp is deliberately left alone. A record that survives
// only because it was in use when another device deleted it ("Protected")
// is a deferred deletion: other devices keep the tombstone, and once this
// device switches away the next sync retires it
// (TestAccountSyncProtectsTheLiveSignIn). Stamping it past the tombstone
// would turn every in-use deletion into a permanent resurrection.
func (s *Store) unsuppressSurvivorsLocked() bool {
	changed := false
	for _, p := range s.data.Profiles {
		if s.data.Deleted[p.Provider+"/"+p.ID] > 0 {
			delete(s.data.Deleted, p.Provider+"/"+p.ID)
			changed = true
		}
		doc, err := s.readCredential(p)
		if err != nil {
			continue
		}
		for _, key := range s.identityKeysLocked(p, doc) {
			if s.data.Deleted[key] > 0 {
				delete(s.data.Deleted, key)
				changed = true
			}
		}
	}
	return changed
}

// foldDuplicateAccountsLocked merges records of the same provider carrying
// the same stable identity: they are one account with two copies, and the
// pair reads as a duplicate in every list. A record whose credential file
// survived outranks one whose copy is gone; then the record the live login
// is filed under, else the more recently modified one. The loser is removed
// with its file, and its removal is recorded so sync retires it on other
// devices instead of filing it back.
func (s *Store) foldDuplicateAccountsLocked() bool {
	changed := false
	for {
		folded := false
		seen := map[string]int{}
		for i := range s.data.Profiles {
			p := &s.data.Profiles[i]
			if !supported(p.Provider) || p.StableID == "" {
				continue
			}
			key := p.Provider + "\x00" + p.StableID
			// An Antigravity record may carry only the email where another
			// holds email|sub: one Google account, two records, so the
			// email keys them. Other providers can share an email across
			// distinct logins and keep the strict stable key.
			if p.Provider == "antigravity" && p.Email != "" {
				key = p.Provider + "\x00mail\x00" + strings.ToLower(p.Email)
			}
			j, ok := seen[key]
			if !ok {
				seen[key] = i
				continue
			}
			keep, drop := j, i
			keepFile, dropFile := s.credentialPath(p.Provider, s.data.Profiles[j].ID), s.credentialPath(p.Provider, p.ID)
			_, keepErr := os.Stat(keepFile)
			_, dropErr := os.Stat(dropFile)
			switch {
			case os.IsNotExist(keepErr) && dropErr == nil:
				keep, drop = i, j
			case keepErr == nil && os.IsNotExist(dropErr):
				// keep stays
			case s.active[p.Provider] == p.ID ||
				(s.active[p.Provider] != s.data.Profiles[j].ID && p.ModifiedAt > s.data.Profiles[j].ModifiedAt):
				keep, drop = i, j
			}
			kd, dd := &s.data.Profiles[keep], &s.data.Profiles[drop]
			if kd.Name == "" && dd.Name != "" {
				kd.Name, kd.NameModifiedAt = dd.Name, dd.NameModifiedAt
			}
			if dd.LastUsedAt > kd.LastUsedAt {
				kd.LastUsedAt = dd.LastUsedAt
			}
			if s.active[p.Provider] == dd.ID {
				s.active[p.Provider] = kd.ID
			}
			s.suppressRecordBackupLocked(*dd)
			_ = os.Remove(s.credentialPath(dd.Provider, dd.ID))
			s.data.Profiles = append(s.data.Profiles[:drop], s.data.Profiles[drop+1:]...)
			changed, folded = true, true
			break // indexes shifted; rescan until no pair remains
		}
		if !folded {
			return changed
		}
	}
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
		// A claims-less document may merge into any sharing account - the
		// tokens say it is the same login. A document carrying claims must
		// not overwrite a profile whose identity contradicts them, or a
		// wrongly stamped copy keeps being rewritten under the wrong name.
		sameTokens := !sameID && !sameStable && (id.Stable == "" && id.Email == "" || sameAccountClaims(id, *p)) && s.sameSavedLocked(*p, provider, doc)
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
		// Keep the richer stable key: email|sub names the account more
		// precisely than the email an email-only claim leaves, and either
		// direction can arrive from differently stamped documents.
		if stableUpgrade(p.StableID, id.Stable) {
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
	id, _ := s.matchReasonLocked(provider, doc, hash)
	return id
}

func (s *Store) matchReasonLocked(provider string, doc map[string]any, hash string) (string, string) {
	id := credentialid.Of(provider, doc)
	for _, p := range s.data.Profiles {
		if p.Provider != provider {
			continue
		}
		if id.Stable != "" && p.StableID == id.Stable {
			return p.ID, "stable_identity"
		}
		if p.Credentials != "" && p.Credentials == hash {
			return p.ID, "document_match"
		}
	}
	if id.Stable == "" && id.Email == "" {
		// A claims-less document belongs to whichever saved copy shares its
		// tokens - not only the active one's: Antigravity's state-database
		// login reads back as bare tokens, and its account is whichever
		// profile captured the same refresh token.
		for _, p := range s.data.Profiles {
			if p.Provider == provider && s.sameSavedLocked(p, provider, doc) {
				return p.ID, "saved_tokens"
			}
		}
		// An unprovable login keeps the previous attribution: a dying client
		// can write a stale sign-in back over a finished switch, and clearing
		// the active record on that write loses which account was switched
		// to.
		return s.active[provider], "previous_active_unverified"
	}
	if !strings.Contains(id.Stable, "|") {
		for _, p := range s.data.Profiles {
			if p.Provider != provider {
				continue
			}
			if provider == "antigravity" && id.Email != "" && strings.EqualFold(p.Email, id.Email) {
				return p.ID, "antigravity_email"
			}
			if p.ID == s.active[provider] && strings.HasPrefix(p.StableID, id.Stable+"|") {
				return p.ID, "active_identity_prefix"
			}
		}
	}
	return "", "unmatched"
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

func (s *Store) claudeEmailAcceptableLocked(email string, doc map[string]any) bool {
	if email == "" {
		return false
	}
	// Switching Claude in this app rewrites .credentials.json without
	// touching ~/.claude.json, which keeps the previous account's
	// oauthAccount until Claude Code updates it. Never attribute that
	// leftover email to a different saved copy's tokens.
	hasNamedOwner := false
	hasUnnamedTokenMatch := false
	for _, p := range s.data.Profiles {
		if p.Provider != "claude" {
			continue
		}
		if strings.EqualFold(p.Email, email) && !s.sameSavedLocked(p, "claude", doc) {
			hasNamedOwner = true
		}
		if p.Email == "" && s.sameSavedLocked(p, "claude", doc) {
			hasUnnamedTokenMatch = true
		}
	}
	return !(hasNamedOwner && hasUnnamedTokenMatch)
}

func claudeProfileEmail() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return claudeJSONEmail(filepath.Join(dir, ".claude.json"))
	}
	home := homeDir()
	if home == "" {
		return ""
	}
	if email := claudeJSONEmail(filepath.Join(home, ".claude.json")); email != "" {
		return email
	}
	return claudeJSONEmail(filepath.Join(home, ".claude", ".claude.json"))
}

func claudeJSONEmail(path string) string {
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

// stableUpgrade reports whether next names the account more precisely than
// cur: a fresh claim, or the |suffixed form of the same stable key.
func stableUpgrade(cur, next string) bool {
	return next != "" && next != cur && (cur == "" || strings.HasPrefix(next, cur+"|"))
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
			// The client names the signed-in account by this field;
			// Antigravity Tools writes it too.
			"id_token_email": antigravityEmailOf(doc, idToken),
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

// diagnosticRef never emits credential-derived document hashes.
func diagnosticRef(id string) string {
	if id == "" {
		return ""
	}
	return syncdiag.ID(id)
}

// antigravityEmailOf is the account email for a credential being written to
// Antigravity's keychain item: the item's own field, the email this app
// stamped, or the id_token's claim; "" leaves the field empty.
func antigravityEmailOf(doc map[string]any, idToken string) string {
	if email := str(doc["id_token_email"]); email != "" {
		return email
	}
	if email := str(doc["email"]); email != "" {
		return email
	}
	return credentialid.Of("antigravity", map[string]any{"id_token": idToken}).Email
}
