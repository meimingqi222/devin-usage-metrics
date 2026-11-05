package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

type accountBackupUI struct {
	open                          bool
	mode, file, password, confirm string
	message                       string
	preview                       *accounts.Backup
	generation                    uint64
	remember, showPassword        bool
	// enableSwitching backs the explicit checkbox that appears when account
	// sync needs to turn account switching on; unchecked blocks "Start syncing".
	enableSwitching bool
}

type accountSyncState struct {
	password, scope, message, detail string
	// prevPassword is the password this device used before a rotation; it
	// still opens other devices' old-sealed snapshots while they catch up.
	prevPassword                       string
	failed                             bool
	cancel                             context.CancelFunc
	generation                         uint64
	remember, restored, details, dirty bool
	remembered                         bool
	quotaPending, quotaForce           bool
	authRequired                       bool
	// pendingOff holds the switch-off confirmation; disabling happens only
	// after the user accepts, because it also deletes the remembered password.
	pendingOff bool
	// pendingDropUnreadable / pendingDropForeign confirm pruning remote snapshots.
	pendingDropUnreadable, pendingDropForeign bool
	lastAt                                    time.Time
	result                                    syncclient.AccountSyncResult
}

func (a *App) openAccountBackup(mode string) {
	if a.busy["account-backup"] {
		return
	}
	a.backupUI = accountBackupUI{open: true, mode: mode, remember: true, enableSwitching: true, generation: a.backupUI.generation + 1}
}

func (a *App) closeAccountBackup() {
	a.backupUI = accountBackupUI{generation: a.backupUI.generation + 1}
}

func (a *App) lockAccountSync() {
	if a.accountSync.cancel != nil {
		a.accountSync.cancel()
	}
	a.accountSync = accountSyncState{generation: a.accountSync.generation + 1}
}

func (a *App) disableAccountSync() error {
	scope := syncclient.ReadAccountSyncConfig().Scope
	a.lockAccountSync()
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{}); err != nil {
		return err
	}
	a.forgetAccountPassword(scope)
	a.dropOwnAccountSnapshot()
	return nil
}

