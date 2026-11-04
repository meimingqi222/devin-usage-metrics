package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"runtime"
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

var (
	// ErrBackupPassword covers both a wrong password and a damaged envelope;
	// callers answer with the same re-enter-password flow.
	ErrBackupPassword = errors.New("incorrect backup password or damaged account backup")
	ErrShortPassword  = errors.New("use an account backup password of at least 10 characters")
	// ErrTooManySavedAccounts and ErrBackupFileConflict surface in import and
	// sync paths; the message text is the syncdiag error code, do not reword.
	ErrTooManySavedAccounts = errors.New("too many saved accounts")
	ErrBackupFileConflict   = errors.New("account backup conflicts with an existing saved file")
)

// Backup is plaintext in memory only. Never log or upload its JSON directly.
type Backup struct {
	Version int              `json:"version"`
	Entries []BackupEntry    `json:"accounts"`
	Deleted map[string]int64 `json:"deleted,omitempty"`
}

type BackupEntry struct {
	Provider       string         `json:"provider"`
	Name           string         `json:"name,omitempty"`
	ModifiedAt     int64          `json:"modified_at"`
	Credential     map[string]any `json:"credential"`
	NameModifiedAt int64          `json:"name_modified_at,omitempty"`
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
		return ErrShortPassword
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
	if b.Version != 1 || len(b.Entries) > MaxBackupAccounts || len(b.Deleted) > 4096 {
		return errors.New("unsupported or oversized account backup")
	}
	for _, e := range b.Entries {
		if !supported(e.Provider) || !accountDocument(e.Provider, e.Credential) || len(e.Name) > 512 || e.ModifiedAt < 0 || e.ModifiedAt > time.Now().Add(24*time.Hour).UnixNano() || e.NameModifiedAt < 0 || e.NameModifiedAt > time.Now().Add(24*time.Hour).UnixNano() {
			return errors.New("invalid account backup entry")
		}
	}
	for key, at := range b.Deleted {
		provider, id, ok := strings.Cut(key, "/")
		if !ok || !supported(provider) || len(id) != 16 || strings.ContainsAny(id, "/\\.") || at <= 0 || at > time.Now().Add(24*time.Hour).UnixNano() {
			return errors.New("invalid deleted account")
		}
		if _, err := hex.DecodeString(id); err != nil {
			return errors.New("invalid deleted account")
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
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() (Backup, error) {
	b := Backup{Version: 1, Entries: []BackupEntry{}, Deleted: maps.Clone(s.data.Deleted)}
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
		b.Entries = append(b.Entries, BackupEntry{Provider: p.Provider, Name: p.Name, ModifiedAt: stamp, Credential: doc, NameModifiedAt: p.NameModifiedAt})
	}
	return b, validateBackup(b)
}

func backupIdentity(e BackupEntry) string {
	id := credentialid.Of(e.Provider, e.Credential)
	return e.Provider + "/" + profileID(e.Provider, id, e.Credential, docHash(e.Credential))
}

// MergeBackups reconciles credentials, names and deletions independently.
func MergeBackups(backups ...Backup) Backup {
	entries := map[string]BackupEntry{}
	deleted := map[string]int64{}
	for _, b := range backups {
		for _, e := range b.Entries {
			key := backupIdentity(e)
			old, exists := entries[key]
			if !exists {
				entries[key] = e
				continue
			}
			credential := old
			if e.ModifiedAt > old.ModifiedAt || e.ModifiedAt == old.ModifiedAt && docHash(e.Credential) > docHash(old.Credential) {
				credential = e
			}
			name := old
			if e.NameModifiedAt > old.NameModifiedAt || e.NameModifiedAt == old.NameModifiedAt && e.Name > old.Name {
				name = e
			}
			credential.Name, credential.NameModifiedAt = name.Name, name.NameModifiedAt
			entries[key] = credential
		}
		for key, at := range b.Deleted {
			deleted[key] = max(deleted[key], at)
		}
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b := Backup{Version: 1, Entries: []BackupEntry{}, Deleted: deleted}
	for _, key := range keys {
		if deleted[key] < entries[key].ModifiedAt || deleted[key] == 0 {
			b.Entries = append(b.Entries, entries[key])
		}
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
	beforeDeleted := maps.Clone(s.data.Deleted)
	var created []string
	defer func() {
		if err != nil {
			s.data.Profiles = before
			s.data.Deleted = beforeDeleted
			for _, path := range created {
				_ = os.Remove(path)
			}
			added = 0
		}
	}()
	// File import is an explicit restore, so deletion history is not applied.
	b.Deleted = nil
	for _, e := range MergeBackups(b).Entries {
		key := backupIdentity(e)
		delete(s.data.Deleted, key)
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
			return 0, kept, ErrTooManySavedAccounts
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
		p := Profile{ID: pid, Provider: e.Provider, Label: profileLabel(e.Provider, id, pid), Email: id.Email, StableID: id.Stable, Plan: id.Plan, Name: e.Name, Kind: kind, CreatedAt: time.Now().Unix(), ModifiedAt: e.ModifiedAt, NameModifiedAt: e.NameModifiedAt, Imported: true}
		if beforeDeleted[key] > 0 {
			p.ModifiedAt = max(time.Now().UnixNano(), beforeDeleted[key]+1)
		}
		path := s.credentialPath(p.Provider, p.ID)
		if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
			return 0, kept, ErrBackupFileConflict
		}
		if err = s.writeCredential(p, doc); err != nil {
			return
		}
		created = append(created, path)
		modified := time.Unix(0, p.ModifiedAt)
		if err = os.Chtimes(path, modified, modified); err != nil {
			return
		}
		s.data.Profiles = append(s.data.Profiles, p)
		added++
	}
	if added > 0 || !maps.Equal(beforeDeleted, s.data.Deleted) {
		err = s.saveLocked()
	}
	return
}

func (s *Store) suppressBackupLocked(p Profile) {
	keys := []string{p.Provider + "/" + p.ID}
	if doc, err := s.readCredential(p); err == nil {
		keys = append(keys, backupIdentity(BackupEntry{Provider: p.Provider, Credential: doc}))
	}
	if s.data.Deleted == nil {
		s.data.Deleted = map[string]int64{}
	}
	for _, key := range keys {
		s.data.Deleted[key] = time.Now().UnixNano()
	}
}

func credentialKind(doc map[string]any) string {
	if obj(doc["token"]) != nil {
		return "keychain"
	}
	return "file"
}
