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

// AccountSyncConfig stores consent and its exact server/tenant scope, never the
// password. A process restart requires unlocking again.
type AccountSyncConfig struct {
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope,omitempty"`
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
		return transport{}, errors.New("sign in to sync before enabling account sync")
	}
	base, err := baseURL(c.APIURL)
	if err != nil {
		return transport{}, err
	}
	s, err := loadSession()
	if err != nil || s.Expires <= time.Now().Unix() {
		return transport{}, errors.New("sign in to sync before enabling account sync")
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
	Added, Kept, Devices int
}

// SyncAccounts publishes one encrypted snapshot per device and imports missing
// accounts from all devices. A device never overwrites another device's object.
// Existing local credentials, active sign-ins and deletions are not replicated.
func SyncAccounts(ctx context.Context, store *accounts.Store, password, scope string) (result AccountSyncResult, err error) {
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
		return result, errors.New("account sync must be enabled again for this server and GitHub account")
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
		return result, errors.New("update the sync server to support encrypted account backups")
	}
	files, err := t.list(ctx, accountPrefix)
	if err != nil {
		return result, err
	}
	if len(files) > 64 {
		return result, errors.New("too many account backup devices")
	}
	merged := accounts.Backup{Version: 1}
	for _, name := range files {
		if !validAccountObject(name) {
			return result, errors.New("invalid account backup object name")
		}
		raw, err := t.get(ctx, name, accounts.MaxBackupBytes)
		if err != nil {
			return result, err
		}
		b, err := accounts.OpenBackup(raw, password)
		if err != nil {
			// Never publish or import on a wrong password or corrupt snapshot,
			// including when replacing our own device's backup.
			return result, err
		}
		merged = accounts.MergeBackups(merged, b)
		if len(merged.Entries) > accounts.MaxBackupAccounts {
			return result, errors.New("too many accounts in remote backups")
		}
	}
	if err = ctx.Err(); err != nil {
		return
	}
	// Snapshot before import keeps downloaded credentials' original timestamp.
	local, err := store.Snapshot()
	if err != nil {
		return result, err
	}
	raw, err = accounts.SealBackup(local, password)
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
		return result, errors.New("account sync was disabled or its account changed")
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
		return result, errors.New("account sync was disabled or its account changed")
	}
	result.Added, result.Kept, err = store.ImportSyncedBackup(merged)
	result.Devices = len(files)
	syncdiag.Log(ctx, "account_sync_result", "added", result.Added, "kept", result.Kept, "devices", result.Devices, "error", syncdiag.ErrorCode(err))
	return
}

func validAccountObject(name string) bool {
	digest := strings.TrimSuffix(strings.TrimPrefix(name, accountPrefix), ".json")
	return name == accountPrefix+digest+".json" && validHash(digest)
}