func (a *App) dropOwnAccountSnapshot() {
	if a.busy["account-drop-own"] {
		return
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.work("account-drop-own", func() func() {
		err := syncclient.DeleteOwnAccountSnapshotIfDisabled(ctx)
		return func() {
			if err != nil && !errors.Is(err, syncclient.ErrNotSignedIn) && !errors.Is(err, context.Canceled) {
				a.accountSync.detail = err.Error()
			}
		}
	})
}

func (a *App) dropUnreadableAccountSnapshots() {
	if a.busy["account-prune"] || a.accountSync.password == "" {
		return
	}
	password, prev, generation := a.accountSync.password, a.accountSync.prevPassword, a.accountSync.generation
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.work("account-prune", func() func() {
		dropped, err := syncclient.DropUnreadableAccountSnapshots(ctx, password, prev)
		return func() {
			if generation != a.accountSync.generation {
				return
			}
			if err != nil {
				a.accountSync.message, a.accountSync.detail, a.accountSync.failed = a.accountBackupError(err), a.accountBackupErrorDetail(err), true
				return
			}
			a.accountSync.result.Unreadable = 0
			if dropped > 0 {
				a.accountSync.message = fmt.Sprintf(a.t("Removed %d unread snapshot(s).", "已移除 %d 个无法解开的快照。"), dropped)
				a.accountSync.failed = false
			}
			a.syncAccounts()
		}
	})
}

func (a *App) dropForeignAccountSnapshots() {
	if a.busy["account-prune"] {
		return
	}
	generation := a.accountSync.generation
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.work("account-prune", func() func() {
		dropped, err := syncclient.DropForeignAccountSnapshots(ctx)
		return func() {
			if generation != a.accountSync.generation {
				return
			}
			if err != nil {
				a.accountSync.message, a.accountSync.detail, a.accountSync.failed = a.accountBackupError(err), a.accountBackupErrorDetail(err), true
				return
			}
			if dropped > 0 {
				a.accountSync.message = fmt.Sprintf(a.t("Removed %d other device snapshot(s).", "已移除 %d 台其他设备的快照。"), dropped)
				a.accountSync.failed = false
			}
			a.syncAccounts()
		}
	})
}

func (a *App) forgetAccountPassword(scope string) {
	store := a.accountPasswords
	if store == nil || scope == "" {
		return
	}
	go func() {
		a.accountPasswordMu.Lock()
		defer a.accountPasswordMu.Unlock()
		cfg := syncclient.ReadAccountSyncConfig()
		if cfg.Enabled && cfg.Remember && cfg.Scope == scope {
			return
		}
		_ = store.Delete(scope)
	}()
}

func (a *App) restoreAccountSync(scope string) {
	store := a.accountPasswords
	if store == nil || a.accountSync.restored || a.busy["account-restore"] {
		return
	}
	a.accountSync.restored = true
	generation := a.accountSync.generation
	a.work("account-restore", func() func() {
		a.accountPasswordMu.Lock()
		password, err := store.Get(scope)
		a.accountPasswordMu.Unlock()
		return func() {
			cfg := syncclient.ReadAccountSyncConfig()
			current, currentErr := syncclient.AccountSyncScope()
			if generation != a.accountSync.generation || !cfg.Enabled || !cfg.Remember || cfg.Scope != scope || currentErr != nil || current != scope {
				return
			}
			if err != nil || accounts.CheckBackupPassword(password) != nil {
				return
			}
			a.accountSync.password, a.accountSync.scope, a.accountSync.remember = password, scope, true
			a.accountSync.remembered = true
			a.syncAccounts()
		}
	})
}

func (a *App) accountChanged() {
	if a.busy["account-sync"] {
		a.accountSync.dirty = true
		return
	}
	a.syncAccounts()
}

func (a *App) syncAccounts() {
	if a.busy["account-sync"] {
		return
	}
	// A disable confirmation is pending: hold automatic syncs so the dialog
	// cannot be undermined (or dismissed by a state reset) mid-decision.
	if a.accountSync.pendingOff {
		return
	}
	consent := syncclient.ReadAccountSyncConfig()
	if !consent.Enabled || !a.accountStore.Enabled() {
		a.accountSync.dirty = false
		return
	}
	if a.busy["quota"] {
		a.accountSync.dirty = true
		return
	}
	scope, err := syncclient.AccountSyncScope()
	if err != nil {
		a.lockAccountSync()
		a.accountSync.failed, a.accountSync.authRequired = true, true
		a.accountSync.message = a.t("Your sync sign-in has expired. Sign in to GitHub above to continue.", "同步登录已失效，请在上方重新登录 GitHub。")
		return
	}
	if consent.Scope != scope {
		a.lockAccountSync()
		a.accountSync.message = a.t("The sync account changed. Enter its sync password to continue.", "同步账号已更改，请输入对应的同步密码。")
		return
	}
	if a.accountSync.password == "" {
		if consent.Remember {
			a.restoreAccountSync(scope)
		}
		return
	}
	if a.accountSync.scope != scope {
		a.lockAccountSync()
		return
	}
	password, prev, generation := a.accountSync.password, a.accountSync.prevPassword, a.accountSync.generation
	remember, store := a.accountSync.remember && !a.accountSync.remembered, a.accountPasswords
	ctx, cancel := context.WithTimeout(a.ctx, syncDeadline)
	a.accountSync.cancel = cancel
	a.accountSync.message, a.accountSync.detail, a.accountSync.failed = "", "", false
	a.accountSync.dirty = false
	a.work("account-sync", func() func() {
		defer cancel()
		result, err := syncclient.SyncAccounts(ctx, a.accountStore, password, scope, prev)
		var rememberErr error
		rememberConfigured := false
		if store != nil && ctx.Err() == nil {
			a.accountPasswordMu.Lock()
			cfg := syncclient.ReadAccountSyncConfig()
			rememberConfigured = cfg.Remember
			if err == nil && remember && cfg.Enabled && cfg.Remember && cfg.Scope == scope {
				rememberErr = store.Set(scope, password)
			}
			if errors.Is(err, accounts.ErrBackupPassword) {
				_ = store.Delete(scope)
			}
			a.accountPasswordMu.Unlock()
		}
		return func() {
			if generation != a.accountSync.generation {
				return
			}
			a.accountSync.cancel = nil
			a.accountSync.failed = err != nil
			if err != nil {
				if errors.Is(err, accounts.ErrBackupPassword) {
					a.accountSync.password, a.accountSync.prevPassword = "", ""
					if rememberConfigured {
						// The wrong password silently deleting the remembered
						// secret reads as a bug unless it is said out loud.
						a.accountSync.message = a.accountBackupError(err) + " " + a.t("The password remembered on this device was removed.", "已清除本机记住的密码。")
					}
				}
				if a.accountSync.message == "" {
					a.accountSync.message = a.accountBackupError(err)
				}
				a.accountSync.detail = a.accountBackupErrorDetail(err)
			} else {
				a.accountSync.lastAt, a.accountSync.result = time.Now(), result
				a.accountSync.detail = ""
				// This device is done with any rotation it was part of.
				a.accountSync.prevPassword = ""
				if remember && store != nil && rememberErr == nil {
					a.accountSync.remembered = true
				}
				if rememberErr != nil {
					a.accountSync.message = a.t("Synced. Could not save the password on this device.", "已同步，但系统未能记住密码，下次启动时需再次输入。")
				}
				a.profiles = a.accountStore.List()
			}
			if a.accountSync.quotaPending {
				force := a.accountSync.quotaForce
				a.accountSync.quotaPending, a.accountSync.quotaForce = false, false
				a.refreshQuota(force)
			} else if a.accountSync.dirty && err == nil {
				a.syncAccounts()
			}
		}
	})
}

// accountBackupError maps account-sync and backup errors to an actionable,
// translated message. Match by errors.Is; the sentinel message text doubles as
// the syncdiag error code. Unknown errors pass through raw, and
// accountBackupErrorDetail exposes the underlying text for bug reports.
func (a *App) accountBackupError(err error) string {
	if err == nil {
		return ""
	}
	var status syncclient.APIStatusError
	switch {
	case errors.Is(err, accounts.ErrBackupPassword):
		return a.t("The password did not unlock your accounts. Enter the password used on your other devices.", "密码无法解锁云端账号，请输入其他设备使用的同步密码。")
	case errors.Is(err, accounts.ErrShortPassword):
		return a.t("Use a password of at least 10 characters.", "请使用至少 10 个字符的口令。")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, syncclient.ErrTransport):
		return a.t("Could not reach the sync server. Your password is kept, and syncing retries automatically.", "暂时无法连接同步服务器；密码已保留，稍后会自动重试。")
	case errors.Is(err, syncclient.ErrNotSignedIn):
		return a.t("Your GitHub sync sign-in has expired. Sign in to GitHub above to continue.", "GitHub 同步登录已失效，请在上方重新登录 GitHub。")
	case errors.Is(err, syncclient.ErrSyncReenabled), errors.Is(err, syncclient.ErrSyncAborted):
		return a.t("Account sync stopped because it was turned off or its account changed.", "同步已停止：账号同步被关闭或账号发生变更。")
	case errors.Is(err, syncclient.ErrServerTooOld):
		return a.t("Update your sync server to use encrypted account sync.", "请先升级同步服务器，再使用加密账号同步。")
	case errors.Is(err, syncclient.ErrTooManyDevices):
		return a.t("Too many devices are syncing accounts (up to 64). Remove other devices' snapshots in Account sync details.", "同步账号的设备太多（最多 64 台），请在账号同步详情中移除其他设备的快照。")
	case errors.Is(err, syncclient.ErrTooManyRemote), errors.Is(err, accounts.ErrTooManySavedAccounts):
		return a.t("Too many saved accounts to sync (up to 512).", "要同步的账号太多（最多 512 个）。")
	case errors.Is(err, accounts.ErrBackupFileConflict):
		return a.t("A saved account file conflicts with the incoming change. Restart the app and sync again.", "保存的账号文件与同步内容冲突，请重启应用后再次同步。")
	case errors.Is(err, accounts.ErrDisabled):
		return a.t("Enable account switching to sync accounts.", "请先开启账号切换，才能同步账号。")
	case errors.As(err, &status):
		return a.t("The sync server reported a problem. Syncing retries automatically.", "同步服务器出现问题，稍后会自动重试。")
	default:
		return err.Error()
	}
}

