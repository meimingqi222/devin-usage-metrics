package desktop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

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
}

type accountSyncState struct {
	password, scope, message string
	failed                   bool
	cancel                   context.CancelFunc
	generation               uint64
}

func (a *App) openAccountBackup(mode string) {
	if a.busy["account-backup"] {
		return
	}
	a.backupUI = accountBackupUI{open: true, mode: mode, generation: a.backupUI.generation + 1}
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
	a.lockAccountSync()
	return syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{})
}

func (a *App) syncAccounts() {
	if a.busy["account-sync"] || a.accountSync.password == "" {
		return
	}
	scope, err := syncclient.AccountSyncScope()
	consent := syncclient.ReadAccountSyncConfig()
	if err != nil || !consent.Enabled || consent.Scope != scope || a.accountSync.scope != scope || !a.accountStore.Enabled() {
		a.lockAccountSync()
		return
	}
	password, generation := a.accountSync.password, a.accountSync.generation
	ctx, cancel := context.WithTimeout(a.ctx, syncDeadline)
	a.accountSync.cancel = cancel
	a.accountSync.message, a.accountSync.failed = a.t("Syncing encrypted accounts…", "正在同步加密账号…"), false
	a.work("account-sync", func() func() {
		defer cancel()
		result, err := syncclient.SyncAccounts(ctx, a.accountStore, password, scope)
		return func() {
			if generation != a.accountSync.generation {
				return
			}
			a.accountSync.cancel = nil
			a.accountSync.failed = err != nil
			if err != nil {
				// Clear a failed unlock rather than retrying a wrong password
				// on every automatic sync. Consent remains enabled for retry.
				a.accountSync.password = ""
				a.accountSync.message = a.accountBackupError(err)
			} else {
				a.accountSync.message = fmt.Sprintf(a.t("Accounts synced: %d added, %d kept.", "账号同步完成：新增 %d 个，保留本机 %d 个。"), result.Added, result.Kept)
				a.profiles = a.accountStore.List()
			}
		}
	})
}

func (a *App) accountBackupError(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case accounts.ErrBackupPassword.Error():
		return a.t("Incorrect password or damaged account backup.", "口令不正确，或账号备份已损坏。")
	case "use an account backup password of at least 10 characters":
		return a.t("Use a password of at least 10 characters.", "请使用至少 10 个字符的口令。")
	case "update the sync server to support encrypted account backups":
		return a.t("Update your sync server to use encrypted account sync.", "请先升级同步服务器，再使用加密账号同步。")
	default:
		return err.Error()
	}
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
			a.backupUI.message = a.t("Enable account switching first.", "请先启用账号切换。")
			return
		}
		if err := accounts.CheckBackupPassword(draft.password); err != nil {
			a.backupUI.message = a.accountBackupError(err)
			return
		}
		scope, err := syncclient.AccountSyncScope()
		if err == nil {
			err = syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: scope})
		}
		if err != nil {
			a.backupUI.message = a.accountBackupError(err)
			return
		}
		a.lockAccountSync()
		a.accountSync.password, a.accountSync.scope = draft.password, scope
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
			}
		}
	})
}

