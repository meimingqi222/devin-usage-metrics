package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

const accountPrefix = "accounts-v1-"

// Account sync failures a user can act on. The desktop UI matches these with
// errors.Is; the message text is also the syncdiag error code, so do not reword.
var (
	ErrNotSignedIn    = errors.New("sign in to sync before enabling account sync")
	ErrSyncReenabled  = errors.New("account sync must be enabled again for this server and GitHub account")
	ErrServerTooOld   = errors.New("update the sync server to support encrypted account backups")
	ErrTooManyDevices = errors.New("too many account backup devices")
	ErrTooManyRemote  = errors.New("too many accounts in remote backups")
	ErrSyncAborted    = errors.New("account sync was disabled or its account changed")
)

// AccountSyncConfig stores consent and scope, never the password itself.
type AccountSyncConfig struct {
	Enabled  bool   `json:"enabled"`
	Scope    string `json:"scope,omitempty"`
	Remember bool   `json:"remember,omitempty"`
}

func ReadAccountSyncConfig() AccountSyncConfig {
	var c AccountSyncConfig
	raw, err := os.ReadFile(path("account-sync.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

func SaveAccountSyncConfig(c AccountSyncConfig) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomic("account-sync.json", raw)
}

func accountTransport() (transport, error) {
	c := ReadConfig()
	// Config.Enabled controls usage sync only. Account sync has its own
	// consent in AccountSyncConfig and shares only the server and sign-in.
	base, err := baseURL(c.APIURL)
	if err != nil {
		return transport{}, err
	}
	s, err := loadSession()
	if err != nil || s.Token == "" || s.Expires <= time.Now().Unix() {
		return transport{}, ErrNotSignedIn
	}
	return transport{base, s.Token}, nil
}

func AccountSyncScope() (string, error) {
	t, err := accountTransport()
	if err != nil {
		return "", err
	}
	return t.account(), nil
}

type AccountSyncResult struct {
	accounts.SyncResult
	Devices int
	// Unreadable counts remote snapshots that neither password could open:
	// devices still sealed with a retired password (mid-rotation) or offline
	// and out of date. They are skipped, never merged.
	Unreadable int
}

// SyncAccounts reconciles encrypted saved accounts across devices. Each device
// publishes its own snapshot; additions, edits and deletions are merged while
// active agent sign-ins are kept out of the write path. prevPassword, when the
// password was just rotated, still opens snapshots sealed with the previous
// one so the rotation publishes a complete merge.
func SyncAccounts(ctx context.Context, store *accounts.Store, password, scope, prevPassword string) (result AccountSyncResult, err error) {
	ctx = context.WithValue(ctx, responseLimitKey{}, accounts.MaxSyncObjectBytes)
	ctx = syncdiag.WithFields(syncdiag.WithRun(ctx), "operation", "account_sync")
	ctx, finish := syncdiag.Stage(ctx, "account_sync")
	defer func() { finish(err) }()
	if err = accounts.CheckBackupPassword(password); err != nil {
		return
	}
	t, err := accountTransport()
	if err != nil {
		return result, err
	}
	consent := ReadAccountSyncConfig()
	if !consent.Enabled || consent.Scope != scope || t.account() != scope {
		return result, ErrSyncReenabled
	}
	if !store.Enabled() {
		return result, accounts.ErrDisabled
	}
	id, err := device()
	if err != nil {
		return result, err
	}
	// Old servers would garbage-collect independent account snapshots. Require
	// an explicit retention capability before sending a single credential byte.
	raw, _, status, err := t.request(ctx, "GET", "/v1/capabilities", nil, nil)
	if err != nil {
		return result, err
	}
	var capabilities struct {
		AccountBackups int `json:"account_backups"`
	}
	if status != 200 || json.Unmarshal(raw, &capabilities) != nil || capabilities.AccountBackups != 1 {
		return result, ErrServerTooOld
	}
	files, err := t.list(ctx, accountPrefix)
	if err != nil {
		return result, err
	}
	if len(files) > 64 {
		return result, ErrTooManyDevices
	}
	merged := accounts.Backup{Version: 1}
	unreadable := 0
	for _, name := range files {
		if !validAccountObject(name) {
			return result, errors.New("invalid account backup object name")
		}
		raw, err := t.get(ctx, name, accounts.MaxSyncObjectBytes)
		if err != nil {
			return result, err
		}
		b, err := accounts.OpenBackup(raw, password)
		if err != nil && prevPassword != "" && prevPassword != password {
			b, err = accounts.OpenBackup(raw, prevPassword)
		}
		if err != nil {
			// A snapshot sealed with a password this device does not hold — a
			// device that has not received a rotation yet, or a damaged copy
			// — must not abort the cycle, or no device could ever change the
			// password. Skip it and report the count; the guard below still
			// rejects an outright wrong password before publishing anything.
			unreadable++
			continue
		}
		merged = accounts.MergeSync(merged, accounts.Syncable(b))
		if len(merged.Entries) > accounts.MaxBackupAccounts {
			return result, ErrTooManyRemote
		}
	}
	if len(files) > 0 && unreadable == len(files) {
		// Nothing unlocked, including our own previous snapshot: the entered
		// password is simply wrong. Never publish or import in that case.
		return result, accounts.ErrBackupPassword
	}
	result.Unreadable = unreadable
	if err = ctx.Err(); err != nil {
		return
	}
	// Snapshot before import keeps downloaded credentials' original timestamp.
	local, err := store.Snapshot()
	if err != nil {
		return result, err
	}
	merged = accounts.MergeSync(merged, accounts.Syncable(local))
	raw, err = accounts.SealBackup(merged, password)
	if err != nil {
		return result, err
	}
	if len(raw) > accounts.MaxSyncObjectBytes {
		return result, errors.New("encrypted account backup exceeds sync size limit")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	name := accountPrefix + hash([]byte(id)) + ".json"
	consent = ReadAccountSyncConfig()
	current, currentErr := AccountSyncScope()
	if !consent.Enabled || consent.Scope != scope || currentErr != nil || current != scope {
		return result, ErrSyncAborted
	}
	if !store.Enabled() {
		return result, accounts.ErrDisabled
	}
	if err = t.put(ctx, name, raw, nil); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	// Turning off or changing the endpoint during networking prevents a late import.
	consent = ReadAccountSyncConfig()
	current, currentErr = AccountSyncScope()
	if !consent.Enabled || consent.Scope != scope || currentErr != nil || current != scope {
		return result, ErrSyncAborted
	}
	result.SyncResult, err = store.ApplySync(merged)
	result.Devices = len(files)
	syncdiag.Log(ctx, "account_sync_result", "added", result.Added, "updated", result.Updated, "removed", result.Removed, "protected", result.Protected, "devices", result.Devices, "unreadable", result.Unreadable, "error", syncdiag.ErrorCode(err))
	return
}

func validAccountObject(name string) bool {
	digest := strings.TrimSuffix(strings.TrimPrefix(name, accountPrefix), ".json")
	return name == accountPrefix+digest+".json" && validHash(digest)
}

func accountSnapshotName() (string, error) {
	id, err := device()
	if err != nil {
		return "", err
	}
	return accountPrefix + hash([]byte(id)) + ".json", nil
}

// DeleteAccountSnapshot removes one encrypted account object in this GitHub
// tenant. Only accounts-v1-* names are accepted; usage objects cannot be deleted
// through this path even if the server were to allow it.
func DeleteAccountSnapshot(ctx context.Context, name string) error {
	if !validAccountObject(name) {
		return errors.New("invalid account backup object name")
	}
	t, err := accountTransport()
	if err != nil {
		return err
	}
	return t.delete(ctx, name)
}

// DeleteOwnAccountSnapshot removes this device's snapshot. Turning account sync
// off calls it so a reinstall does not keep occupying a device slot.
func DeleteOwnAccountSnapshot(ctx context.Context) error {
	name, err := accountSnapshotName()
	if err != nil {
		return err
	}
	return DeleteAccountSnapshot(ctx, name)
}

// DeleteOwnAccountSnapshotIfDisabled is the disable-sync path. A late worker
// must not delete a snapshot after the user has already turned sync back on.
func DeleteOwnAccountSnapshotIfDisabled(ctx context.Context) error {
	if ReadAccountSyncConfig().Enabled {
		return nil
	}
	return DeleteOwnAccountSnapshot(ctx)
}

// DropUnreadableAccountSnapshots deletes tenant snapshots neither password can
// open. If nothing opens, the password is treated as wrong and nothing is deleted.
func DropUnreadableAccountSnapshots(ctx context.Context, password, prevPassword string) (dropped int, err error) {
	t, err := accountTransport()
	if err != nil {
		return 0, err
	}
	files, err := t.list(ctx, accountPrefix)
	if err != nil {
		return 0, err
	}
	var unreadable []string
	opened := 0
	for _, name := range files {
		if !validAccountObject(name) {
			return 0, errors.New("invalid account backup object name")
		}
		raw, getErr := t.get(ctx, name, accounts.MaxSyncObjectBytes)
		if getErr != nil {
			return dropped, getErr
		}
		_, openErr := accounts.OpenBackup(raw, password)
		if openErr != nil && prevPassword != "" && prevPassword != password {
			_, openErr = accounts.OpenBackup(raw, prevPassword)
		}
		if openErr != nil {
			unreadable = append(unreadable, name)
			continue
		}
		opened++
	}
	if len(files) > 0 && opened == 0 {
		return 0, accounts.ErrBackupPassword
	}
	for _, name := range unreadable {
		if err := t.delete(ctx, name); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

// DropForeignAccountSnapshots deletes every account snapshot except this device's.
// Live devices re-upload on their next sync; it is the way to free the 64-device cap.
func DropForeignAccountSnapshots(ctx context.Context) (dropped int, err error) {
	own, err := accountSnapshotName()
	if err != nil {
		return 0, err
	}
	t, err := accountTransport()
	if err != nil {
		return 0, err
	}
	files, err := t.list(ctx, accountPrefix)
	if err != nil {
		return 0, err
	}
	for _, name := range files {
		if !validAccountObject(name) {
			return dropped, errors.New("invalid account backup object name")
		}
		if name == own {
			continue
		}
		if err := t.delete(ctx, name); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}