// accountBackupErrorDetail returns the raw error text when it carries more
// information than the friendly message: wrapped causes worth a bug report.
// Bare sentinels and passthrough text need no second line.
func (a *App) accountBackupErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	if _, ok := err.(syncclient.APIStatusError); ok {
		return ""
	}
	for _, sentinel := range []error{
		accounts.ErrBackupPassword, accounts.ErrShortPassword, accounts.ErrTooManySavedAccounts,
		accounts.ErrBackupFileConflict, accounts.ErrDisabled,
		syncclient.ErrNotSignedIn, syncclient.ErrSyncReenabled, syncclient.ErrServerTooOld,
		syncclient.ErrTooManyDevices, syncclient.ErrTooManyRemote, syncclient.ErrSyncAborted, syncclient.ErrTransport,
	} {
		if msg == sentinel.Error() {
			return ""
		}
	}
	if msg == a.accountBackupError(err) {
		return ""
	}
	return msg
}

// relativeSyncTime renders a past timestamp honestly for an always-running
// tray app: "3 days ago" where a bare "14:05" would read as current.
func (a *App) relativeSyncTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	hours := int(d.Hours())
	switch {
	case d < time.Minute:
		return a.t("just now", "刚刚")
	case d < time.Hour:
		return fmt.Sprintf(a.t("%d min ago", "%d 分钟前"), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(a.t("%d hr ago", "%d 小时前"), hours)
	default:
		return fmt.Sprintf(a.t("%d days ago", "%d 天前"), hours/24)
	}
}

