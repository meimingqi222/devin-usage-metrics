package accounts

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"runtime"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/credentialid"
)

type SyncResult struct {
	Added, Updated, Removed, Protected, Total int
}

type credentialChange struct {
	path     string
	data     []byte
	modified int64
	remove   bool
}

type credentialUndo struct {
	path     string
	data     []byte
	modified time.Time
	existed  bool
}

// ApplySync reconciles saved accounts. It never writes an agent's live store,
// and a remote change cannot replace/remove the account currently in use.
func (s *Store) ApplySync(remote Backup) (result SyncResult, err error) {
	if err = validateBackup(remote); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if !s.data.Enabled {
		return result, ErrDisabled
	}
	local, err := s.snapshotLocked()
	if err != nil {
		return result, err
	}
	merged := MergeSync(Syncable(local), Syncable(remote))
	if err = validateBackup(merged); err != nil {
		return
	}
	wanted := map[string]BackupEntry{}
	for _, e := range merged.Entries {
		if key := syncIdentity(e); key != "" {
			wanted[key] = e
		}
	}
	profiles := make([]Profile, 0, len(s.data.Profiles)+len(wanted))
	var changes []credentialChange
	for _, p := range s.data.Profiles {
		doc, readErr := s.readCredential(p)
		if readErr != nil {
			return result, errors.New("could not read a saved account during sync")
		}
		key := profileSyncKey(p, doc)
		if key == "" {
			profiles = append(profiles, p)
			continue
		}
		e, exists := wanted[key]
		active := s.active[p.Provider] == p.ID
		if !exists {
			if merged.Deleted[key] > 0 {
				if active {
					result.Protected++
				} else {
					changes = append(changes, credentialChange{path: s.credentialPath(p.Provider, p.ID), remove: true})
					result.Removed++
					continue
				}
			}
			profiles = append(profiles, p)
			continue
		}
		delete(wanted, key)
		incoming := backupDocument(e, p.Kind)
		updated := p.Name != e.Name
		p.Name, p.NameModifiedAt = e.Name, e.NameModifiedAt
		if docHash(doc) != docHash(incoming) {
			if active {
				result.Protected++
			} else {
				raw, marshalErr := json.Marshal(incoming)
				if marshalErr != nil {
					return result, errors.New("invalid account document")
				}
				changes = append(changes, credentialChange{path: s.credentialPath(p.Provider, p.ID), data: raw, modified: e.ModifiedAt})
				p.ModifiedAt = e.ModifiedAt
				p.Credentials = docHash(incoming)
				updated = true
			}
		}
		if updated {
			result.Updated++
		}
		profiles = append(profiles, p)
	}
	for _, e := range merged.Entries {
		if _, exists := wanted[syncIdentity(e)]; !exists {
			continue
		}
		id := credentialid.Of(e.Provider, e.Credential)
		kind := "file"
		if runtime.GOOS == "darwin" && (e.Provider == "claude" || e.Provider == "antigravity") {
			kind = "keychain"
		}
		p := Profile{ID: profileID(e.Provider, id, e.Credential, docHash(e.Credential)), Provider: e.Provider, Kind: kind, Label: credentialid.Label(e.Provider, id), Email: id.Email, StableID: id.Stable, Plan: id.Plan, Name: e.Name, NameModifiedAt: e.NameModifiedAt, ModifiedAt: e.ModifiedAt, CreatedAt: time.Now().Unix(), Credentials: docHash(e.Credential), Imported: true}
		path := s.credentialPath(p.Provider, p.ID)
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			return result, ErrBackupFileConflict
		}
		raw, marshalErr := json.Marshal(backupDocument(e, kind))
		if marshalErr != nil {
			return result, errors.New("invalid account document")
		}
		changes = append(changes, credentialChange{path: path, data: raw, modified: e.ModifiedAt})
		profiles = append(profiles, p)
		result.Added++
	}
	if len(profiles) > MaxBackupAccounts {
		return result, ErrTooManySavedAccounts
	}
	beforeProfiles, beforeDeleted := s.data.Profiles, s.data.Deleted
	undos := make([]credentialUndo, 0, len(changes))
	defer func() {
		if err == nil {
			return
		}
		s.data.Profiles, s.data.Deleted = beforeProfiles, beforeDeleted
		var rollbackErr error
		for i := len(undos) - 1; i >= 0; i-- {
			u := undos[i]
			if u.existed {
				if e := writePrivate(u.path, u.data); e != nil {
					rollbackErr = errors.Join(rollbackErr, e)
					continue
				}
				rollbackErr = errors.Join(rollbackErr, os.Chtimes(u.path, u.modified, u.modified))
			} else if e := os.Remove(u.path); !os.IsNotExist(e) {
				rollbackErr = errors.Join(rollbackErr, e)
			}
		}
		if rollbackErr != nil {
			err = errors.New("account sync failed; saved credentials could not all be restored")
		}
		result = SyncResult{}
	}()
	for _, change := range changes {
		u := credentialUndo{path: change.path}
		info, statErr := os.Stat(change.path)
		if statErr == nil {
			u.data, err = os.ReadFile(change.path)
			if err != nil {
				return
			}
			u.existed, u.modified = true, info.ModTime()
		} else if !os.IsNotExist(statErr) {
			return result, statErr
		}
		undos = append(undos, u)
		if change.remove {
			err = os.Remove(change.path)
		} else {
			err = writePrivate(change.path, change.data)
			if err == nil {
				stamp := time.Unix(0, change.modified)
				err = os.Chtimes(change.path, stamp, stamp)
			}
		}
		if err != nil {
			return
		}
	}
	s.data.Profiles, s.data.Deleted = profiles, maps.Clone(merged.Deleted)
	err = s.saveLocked()
	result.Total = len(merged.Entries)
	return
}

func backupDocument(e BackupEntry, kind string) map[string]any {
	doc := adaptCredential(e.Provider, e.Credential, credentialKind(e.Credential), kind)
	id := credentialid.Of(e.Provider, e.Credential)
	if id.Email != "" && credentialid.Of(e.Provider, doc).Email == "" {
		doc = maps.Clone(doc)
		doc["email"] = id.Email
	}
	return doc
}
