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
	if !c.Enabled {
		return transport{}, ErrNotSignedIn
	}
	base, err := baseURL(c.APIURL)
	if err != nil {
		return transport{}, err
	}
	s, err := loadSession()
	if err != nil || s.Expires <= time.Now().Unix() {
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
	ctx = context.WithValue(ctx, responseLimitKey{}, accounts.MaxBackupBytes)
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
		raw, err := t.get(ctx, name, accounts.MaxBackupBytes)
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
		merged = accounts.MergeBackups(merged, b)
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
	merged = accounts.MergeBackups(merged, local)
	raw, err = accounts.SealBackup(merged, password)
	if err != nil {
		return result, err
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