// fullSyncTime is the absolute timestamp shown on hover over a relative one.
func fullSyncTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// accountSyncStatus is the semantic sync state shared by the settings section
// and the accounts manager row, so the two surfaces never drift apart. Settings
// layers its own preconditions (sign-in, unsaved endpoint) on top.
type accountSyncStatus struct {
	enabled                       bool
	syncing, restoring, preparing bool
	locked                        bool // enabled but running without the password
	failed                        bool
	message, detail               string // failure or notice text overriding the default status
	lastAt                        time.Time
	result                        syncclient.AccountSyncResult
}

func (a *App) accountSyncStatus() accountSyncStatus {
	cfg := syncclient.ReadAccountSyncConfig()
	s := accountSyncStatus{
		enabled: cfg.Enabled,
		locked:  cfg.Enabled && a.accountSync.password == "" && !a.accountSync.authRequired,
		lastAt:  a.accountSync.lastAt,
		result:  a.accountSync.result,
	}
	switch {
	case !cfg.Enabled:
	case a.busy["account-sync"]:
		s.syncing = true
	case a.busy["account-restore"]:
		s.restoring = true
	case a.accountSync.dirty && a.busy["quota"]:
		s.preparing = true
	}
	if a.accountSync.message != "" {
		s.message, s.detail = a.accountSync.message, a.accountSync.detail
		s.failed = a.accountSync.failed
	}
	return s
}

// openSettings shows the settings dialog directly on a tab; a cold open always
// lands on "sync", so callers pass the tab they mean.
func (a *App) openSettings(tab string) {
	a.settings = true
	a.settingsUI.open = true
	a.settingsUI.tab = tab
	a.settingsUI.apiURL = syncclient.ReadConfig().APIURL
}

// accountSyncManagerRow surfaces sync health where accounts are managed. The
// settings section keeps the toggle; this row is passive status plus a quick
// manual sync, for people who never open settings.
func (a *App) accountSyncManagerRow(c *ui.Context) {
	cfg := syncclient.ReadAccountSyncConfig()
	if !cfg.Enabled {
		r := ui.Row(c).Gap(7).Padding(2, 4).Margin(0, -4).Radius(4).AlignItems(ui.Center).Label(a.t("Account sync is off", "账号同步未开启")).Focusable()
		if r.Hovered() {
			r.Background(c.Theme().Text.Alpha(.04))
		}
		if r.Clicked() {
			a.openSettings("sync")
		}
		r.Children(func() {
			ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(faint(c.Theme()))
			ui.Text(c, a.t("Account sync is off. Turn it on in Settings.", "账号同步未开启，点击到设置中开启。")).FontSize(11).TextColor(faint(c.Theme())).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		})
		return
	}
	st := a.accountSyncStatus()
	busy := st.syncing || st.restoring || st.preparing
	status := a.t("Waiting for the first sync…", "等待首次同步…")
	color := faint(c.Theme())
	switch {
	case st.syncing:
		status, color = a.t("Syncing accounts…", "正在同步账号…"), c.Theme().Accent
	case st.restoring:
		status, color = a.t("Starting automatic sync…", "正在恢复自动同步…"), c.Theme().Accent
	case st.preparing:
		status, color = a.t("Preparing account sync…", "正在准备同步账号…"), c.Theme().Accent
	case st.failed:
		status, color = st.message, quotaRose
	case st.locked:
		status = a.t("Enter your sync password to continue.", "输入同步密码后即可继续。")
	case !st.lastAt.IsZero():
		status = fmt.Sprintf(a.t("%d accounts · %d devices · %s", "%d 个账号 · %d 台设备 · %s"), st.result.Total, st.result.Devices, a.relativeSyncTime(st.lastAt))
		color = syncOnColor
	}
	if st.result.Unreadable > 0 && !st.failed && !st.locked {
		status += fmt.Sprintf(a.t(" · %d device(s) to update", " · %d 台待更新"), st.result.Unreadable)
	}
	r := ui.Row(c).Gap(7).AlignItems(ui.Center).Label(status)
	if !st.lastAt.IsZero() {
		// The row label doubles as hover tooltip with the absolute moment;
		// the text node stays unlabeled so it remains the row's text.
		r.Label(status + " · " + a.t("Last sync ", "上次同步 ") + fullSyncTime(st.lastAt))
	}
	r.Children(func() {
		ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(color)
		if busy {
			ui.Spinner(c).Size(10, 10)
		}
		ui.Text(c, status).FontSize(11).TextColor(color).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		if st.locked {
			if settingsButton(c, a.t("Enter password", "输入密码"), true).Clicked() {
				a.openAccountBackup("sync")
			}
			return
		}
		if settingsButton(c, a.t("Sync now", "立即同步"), false).Disabled(busy).Clicked() {
			a.syncAccounts()
		}
	})
}

