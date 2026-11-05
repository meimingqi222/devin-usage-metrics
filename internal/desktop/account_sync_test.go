package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

type memoryAccountPasswords struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *memoryAccountPasswords) Get(scope string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[scope]
	if !ok {
		return "", errors.New("missing fixture secret")
	}
	return v, nil
}
func (m *memoryAccountPasswords) Set(scope, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[scope] = password
	return nil
}
func (m *memoryAccountPasswords) Delete(scope string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, scope)
	return nil
}

func settleAccountSync(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for a.busy["account-sync"] || a.busy["account-restore"] || a.busy["account-prune"] || a.busy["account-drop-own"] {
		a.drain()
		if time.Now().After(deadline) {
			t.Fatal("account sync worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAccountSyncRemembersPasswordAndSurvivesTemporaryFailure(t *testing.T) {
	a := fixture(t)
	var failed atomic.Bool
	var mu sync.Mutex
	objects := map[string][]byte{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/github/token" {
			fmt.Fprintf(w, `{"sync_token":"fixture-token","login":"fixture-user","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
			return
		}
		if failed.Load() {
			http.Error(w, "fixture failure", http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/capabilities":
			io.WriteString(w, `{"account_backups":1}`)
		case r.URL.Path == "/v1/objects":
			names := []string{}
			for name := range objects {
				names = append(names, name)
			}
			_ = json.NewEncoder(w).Encode(names)
		case strings.HasPrefix(r.URL.Path, "/v1/objects/"):
			name := strings.TrimPrefix(r.URL.Path, "/v1/objects/")
			if r.Method == "PUT" {
				objects[name], _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusNoContent)
			} else if raw, ok := objects[name]; ok {
				_, _ = w.Write(raw)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if err := syncclient.CompleteLogin(context.Background(), ts.URL, syncclient.Authorization{DeviceCode: "fixture", ExpiresIn: 60}); err != nil {
		t.Fatal(err)
	}
	passwords := &memoryAccountPasswords{values: map[string]string{}}
	a.accountPasswords = passwords
	a.openAccountBackup("sync")
	a.backupUI.password = "fixture123"
	a.runAccountBackup()
	settleAccountSync(t, a)
	if a.accountSync.failed || a.accountSync.lastAt.IsZero() {
		t.Fatal("setup did not start syncing")
	}
	scope, err := syncclient.AccountSyncScope()
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := passwords.Get(scope); err != nil || saved != "fixture123" {
		t.Fatal("successful setup did not remember password")
	}
	a.lockAccountSync()
	a.syncAccounts()
	settleAccountSync(t, a)
	if a.accountSync.password != "fixture123" || a.accountSync.failed {
		t.Fatal("restart required typing the remembered password again")
	}
	failed.Store(true)
	a.syncAccounts()
	settleAccountSync(t, a)
	if !a.accountSync.failed || a.accountSync.password != "fixture123" {
		t.Fatal("temporary sync error discarded the password")
	}
	failed.Store(false)
	a.syncAccounts()
	settleAccountSync(t, a)
	if a.accountSync.failed {
		t.Fatal("retry did not recover")
	}
	// Changing the password reseals this device's snapshot, updates the
	// remembered password, and the next start keeps syncing without it.
	a.openAccountBackup("sync")
	a.backupUI.password = "rotated-passphrase"
	a.backupUI.confirm = a.backupUI.password
	a.runAccountBackup()
	settleAccountSync(t, a)
	if a.accountSync.failed || a.accountSync.prevPassword != "" {
		t.Fatalf("password change did not complete: %s", a.accountSync.message)
	}
	if saved, err := passwords.Get(scope); err != nil || saved != "rotated-passphrase" {
		t.Fatal("password change did not update the remembered password")
	}
	a.lockAccountSync()
	a.syncAccounts()
	settleAccountSync(t, a)
	if a.accountSync.failed || a.accountSync.password != "rotated-passphrase" {
		t.Fatal("sync after a password change required the old password")
	}
	if err := a.disableAccountSync(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := passwords.Get(scope)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turning sync off retained the saved password")
		}
		time.Sleep(time.Millisecond)
	}
	if a.accountSync.password != "" || syncclient.ReadAccountSyncConfig().Enabled {
		t.Fatal("turning off sync retained its enabled state")
	}
}

func TestAccountSyncSettingsAreReadable(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	a.settings = true
	if err := a.accountStore.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: "https://sync.example.test"}); err != nil {
		t.Fatal(err)
	}
	dir, _ := os.UserConfigDir()
	dir = filepath.Join(dir, "devin-usage-metrics")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-sync-session.meta.json"), []byte(`{"login":"fixture-user"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: "fixture-scope", Remember: true}); err != nil {
		t.Fatal(err)
	}
	a.accountSync = accountSyncState{password: "fixture123", scope: "fixture-scope", lastAt: time.Now().Add(-2 * time.Minute), result: syncclient.AccountSyncResult{SyncResult: accounts.SyncResult{Total: 3}, Devices: 2, Unreadable: 1}}
	tt := ui.NewTester(func(c *ui.Context) { a.theme(c); a.settingsView(c); a.accountBackupDialog(c) }, 800, 780)
	for _, old := range []string{"锁定", "停用", "保留本机", "加密账号同步", "新增 0 个"} {
		if tt.HasText(old) {
			t.Fatalf("unnecessary default control or copy: %s", old)
		}
	}
	if !tt.HasText("已同步 3 个账号 · 来自 2 台设备 · 2 分钟前") {
		t.Fatal("sync status does not tell the user what is synchronized")
	}
	if !tt.HasText("1 台设备的快照无法用当前密码解开") {
		t.Fatal("an unreadable device snapshot is not reported")
	}
	capture(t, tt, "go-account-sync-ready")
	// With sync enabled and the password known, the dialog rotates the password.
	a.openAccountBackup("sync")
	tt.Frame()
	if !tt.HasText("更改同步密码") || !tt.HasText("新的同步密码") || !tt.HasText("再次输入口令") {
		t.Fatal("changing the password shows the wrong dialog")
	}
	capture(t, tt, "go-account-sync-change")
}

func TestSystemAccountPasswordRoundTrip(t *testing.T) {
	if os.Getenv("AUM_NATIVE_SECRETS") != "1" || runtime.GOOS != "windows" {
		t.Skip("explicit native Windows credential-store acceptance")
	}
	store := systemAccountPasswords{}
	scope := fmt.Sprintf("test-%d", time.Now().UnixNano())
	defer store.Delete(scope)
	if err := store.Set(scope, "fixture123"); err != nil {
		t.Fatal("system credential store write failed")
	}
	value, err := store.Get(scope)
	if err != nil || value != "fixture123" {
		t.Fatal("system credential store round trip failed")
	}
	if err := store.Delete(scope); err != nil {
		t.Fatal("system credential store cleanup failed")
	}
	if _, err := store.Get(scope); err == nil {
		t.Fatal("deleted system credential was still readable")
	}
}

func TestRelativeSyncTime(t *testing.T) {
	a := fixture(t)
	for _, tc := range []struct {
		ago time.Duration
		en  string
		zh  string
	}{
		{0, "just now", "刚刚"},
		{-30 * time.Second, "just now", "刚刚"},
		{-5 * time.Minute, "5 min ago", "5 分钟前"},
		{-3 * time.Hour, "3 hr ago", "3 小时前"},
		{-2 * 24 * time.Hour, "2 days ago", "2 天前"},
	} {
		at := time.Now().Add(tc.ago)
		a.lang = "en"
		if got := a.relativeSyncTime(at); got != tc.en {
			t.Fatalf("en relative(%v) = %q, want %q", tc.ago, got, tc.en)
		}
		a.lang = "zh"
		if got := a.relativeSyncTime(at); got != tc.zh {
			t.Fatalf("zh relative(%v) = %q, want %q", tc.ago, got, tc.zh)
		}
	}
	if a.relativeSyncTime(time.Time{}) != "" {
		t.Fatal("zero time renders as a relative string")
	}
}

func TestAccountBackupErrorMapping(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{accounts.ErrBackupPassword, "密码无法解锁云端账号"},
		{accounts.ErrShortPassword, "至少 10 个字符"},
		{fmt.Errorf("dial failed: %w", syncclient.ErrTransport), "暂时无法连接同步服务器"},
		{context.DeadlineExceeded, "暂时无法连接同步服务器"},
		{syncclient.ErrNotSignedIn, "重新登录 GitHub"},
		{syncclient.ErrSyncAborted, "账号同步被关闭或账号发生变更"},
		{syncclient.ErrServerTooOld, "升级同步服务器"},
		{syncclient.ErrTooManyDevices, "最多 64 台"},
		{syncclient.ErrTooManyRemote, "最多 512 个"},
		{accounts.ErrTooManySavedAccounts, "最多 512 个"},
		{accounts.ErrBackupFileConflict, "重启应用后再次同步"},
		{syncclient.APIStatusError{Code: 507}, "同步服务器出现问题"},
	} {
		if got := a.accountBackupError(tc.err); !strings.Contains(got, tc.want) {
			t.Fatalf("accountBackupError(%v) = %q, want it to contain %q", tc.err, got, tc.want)
		}
	}
	if got := a.accountBackupError(errors.New("mystery failure")); got != "mystery failure" {
		t.Fatal("unknown errors should pass through as raw text")
	}
	if d := a.accountBackupErrorDetail(syncclient.ErrTooManyDevices); d != "" {
		t.Fatal("sentinel errors need no raw detail line")
	}
	if d := a.accountBackupErrorDetail(fmt.Errorf("dial failed: %w", syncclient.ErrTransport)); d == "" {
		t.Fatal("wrapped errors should expose their raw text as detail")
	}
}

func TestAccountSyncDisableRequiresConfirmation(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	a.settings = true
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: "fixture-scope", Remember: true}); err != nil {
		t.Fatal(err)
	}
	a.accountSync = accountSyncState{password: "fixture123", scope: "fixture-scope"}
	tt := ui.NewTester(func(c *ui.Context) { a.theme(c); a.settingsView(c); a.accountBackupDialog(c) }, 800, 780)
	a.accountSync.pendingOff = true
	tt.Frame()
	if !tt.HasText("关闭账号同步？") {
		t.Fatal("turning account sync off has no confirmation dialog")
	}
	// The cancel path clears the armed flag without touching sync state; the
	// alert's own Cancel is unclickable here because the tester's Find hits
	// the settings dialog's identical button underneath the modal first.
	a.accountSync.pendingOff = false
	tt.Frame()
	if tt.HasText("关闭账号同步？") || !syncclient.ReadAccountSyncConfig().Enabled {
		t.Fatal("canceling the confirmation changed sync state")
	}
	a.accountSync.pendingOff = true
	tt.Frame()
	click(t, tt, "关闭同步")
	if syncclient.ReadAccountSyncConfig().Enabled || a.accountSync.password != "" {
		t.Fatal("confirming the dialog did not turn account sync off")
	}
	// Sync is off and switching is off: the dialog is first-time setup again,
	// disclosing the account switching requirement.
	a.openAccountBackup("sync")
	tt.Frame()
	if !tt.HasText("设置账号同步") || !tt.HasText("同时开启账号切换") {
		t.Fatal("first setup does not disclose the account switching requirement")
	}
}

func TestAccountsDialogShowsSyncStatus(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	if err := a.accountStore.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	a.accountUI.manager = true
	tt := ui.NewTester(a.View, 800, 780)
	if !tt.HasText("账号同步未开启，点击到设置中开启。") {
		t.Fatal("the accounts dialog hides that account sync exists")
	}
	click(t, tt, "账号同步未开启，点击到设置中开启。")
	if !a.settings || a.settingsUI.tab != "sync" {
		t.Fatal("the off row did not open settings on the sync tab")
	}
	a.settings, a.settingsUI.open = false, false
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: "fixture-scope"}); err != nil {
		t.Fatal(err)
	}
	a.accountSync = accountSyncState{password: "fixture123", scope: "fixture-scope", lastAt: time.Now().Add(-2 * time.Minute), result: syncclient.AccountSyncResult{SyncResult: accounts.SyncResult{Total: 3}, Devices: 2}}
	tt.Frame()
	if !tt.HasText("3 个账号 · 2 台设备 · 2 分钟前") || !tt.HasText("立即同步") {
		t.Fatal("the accounts dialog does not show the sync status row")
	}
	capture(t, tt, "go-account-sync-manager-row")
	// Without the password the row offers the unlock dialog, titled as unlock.
	a.accountSync.password = ""
	tt.Frame()
	click(t, tt, "输入密码")
	if !tt.HasText("解锁账号同步") || !tt.HasText("解锁并同步") {
		t.Fatal("re-entering a password shows the setup dialog instead of unlock")
	}
}
