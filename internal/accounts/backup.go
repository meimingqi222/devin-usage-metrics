package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meimingqi222/devin-usage-metrics/internal/credentialid"
)

const (
	MaxBackupBytes    = 16 << 20
	MaxBackupAccounts = 512
	maxPlainBytes     = 8 << 20
	backupRounds      = 600_000
	backupDomain      = "aum-account-backup-v1"
)

var ErrBackupPassword = errors.New("incorrect backup password or damaged account backup")

// Backup is plaintext in memory only. Never log or upload its JSON directly.
type Backup struct {
	Version int           `json:"version"`
	Entries []BackupEntry `json:"accounts"`
}

type BackupEntry struct {
	Provider   string         `json:"provider"`
	Name       string         `json:"name,omitempty"`
	ModifiedAt int64          `json:"modified_at"`
	Credential map[string]any `json:"credential"`
}

type backupEnvelope struct {
	Version    int    `json:"version"`
	Cipher     string `json:"cipher"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       []byte `json:"salt"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

func CheckBackupPassword(password string) error {
	if utf8.RuneCountInString(password) < 10 || len(password) > 1024 || strings.TrimSpace(password) == "" {
		return errors.New("use an account backup password of at least 10 characters")
	}
	return nil
}

func backupCipher(password string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, append([]byte(backupDomain), salt...), backupRounds, 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func validateBackup(b Backup) error {
	if b.Version != 1 || len(b.Entries) > MaxBackupAccounts {
		return errors.New("unsupported or oversized account backup")
	}
	for _, e := range b.Entries {
		if !supported(e.Provider) || !accountDocument(e.Provider, e.Credential) || len(e.Name) > 512 || e.ModifiedAt < 0 || e.ModifiedAt > time.Now().Add(24*time.Hour).UnixNano() {
			return errors.New("invalid account backup entry")
		}
	}
	plain, err := json.Marshal(b)
	defer clear(plain)
	if err != nil || len(plain) > maxPlainBytes {
		return errors.New("account backup exceeds size limit")
	}
	return nil
}

func SealBackup(b Backup, password string) ([]byte, error) {
	if err := CheckBackupPassword(password); err != nil {
		return nil, err
	}
	if err := validateBackup(b); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(b)
	if err != nil || len(plain) > maxPlainBytes {
		return nil, errors.New("account backup exceeds size limit")
	}
	defer clear(plain)
	e := backupEnvelope{Version: 1, Cipher: "AES-256-GCM", KDF: "PBKDF2-SHA256", Iterations: backupRounds, Salt: make([]byte, 16), Nonce: make([]byte, 12)}
	if _, err := rand.Read(e.Salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(e.Nonce); err != nil {
		return nil, err
	}
	a, err := backupCipher(password, e.Salt)
	if err != nil {
		return nil, err
	}
	e.Ciphertext = a.Seal(nil, e.Nonce, plain, []byte(backupDomain))
	return json.Marshal(e)
}

func OpenBackup(raw []byte, password string) (Backup, error) {
	var b Backup
	var e backupEnvelope
	if len(raw) > MaxBackupBytes || json.Unmarshal(raw, &e) != nil || e.Version != 1 || e.Cipher != "AES-256-GCM" || e.KDF != "PBKDF2-SHA256" || e.Iterations != backupRounds || len(e.Salt) != 16 || len(e.Nonce) != 12 || len(e.Ciphertext) < 16 || len(e.Ciphertext) > maxPlainBytes+16 {
		return b, errors.New("unsupported or damaged encrypted account backup")
	}
	if err := CheckBackupPassword(password); err != nil {
		return b, err
	}
	a, err := backupCipher(password, e.Salt)
	if err != nil {
		return b, err
	}
	plain, err := a.Open(nil, e.Nonce, e.Ciphertext, []byte(backupDomain))
	if err != nil {
		return b, ErrBackupPassword
	}
	defer clear(plain)
	if json.Unmarshal(plain, &b) != nil {
		return Backup{}, errors.New("invalid account backup")
	}
	return b, validateBackup(b)
}

func ReadBackupFile(path, password string) (Backup, error) {
	f, err := os.Open(path)
	if err != nil {
		return Backup{}, errors.New("could not open account backup")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBackupBytes {
		return Backup{}, errors.New("account backup is not a regular file or exceeds size limit")
	}
	// A bounded read also covers a file growing after Stat.
	raw, err := io.ReadAll(io.LimitReader(f, MaxBackupBytes+1))
	if len(raw) > MaxBackupBytes {
		return Backup{}, errors.New("account backup exceeds size limit")
	}
	if err != nil {
		return Backup{}, errors.New("could not read account backup")
	}
	return OpenBackup(raw, password)
}

func WriteBackupFile(path string, b Backup, password string) error {
	raw, err := SealBackup(b, password)
	if err != nil {
		return err
	}
	if err := writePrivate(path, raw); err != nil {
		return errors.New("could not save encrypted account backup")
	}
	return nil
}

// Snapshot includes valid saved profiles only, never the active agent stores.
func (s *Store) Snapshot() (Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	b := Backup{Version: 1, Entries: []BackupEntry{}}
	for _, p := range s.data.Profiles {
		doc, err := s.readCredential(p)
		if err != nil {
			return Backup{}, errors.New("could not read a saved account for backup")
		}
		if p.Hidden || !accountDocument(p.Provider, doc) {
			continue
		}
		stamp := p.ModifiedAt
		// Quota refresh can rotate the saved credential directly. The file's
		// timestamp catches those writes as well as changes through this Store.
		if info, err := os.Stat(s.credentialPath(p.Provider, p.ID)); err == nil {
			stamp = max(stamp, info.ModTime().UnixNano())
		}
		b.Entries = append(b.Entries, BackupEntry{p.Provider, p.Name, stamp, doc})
	}
	return b, validateBackup(b)
}

func backupIdentity(e BackupEntry) string {
	id := credentialid.Of(e.Provider, e.Credential)
	return e.Provider + "/" + profileID(e.Provider, id, e.Credential, docHash(e.Credential))
}

// MergeBackups unions device snapshots. Newer credential modifications win in
// the backup only; importing always preserves an existing local credential.
func MergeBackups(backups ...Backup) Backup {
	entries := map[string]BackupEntry{}
	for _, b := range backups {
		for _, e := range b.Entries {
			key := backupIdentity(e)
			if old, ok := entries[key]; !ok || e.ModifiedAt > old.ModifiedAt {
				entries[key] = e
			}
		}
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b := Backup{Version: 1, Entries: []BackupEntry{}}
	for _, key := range keys {
		b.Entries = append(b.Entries, entries[key])
	}
	return b
}

func (e BackupEntry) Label() string {
	if e.Name != "" {
		return e.Name
	}
	return credentialid.Label(e.Provider, credentialid.Of(e.Provider, e.Credential))
}

// ImportBackup adds missing profiles transactionally and never calls Switch or
// Reconcile. In particular, upsertLocked's active-account fallback is unsuitable
// for foreign credentials without identity claims.
func (s *Store) ImportBackup(b Backup) (added, kept int, err error) {
	return s.importBackup(b, false)
}

// ImportSyncedBackup honors local removals; an explicit file import can restore
// them, while a later sync cannot silently resurrect a removed account.
func (s *Store) ImportSyncedBackup(b Backup) (added, kept int, err error) {
	return s.importBackup(b, true)
}

func (s *Store) importBackup(b Backup, synced bool) (added, kept int, err error) {
	if err = validateBackup(b); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if !s.data.Enabled {
		return 0, 0, ErrDisabled
	}
	before := append([]Profile(nil), s.data.Profiles...)
	beforeSuppressed := append([]string(nil), s.data.SuppressedBackups...)
	var created []string
	defer func() {
		if err != nil {
			s.data.Profiles = before
			s.data.SuppressedBackups = beforeSuppressed
			for _, path := range created {
				_ = os.Remove(path)
			}
			added = 0
		}
	}()
	for _, e := range MergeBackups(b).Entries {
		key := backupIdentity(e)
		if synced && slices.Contains(s.data.SuppressedBackups, key) {
			kept++
			continue
		}
		if !synced {
			s.data.SuppressedBackups = slices.DeleteFunc(s.data.SuppressedBackups, func(k string) bool { return k == key })
		}
		id := credentialid.Of(e.Provider, e.Credential)
		pid := profileID(e.Provider, id, e.Credential, docHash(e.Credential))
		exists := false
		for _, p := range s.data.Profiles {
			unknownIdentity := id.Stable == "" || p.StableID == ""
			if p.Provider == e.Provider && (p.ID == pid || id.Stable != "" && p.StableID == id.Stable || unknownIdentity && s.sameSavedLocked(p, e.Provider, e.Credential)) {
				exists = true
				break
			}
		}
		if exists {
			kept++
			continue
		}
		if len(s.data.Profiles) >= MaxBackupAccounts {
			return 0, kept, errors.New("too many saved accounts")
		}
		kind := "file"
		if runtime.GOOS == "darwin" && (e.Provider == "claude" || e.Provider == "antigravity") {
			kind = "keychain"
		}
		doc := adaptCredential(e.Provider, e.Credential, credentialKind(e.Credential), kind)
		if id.Email != "" && credentialid.Of(e.Provider, doc).Email == "" {
			doc = maps.Clone(doc)
			doc["email"] = id.Email
		}
		p := Profile{ID: pid, Provider: e.Provider, Label: profileLabel(e.Provider, id, pid), Email: id.Email, StableID: id.Stable, Plan: id.Plan, Name: e.Name, Kind: kind, CreatedAt: time.Now().Unix(), ModifiedAt: e.ModifiedAt, Imported: true}
		path := s.credentialPath(p.Provider, p.ID)
		if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
			return 0, kept, errors.New("account backup conflicts with an existing saved file")
		}
		if err = s.writeCredential(p, doc); err != nil {
			return
		}
		created = append(created, path)
		modified := time.Unix(0, e.ModifiedAt)
		if err = os.Chtimes(path, modified, modified); err != nil {
			return
		}
		s.data.Profiles = append(s.data.Profiles, p)
		added++
	}
	if added > 0 || !slices.Equal(beforeSuppressed, s.data.SuppressedBackups) {
		err = s.saveLocked()
	}
	return
}

func (s *Store) suppressBackupLocked(p Profile) {
	keys := []string{p.Provider + "/" + p.ID}
	if doc, err := s.readCredential(p); err == nil {
		keys = append(keys, backupIdentity(BackupEntry{Provider: p.Provider, Credential: doc}))
	}
	for _, key := range keys {
		if !slices.Contains(s.data.SuppressedBackups, key) {
			s.data.SuppressedBackups = append(s.data.SuppressedBackups, key)
		}
	}
}

func credentialKind(doc map[string]any) string {
	if obj(doc["token"]) != nil {
		return "keychain"
	}
	return "file"
}