func (a *App) chooseAccountBackupFile() {
	if a.accountBackupFile == nil {
		return
	}
	save, generation, choose := a.backupUI.mode == "export", a.backupUI.generation, a.accountBackupFile
	title := a.t("Import encrypted accounts", "导入加密账号")
	if save {
		title = a.t("Export encrypted accounts", "导出加密账号")
	}
	a.work("account-backup", func() func() {
		file, err := choose(save, title)
		return func() {
			if generation != a.backupUI.generation {
				return
			}
			if err != nil {
				a.backupUI.message = a.t("Could not choose a backup file.", "无法选择备份文件。")
			} else if file != "" {
				a.backupUI.file, a.backupUI.preview = file, nil
			}
		}
	})
}

func (a *App) runAccountBackup() {
	if a.busy["account-backup"] {
		return
	}
	draft := a.backupUI
	if draft.mode == "sync" {
		if !a.accountStore.Enabled() {
			if err := a.accountStore.SetEnabled(true); err != nil {
				a.backupUI.message = err.Error()
				return
			}
		}
		if err := accounts.CheckBackupPassword(draft.password); err != nil {
			a.backupUI.message = a.accountBackupError(err)
			return
		}
		scope, err := syncclient.AccountSyncScope()
		if err == nil {
			err = syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: scope, Remember: draft.remember})
		}
		if err != nil {
			a.backupUI.message = a.accountBackupError(err)
			return
		}
		prev := ""
		if a.accountSync.password != "" && a.accountSync.scope == scope {
			// A password change: the previous one still opens the other
			// devices' snapshots while they wait for the new password.
			prev = a.accountSync.password
		}
		a.lockAccountSync()
		a.accountSync.password, a.accountSync.scope = draft.password, scope
		a.accountSync.prevPassword = prev
		a.accountSync.remember, a.accountSync.restored = draft.remember, true
		a.accountSync.remembered = false
		if !draft.remember {
			a.forgetAccountPassword(scope)
		}
		a.closeAccountBackup()
		a.syncAccounts()
		return
	}
	if draft.file == "" {
		return
	}
	a.work("account-backup", func() func() {
		var preview accounts.Backup
		var added, kept int
		var err error
		switch {
		case draft.mode == "export":
			var b accounts.Backup
			b, err = a.accountStore.Snapshot()
			if err == nil {
				err = accounts.WriteBackupFile(draft.file, b, draft.password)
			}
		case draft.preview == nil:
			preview, err = accounts.ReadBackupFile(draft.file, draft.password)
		default:
			added, kept, err = a.accountStore.ImportBackup(*draft.preview)
		}
		return func() {
			if draft.generation != a.backupUI.generation {
				return
			}
			if err != nil {
				a.backupUI.message = a.accountBackupError(err)
				return
			}
			if draft.mode == "import" && draft.preview == nil {
				a.backupUI.preview, a.backupUI.password = &preview, ""
				a.backupUI.message = ""
				return
			}
			a.profiles = a.accountStore.List()
			a.closeAccountBackup()
			if draft.mode == "export" {
				a.message = a.t("Encrypted account backup saved.", "已保存加密账号备份。")
			} else {
				a.message = fmt.Sprintf(a.t("Imported %d accounts; kept %d existing accounts.", "已导入 %d 个账号，保留本机已有的 %d 个账号。"), added, kept)
				a.accountChanged()
			}
		}
	})
}