func (a *App) accountSyncSettings(c *ui.Context) {
	ui.Column(c).Gap(6).BorderWidth(1, 0, 0, 0).BorderColor(c.Theme().Border).Padding(10, 0, 0).Children(func() {
		ui.Text(c, a.t("Encrypted account sync", "加密账号同步")).FontSize(12).Bold()
		status := a.t("Off · enable separately from usage sync", "未启用 · 与用量同步独立开启")
		if syncclient.ReadAccountSyncConfig().Enabled {
			status = a.t("Enabled · unlock after each app restart", "已启用 · 每次启动后需解锁")
		}
		if a.accountSync.message != "" {
			status = a.accountSync.message
		}
		color := c.Theme().TextMuted
		if a.accountSync.failed {
			color = quotaRose
		}
		ui.Text(c, status).FontSize(11).TextColor(color)
		ui.Text(c, a.t("Use the same password on each device. Adds missing backup accounts; existing sign-ins and local deletions stay local.", "每台设备使用同一口令。只合并缺失的备用账号，保留本机已有凭证；删除仅作用于本机。")).FontSize(10.5).TextColor(faint(c.Theme()))
		ui.Row(c).Gap(6).Children(func() {
			endpointChanged := strings.TrimRight(strings.TrimSpace(a.settingsUI.apiURL), "/") != strings.TrimRight(syncclient.ReadConfig().APIURL, "/")
			label := a.t("Enable / unlock", "启用 / 解锁")
			if a.accountSync.password != "" {
				label = a.t("Sync accounts now", "立即同步账号")
			}
			if settingsButton(c, label, true).Disabled(!a.accountStore.Enabled() || syncclient.SessionLabel() == "" || endpointChanged || a.busy["account-sync"]).Clicked() {
				if a.accountSync.password == "" {
					a.openAccountBackup("sync")
				} else {
					a.syncAccounts()
				}
			}
			if a.accountSync.password != "" && settingsButton(c, a.t("Lock", "锁定"), false).Clicked() {
				a.lockAccountSync()
			}
			if syncclient.ReadAccountSyncConfig().Enabled && settingsButton(c, a.t("Disable", "停用"), false).Clicked() {
				if err := a.disableAccountSync(); err != nil {
					a.message = err.Error()
				}
			}
		})
		if !a.accountStore.Enabled() {
			ui.Text(c, a.t("Enable account switching in Manage accounts first.", "请先在“管理账号”中启用账号切换。")).FontSize(10.5).TextColor(faint(c.Theme()))
		}
	})
}

func (a *App) accountBackupDialog(c *ui.Context) {
	if !a.backupUI.open {
		return
	}
	busy := a.busy["account-backup"]
	ui.DialogBase(c.Key("account-backup"), &a.backupUI.open, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(480).MaxHeight(620).Padding(18).Gap(10).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border)
		title := a.t("Import encrypted accounts", "导入加密账号")
		if a.backupUI.mode == "export" {
			title = a.t("Export encrypted accounts", "导出加密账号")
		} else if a.backupUI.mode == "sync" {
			title = a.t("Unlock account sync", "解锁账号同步")
		}
		ui.Text(c, title).FontSize(14).Bold()
		if a.backupUI.mode == "sync" {
			ui.Text(c, syncclient.ReadConfig().APIURL+" · "+syncclient.SessionLabel()).FontSize(11).TextColor(c.Theme().TextMuted)
		}
		ui.Text(c, a.t("The password encrypts account credentials. It is never saved or sent to the server. Keep it safe; it cannot be recovered.", "口令用于加密账号凭证，不会保存或发送给服务器。请妥善保管，遗失后无法找回。")).FontSize(11).TextColor(c.Theme().TextMuted)
		if a.backupUI.mode != "sync" {
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
		confirmPassword := a.backupUI.mode == "export" || a.backupUI.mode == "sync" && !syncclient.ReadAccountSyncConfig().Enabled
		if a.backupUI.preview == nil {
			ui.TextInputBase(c, &a.backupUI.password).Password().Label(a.t("Account backup password", "账号备份口令")).Placeholder(a.t("Password · at least 10 characters", "口令 · 至少 10 个字符")).FillWidth().Padding(8, 10).FontSize(12).Background(c.Theme().Background).Border(1, c.Theme().Border).Radius(4).Disabled(busy)
			if confirmPassword {
				ui.TextInputBase(c, &a.backupUI.confirm).Password().Label(a.t("Confirm backup password", "再次输入口令")).Placeholder(a.t("Confirm password", "再次输入口令")).FillWidth().Padding(8, 10).FontSize(12).Background(c.Theme().Background).Border(1, c.Theme().Border).Radius(4).Disabled(busy)
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
			} else if a.backupUI.mode == "sync" {
				label = a.t("Unlock and sync", "解锁并同步")
			}
			disabled := busy || a.backupUI.mode != "sync" && a.backupUI.file == ""
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