func (a *App) accountSyncSettings(c *ui.Context) {
	cfg := syncclient.ReadAccountSyncConfig()
	endpointChanged := strings.TrimRight(strings.TrimSpace(a.settingsUI.apiURL), "/") != strings.TrimRight(syncclient.ReadConfig().APIURL, "/")
	signedIn := syncclient.SessionLabel() != ""
	ready := signedIn && !endpointChanged && !a.accountSync.authRequired
	st := a.accountSyncStatus()
	ui.Column(c).Gap(8).BorderWidth(1, 0, 0, 0).BorderColor(c.Theme().Border).Padding(12, 0, 0).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(12).Children(func() {
			ui.Text(c, a.t("Account sync", "账号同步")).FontSize(12).Bold().Grow(1)
			enabled := cfg.Enabled
			ui.Switch(c, &enabled).Label(a.t("Account sync", "账号同步")).Disabled(!ready && !cfg.Enabled)
			if enabled != cfg.Enabled {
				if enabled {
					a.openAccountBackup("sync")
				} else {
					// Disabling also deletes this device's remembered password;
					// confirm before it happens instead of doing it silently.
					a.accountSync.pendingOff = true
				}
			}
		})
		syncing := st.syncing || st.restoring || st.preparing
		status := a.t("Keep your saved accounts up to date across devices.", "自动同步各设备上的账号，新增、修改和删除都会同步。")
		switch {
		case !signedIn:
			status = a.t("Sign in to GitHub above to sync accounts.", "先登录上方的 GitHub，即可同步账号。")
		case endpointChanged:
			status = a.t("Save the server address before syncing.", "请先保存修改后的同步地址。")
		case st.syncing:
			status = a.t("Syncing accounts…", "正在同步账号…")
		case st.restoring:
			status = a.t("Starting automatic sync…", "正在恢复自动同步…")
		case st.preparing:
			status = a.t("Preparing account sync…", "正在准备同步账号…")
		case st.failed:
			status = st.message
		case st.message != "":
			status = st.message
		case st.locked:
			status = a.t("Enter your sync password to continue.", "输入同步密码后即可继续。")
		case st.enabled && !st.lastAt.IsZero():
			status = fmt.Sprintf(a.t("%d accounts synced · %d devices · %s", "已同步 %d 个账号 · 来自 %d 台设备 · %s"), st.result.Total, st.result.Devices, a.relativeSyncTime(st.lastAt))
		}
		color := c.Theme().TextMuted
		if st.failed {
			color = quotaRose
		}
		statusRow := ui.Row(c).Gap(8).AlignItems(ui.Center).Label(status)
		if st.enabled && !st.lastAt.IsZero() {
			// Row label doubles as hover tooltip with the absolute moment.
			statusRow.Label(status + " · " + a.t("Last sync ", "上次同步 ") + fullSyncTime(st.lastAt))
		}
		statusRow.Children(func() {
			if syncing {
				ui.Spinner(c).Size(12, 12)
			}
			ui.Text(c, status).FontSize(11).TextColor(color).Grow(1).MinWidth(0)
		})
		if st.failed && st.detail != "" {
			ui.Text(c, st.detail).FontSize(10).TextColor(faint(c.Theme())).SingleLine().Ellipsis("…")
		}
		if st.result.Unreadable > 0 && !st.failed && !st.locked {
			ui.Text(c, fmt.Sprintf(a.t("Snapshots from %d device(s) could not be read with the current password; they may still use the old one. Enter the new password there to finish the change.", "%d 台设备的快照无法用当前密码解开，可能仍在使用旧密码；在那边输入新密码即完成更换。"), st.result.Unreadable)).FontSize(10.5).TextColor(c.Theme().TextMuted)
			if settingsButton(c, a.t("Remove unread snapshots", "移除无法解开的快照"), false).Disabled(a.busy["account-prune"]).Clicked() {
				a.accountSync.pendingDropUnreadable = true
			}
		}
		ui.Row(c).Gap(8).Children(func() {
			if cfg.Enabled && ready {
				label := a.t("Sync now", "立即同步")
				if st.failed {
					label = a.t("Retry", "重试")
				}
				if a.accountSync.password == "" {
					label = a.t("Enter password", "输入密码")
				}
				if settingsButton(c, label, a.accountSync.password == "").Disabled(syncing).Clicked() {
					if a.accountSync.password == "" {
						a.openAccountBackup("sync")
					} else {
						a.syncAccounts()
					}
				}
				if settingsButton(c, a.t("Details", "详情"), false).Clicked() {
					a.accountSync.details = !a.accountSync.details
				}
			}
		})
		if cfg.Enabled && a.accountSync.details {
			if !st.lastAt.IsZero() {
				r := st.result
				ui.Text(c, fmt.Sprintf(a.t("Last sync: %d added, %d updated, %d removed, from %d devices.", "上次同步：新增 %d 个，更新 %d 个，删除 %d 个，来自 %d 台设备。"), r.Added, r.Updated, r.Removed, r.Devices)).FontSize(10.5).TextColor(faint(c.Theme()))
				if r.Protected > 0 {
					ui.Text(c, a.t("Accounts currently in use were left untouched. They will update after you switch accounts.", "正在使用的账号保持原登录，切换后会继续同步更新。")).FontSize(10.5).TextColor(c.Theme().TextMuted)
				}
			}
			ui.Text(c, a.t("Saved account changes sync automatically, about once a minute while the app runs.", "同步只管理保存的账号，应用运行期间每分钟左右自动同步一次。")).FontSize(10.5).TextColor(faint(c.Theme()))
			ui.Text(c, a.t("Your current agent sign-ins stay unchanged.", "Agent 当前的登录不会被切换。")).FontSize(10.5).TextColor(faint(c.Theme()))
			if settingsButton(c, a.t("Sync password settings", "同步密码设置"), false).Clicked() {
				a.openAccountBackup("sync")
			}
			if st.failed && strings.Contains(st.message, "64") {
				if settingsButton(c, a.t("Remove other devices' snapshots", "移除其他设备的快照"), false).Disabled(a.busy["account-prune"]).Clicked() {
					a.accountSync.pendingDropForeign = true
				}
			}
		}
	})
}

func (a *App) accountBackupDialog(c *ui.Context) {
	if !a.backupUI.open {
		return
	}
	busy := a.busy["account-backup"]
	cfg := syncclient.ReadAccountSyncConfig()
	// The same password dialog serves first-time setup, re-entering a password
	// the app no longer holds, and rotating to a new one; only the wording and
	// the confirm field differ.
	syncMode := a.backupUI.mode == "sync"
	unlock := syncMode && cfg.Enabled && a.accountSync.password == ""
	change := syncMode && cfg.Enabled && a.accountSync.password != ""
	needSwitching := syncMode && !a.accountStore.Enabled() && !a.backupUI.enableSwitching
	ui.DialogBase(c.Key("account-backup"), &a.backupUI.open, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(480).MaxHeight(620).Padding(18).Gap(10).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border)
		title := a.t("Import encrypted accounts", "导入加密账号")
		if a.backupUI.mode == "export" {
			title = a.t("Export encrypted accounts", "导出加密账号")
		} else if change {
			title = a.t("Change sync password", "更改同步密码")
		} else if unlock {
			title = a.t("Unlock account sync", "解锁账号同步")
		} else if syncMode {
			title = a.t("Set up account sync", "设置账号同步")
		}
		ui.Text(c, title).FontSize(14).Bold()
		if syncMode {
			ui.Text(c, syncclient.ReadConfig().APIURL+" · "+syncclient.SessionLabel()).FontSize(11).TextColor(c.Theme().TextMuted)
			switch {
			case change:
				ui.Text(c, a.t("Devices still using the old password ask for the new one at their next sync; until then their snapshots cannot be read across.", "仍使用旧密码的设备会在下次同步时要求输入新密码；在那之前，新旧快照互相无法读取。")).FontSize(11).TextColor(c.Theme().TextMuted)
			case unlock:
				ui.Text(c, a.t("Enter the sync password used on your other devices.", "输入其他设备使用的同步密码。")).FontSize(11).TextColor(c.Theme().TextMuted)
			default:
				ui.Text(c, a.t("Use the same password on your devices. Accounts sync automatically once this is set up.", "各设备使用同一个同步密码，设置一次后自动同步。")).FontSize(11).TextColor(c.Theme().TextMuted)
			}
		} else {
			ui.Text(c, a.t("Use this password to open the backup on another device.", "在其他设备导入备份时，需要输入这个密码。")).FontSize(11).TextColor(c.Theme().TextMuted)
		}
		if !syncMode {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				label := a.t("Choose backup file", "选择备份文件")
				if a.backupUI.file != "" {
					label = filepath.Base(a.backupUI.file)
				}
				ui.Text(c, label).FontSize(11).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
				if settingsButton(c, a.t("Browse…", "浏览…"), false).Disabled(busy || a.accountBackupFile == nil).Clicked() {
					a.chooseAccountBackupFile()
				}
			})
		}
		confirmPassword := a.backupUI.mode == "export" || change
		if a.backupUI.preview == nil {
			passwordLabel := a.t("Account backup password", "同步或备份密码")
			switch {
			case change:
				passwordLabel = a.t("New sync password", "新的同步密码")
			case syncMode:
				passwordLabel = a.t("Sync password", "同步密码")
			}
			input := ui.TextInputBase(c, &a.backupUI.password).Label(passwordLabel).Placeholder(a.t("Password · at least 10 characters", "密码 · 至少 10 个字符")).FillWidth().Padding(8, 10).FontSize(12).Background(c.Theme().Background).Border(1, c.Theme().Border).Radius(4).Disabled(busy)
			if !a.backupUI.showPassword {
				input.Password()
			}
			ui.Checkbox(c, &a.backupUI.showPassword, a.t("Show password", "显示密码")).FontSize(11)
			if confirmPassword {
				ui.TextInputBase(c, &a.backupUI.confirm).Password().Label(a.t("Confirm backup password", "再次输入口令")).Placeholder(a.t("Confirm password", "再次输入口令")).FillWidth().Padding(8, 10).FontSize(12).Background(c.Theme().Background).Border(1, c.Theme().Border).Radius(4).Disabled(busy)
			}
			if syncMode && (!unlock || !cfg.Remember) {
				ui.Checkbox(c, &a.backupUI.remember, a.t("Remember on this device", "在此设备记住密码")).FontSize(11)
				ui.Text(c, a.t("Stored in your system credential store for automatic sync after restarting.", "保存到系统凭据库，下次启动自动同步。")).FontSize(10.5).TextColor(faint(c.Theme()))
			}
			if syncMode && !a.accountStore.Enabled() {
				// Sync manages saved copies of sign-ins, so enabling it turns
				// account switching on; make that side effect explicit.
				ui.Checkbox(c, &a.backupUI.enableSwitching, a.t("Enable account switching (required for sync)", "同时开启账号切换（账号同步需要）")).FontSize(11)
				ui.Text(c, a.t("Sync keeps saved copies of the agents' sign-ins on this device, which account switching manages.", "账号同步会在本机保存 Agent 登录的副本，由账号切换管理。")).FontSize(10.5).TextColor(faint(c.Theme()))
			}
		} else {
			ui.Text(c, fmt.Sprintf(a.t("%d accounts in this backup", "备份包含 %d 个账号"), len(a.backupUI.preview.Entries))).FontSize(12).Bold()
			ui.Scroll(c).MaxHeight(200).Children(func() {
				ui.Column(c).Gap(5).Children(func() {
					for _, entry := range a.backupUI.preview.Entries {
						ui.Text(c, providerName(entry.Provider)+" · "+entry.Label()).FontSize(11).TextColor(c.Theme().TextMuted)
					}
				})
			})
			ui.Text(c, a.t("Import adds missing backup accounts only. Existing credentials and current agent sign-ins stay as they are.", "导入只增加缺失的备用账号，保留本机已有凭证及 Agent 当前登录。")).FontSize(11).TextColor(c.Theme().TextMuted)
		}
		if a.backupUI.message != "" {
			ui.Text(c, a.backupUI.message).FontSize(11).TextColor(quotaRose)
		}
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if settingsButton(c, a.t("Cancel", "取消"), false).Disabled(busy).Clicked() {
				a.closeAccountBackup()
				return
			}
			label := a.t("Decrypt preview", "解密并预览")
			if a.backupUI.preview != nil {
				label = a.t("Import backup accounts", "导入备用账号")
			} else if a.backupUI.mode == "export" {
				label = a.t("Export encrypted backup", "导出加密备份")
			} else if change {
				label = a.t("Change password", "更改密码")
			} else if unlock {
				label = a.t("Unlock and sync", "解锁并同步")
			} else if syncMode {
				label = a.t("Start syncing", "开始同步")
			}
			disabled := busy || needSwitching || a.backupUI.mode != "sync" && a.backupUI.file == ""
			if a.backupUI.preview == nil {
				disabled = disabled || accounts.CheckBackupPassword(a.backupUI.password) != nil || confirmPassword && a.backupUI.password != a.backupUI.confirm
			}
			if settingsButton(c, label, true).Disabled(disabled).Clicked() {
				a.runAccountBackup()
			}
		})
	})
	if !a.backupUI.open {
		a.closeAccountBackup()
	}
}
