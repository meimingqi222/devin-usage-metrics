package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

type accountMessage struct {
	text string
	err  bool
}

type accountUIState struct {
	menuOpen      bool
	menuProvider  string
	menuCard      string
	switching     map[string]bool
	manager       bool
	enabled       bool
	add           string
	confirmOpen   bool
	confirmID     string
	confirmWho    string
	renameID      string
	renameText    string
	removeAll     bool
	showHidden    bool
	managerMsg    string
	managerErr    bool
	message       map[string]accountMessage
	loginProvider string
	loginStatus   string
	loginURL      string
	loginCancel   context.CancelFunc
}

var accountProviders = []string{"claude", "codex", "antigravity", "grok"}

func (a *App) profilesFor(provider string) []accounts.Profile {
	var out []accounts.Profile
	for _, p := range a.profiles {
		if p.Provider == provider && !p.Hidden {
			out = append(out, p)
		}
	}
	return out
}

func (a *App) allProfilesFor(provider string) []accounts.Profile {
	var out []accounts.Profile
	for _, p := range a.profiles {
		if p.Provider == provider {
			out = append(out, p)
		}
	}
	return out
}

// profileName keeps two accounts with the same detected name apart in the
// menu, even before the user renames one.
func (a *App) profileName(provider string, p accounts.Profile) string {
	name := p.DisplayName()
	count := 0
	for _, q := range a.profilesFor(provider) {
		if q.DisplayName() == name {
			count++
		}
	}
	if count > 1 && len(p.ID) >= 4 {
		return name + " (" + p.ID[:4] + ")"
	}
	return name
}

func (a *App) liveAccount(provider string) (quota.Account, bool) {
	for _, ar := range a.accounts {
		if ar.account.Provider == provider && ar.isLive {
			return ar.account, true
		}
	}
	for _, ar := range a.accounts {
		if ar.account.Provider == provider {
			return ar.account, true
		}
	}
	return quota.Account{}, false
}

func (a *App) setAccountMessage(provider, text string, err bool) {
	if a.accountUI.message == nil {
		a.accountUI.message = map[string]accountMessage{}
	}
	if text == "" {
		delete(a.accountUI.message, provider)
		return
	}
	a.accountUI.message[provider] = accountMessage{text: text, err: err}
}

func (a *App) openAccounts() {
	a.settings = false
	a.settingsUI.open = false
	a.accountUI.enabled = a.accountStore.Enabled()
	a.accountUI.menuOpen = false
	a.accountUI.renameID, a.accountUI.renameText = "", ""
	a.accountUI.managerMsg, a.accountUI.managerErr = "", false
	a.accountUI.manager = true
}

func switchableProvider(provider string) bool {
	switch provider {
	case "claude", "codex", "antigravity", "grok":
		return true
	}
	return false
}

func (a *App) openAddAccount(provider string) {
	if !switchableProvider(provider) {
		return
	}
	a.accountUI.add = provider
	a.accountUI.manager = false
}

func (a *App) switchAccount(provider, id string) {
	live, ok := a.liveAccount(provider)
	if !ok {
		a.setAccountMessage(provider, a.t("Sign in to this agent first; no live account was detected.", "请先在该 Agent 中登录，目前未检测到可用账号。"), true)
		return
	}
	if provider == "antigravity" {
		a.accountUI.menuOpen = false
		for _, p := range a.profiles {
			if p.Provider == provider && p.ID == id {
				a.accountUI.confirmOpen = true
				a.accountUI.confirmID = id
				a.accountUI.confirmWho = p.DisplayName()
				break
			}
		}
		return
	}
	a.switchAccountNow(provider, id, live)
}

func (a *App) switchAccountNow(provider, id string, live quota.Account) {
	if a.accountUI.switching == nil {
		a.accountUI.switching = map[string]bool{}
	}
	if a.accountUI.switching[provider] {
		return
	}
	a.accountUI.switching[provider] = true
	a.accountUI.menuOpen = false
	if provider == "antigravity" {
		a.setAccountMessage(provider, a.t("Switching Antigravity and reconnecting its language service…", "正在切换 Antigravity 并重连语言服务…"), false)
	}
	a.work("switch/"+provider, func() func() {
		var err error
		if provider == "antigravity" {
			// Quitting, restarting and waiting for the client to come back
			// must all fit: the switch worker waits up to 15s for the
			// language service and again for the client itself.
			ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
			defer cancel()
			err = a.accountStore.SwitchApp(ctx, provider, id, live)
		} else {
			err = a.accountStore.Switch(provider, id, live)
		}
		return func() {
			delete(a.accountUI.switching, provider)
			if err != nil {
				a.setAccountMessage(provider, err.Error(), true)
				return
			}
			a.setAccountMessage(provider, a.t("Switched · new sessions use this account.", "已切换，新会话将使用该账号。"), false)
			a.profiles = a.accountStore.List()
			a.refreshQuota(true)
		}
	})
}

func (a *App) accountsConfirm(c *ui.Context) {
	if !a.accountUI.confirmOpen {
		return
	}
	text := fmt.Sprintf(a.t(
		"Switch to %s? Antigravity stays open and its language service reconnects; if that fails it will restart. Save any work first.",
		"切换到 %s？Antigravity 通常保持打开，只重连语言服务；失败时会自动重启。请先保存工作。"), a.accountUI.confirmWho)
	if ui.AlertDialog(c, &a.accountUI.confirmOpen, a.t("Switch Antigravity account", "切换 Antigravity 账号"), text, a.t("Cancel", "取消"), a.t("Quit & Switch", "退出并切换")) == 1 {
		if live, ok := a.liveAccount("antigravity"); ok {
			a.switchAccountNow("antigravity", a.accountUI.confirmID, live)
		}
	}
}

// accountLabel is the card header's account name. It becomes a popover
// trigger only when the provider has more than one saved account.
func (a *App) accountLabel(c *ui.Context, ar accountResult, compact bool) ui.Element {
	t := c.Theme()
	if !ar.isLive && ar.profileID != "" {
		return ui.Text(c, ar.account.Label).FontSize(fontFor(compact, 11, 10)).MinWidth(0).SingleLine().Ellipsis("…").TextColor(faint(t))
	}
	profiles := a.profilesFor(ar.account.Provider)
	switching := a.accountUI.switching[ar.account.Provider]
	needPicker := len(profiles) > 1
	if !needPicker && !switching {
		return ui.Row(c).Gap(fontFor(compact, 6, 4)).AlignItems(ui.Center).Shrink(1).MinWidth(0).Children(func() {
			if ar.isLive {
				ui.Box(c).Size(fontFor(compact, 6, 5), fontFor(compact, 6, 5)).Shrink(0).Radius(3).Background(quotaGreen)
			}
			ui.Text(c, ar.account.Label).FontSize(fontFor(compact, 11, 10)).Shrink(1).MinWidth(0).SingleLine().Ellipsis("…").TextColor(t.TextMuted)
		})
	}
	size := fontFor(compact, 11, 10)
	pill := ui.Row(c.Key("account/"+ar.account.Provider)).MaxWidth(fontFor(compact, 220, 160)).Shrink(1).MinWidth(0).Gap(6).Padding(0, 6).Height(fontFor(compact, 24, 22)).Radius(4).AlignItems(ui.Center).Label(a.t("Account switcher", "账号切换")).Focusable()
	pillBG, pillBorder := controlColor, strongBorder
	if compact {
		pillBG, pillBorder = trayPill, trayBorder
	}
	if pill.Hovered() || (a.accountUI.menuOpen && a.accountUI.menuProvider == ar.account.Provider) {
		pill.Background(pillBG).Border(1, pillBorder)
	}
	pill.Children(func() {
		if switching {
			ui.Spinner(c)
			ui.Text(c, a.t("Switching…", "正在切换…")).FontSize(size).TextColor(t.TextMuted).NoWrap()
			return
		}
		ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(syncOnColor)
		ui.Text(c, ar.account.Label).FontSize(size).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		ui.Text(c, "▾").FontSize(9).Shrink(0).TextColor(faint(t))
	})
	if !switching && pill.Clicked() {
		if a.accountUI.menuOpen && a.accountUI.menuCard == ar.account.Key {
			a.accountUI.menuOpen = false
		} else {
			a.accountUI.menuProvider = ar.account.Provider
			a.accountUI.menuCard = ar.account.Key
			a.accountUI.menuOpen = true
		}
	}
	if !switching && a.accountUI.menuOpen && a.accountUI.menuCard == ar.account.Key {
		ui.PopoverBase(c, pill, &a.accountUI.menuOpen, func(panel ui.Element) {
			a.accountMenu(c, panel, ar, compact)
		})
	}
	return pill
}

func (a *App) accountMenu(c *ui.Context, panel ui.Element, ar accountResult, compact bool) {
	t := c.Theme()
	profiles := a.profilesFor(ar.account.Provider)
	w := fontFor(compact, 280, 238)
	bg, border := t.Surface, strongBorder
	if compact {
		bg, border = trayCard, trayBorder
	}
	panel.Width(w).Padding(4).Gap(2).Radius(6).Background(bg).Border(1, border).Shadow(0, 8, 24, 0, ui.Hex("#000000").Alpha(.55))
	panel.Children(func() {
		if !a.accountStore.Enabled() {
			ui.Text(c, a.t("Account switching is off. It stores account names and, only after you enable it, sign-ins on this device.", "账号切换未启用。启用后才在本机保存账号名称和登录信息。")).FontSize(10.5).TextColor(t.TextMuted).Padding(6, 8)
			if menuButton(c, a.t("Enable account switching…", "启用账号切换…"), compact).Clicked() {
				a.accountUI.menuOpen = false
				a.openAccounts()
			}
			return
		}
		for _, p := range profiles {
			a.accountMenuItem(c, ar.account.Provider, p, compact)
		}
		if len(profiles) == 0 {
			ui.Text(c, ar.account.Label).FontSize(11).TextColor(t.Text).Padding(6, 8)
		}
		ui.Box(c).Height(1).Shrink(0).Margin(3, 0).Background(t.Border)
		if menuButton(c, "+ "+a.t("Add account…", "添加账号…"), compact).Clicked() {
			a.accountUI.menuOpen = false
			a.openAccounts()
			a.accountUI.add = ar.account.Provider
		}
		if menuButton(c, a.t("Manage accounts…", "管理账号…"), compact).Clicked() {
			a.accountUI.menuOpen = false
			a.openAccounts()
		}
		ui.Text(c, a.t("Running sessions keep their current account.", "已运行的会话在重启前仍使用原账号。")).FontSize(9).TextColor(faint(t)).Padding(4, 8, 6)
	})
}

func (a *App) accountMenuItem(c *ui.Context, provider string, p accounts.Profile, compact bool) {
	t := c.Theme()
	height := fontFor(compact, 34, 28)
	name := a.profileName(provider, p)
	row := ui.Row(c.Key("account-item/"+p.ID)).FillWidth().Height(height).Shrink(0).Padding(0, 8).Gap(8).Radius(4).AlignItems(ui.Center).Label(name).Focusable()
	switch {
	case p.Active:
		row.Background(t.Accent.Alpha(.12))
	case row.Hovered():
		row.Background(t.SurfaceHover)
	}
	if p.Writable {
		if row.Clicked() {
			a.switchAccount(provider, p.ID)
		}
	}
	row.Children(func() {
		if p.Active {
			ui.Icon(c, checkIcon).Size(11, 11).Shrink(0).TextColor(t.Accent)
		} else {
			ui.Box(c).Size(7, 7).Shrink(0).Radius(3.5).Border(1, faint(t).Alpha(.6))
		}
		ui.Column(c).Grow(1).MinWidth(0).Gap(0).Children(func() {
			text := ui.Text(c, name).FontSize(fontFor(compact, 11, 10)).TextColor(t.Text).SingleLine().Ellipsis("…")
			if p.Active {
				text.TextColor(t.Accent)
			}
			sub := p.Plan
			if !p.Active && p.LastUsedAt > 0 {
				sub = strings.TrimSpace(strings.Join(nonEmpty(sub, fmt.Sprintf(a.t("%s ago", "%s前"), shortAge(time.Since(time.Unix(p.LastUsedAt, 0))))), " · "))
			}
			if !p.Writable && p.Reason != "" {
				sub = p.Reason
			}
			if sub != "" {
				color := faint(t)
				if !p.Writable {
					color = quotaAmber
				}
				ui.Text(c, sub).FontSize(fontFor(compact, 9.5, 9)).TextColor(color).SingleLine().Ellipsis("…")
			}
		})
		if p.Active {
			ui.Text(c, a.t("Active", "当前")).FontSize(9).TextColor(t.Accent).NoWrap()
		}
	})
}

func menuButton(c *ui.Context, label string, compact bool) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).FontSize(fontFor(compact, 11, 10)).FillWidth().Height(fontFor(compact, 28, 24)).Padding(0, 8).Radius(4).AlignItems(ui.Center).Justify(ui.Start)
	b.Children(func() { ui.Text(c, label).NoWrap() })
	if b.Hovered() {
		b.Background(t.SurfaceHover)
	}
	return b
}

func fontFor(compact bool, wide, small float32) float32 {
	if compact {
		return small
	}
	return wide
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// startAccountLogin adds an account without touching the live sign-in.
// Claude and Codex run their own browser login in an isolated config
// directory; Antigravity uses Google OAuth in this process.
func (a *App) startAccountLogin(provider string) {
	if a.accountUI.loginProvider != "" {
		return
	}
	if !a.accountStore.Enabled() {
		a.message = a.t("Enable account switching first.", "请先启用账号切换。")
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	ctx = syncdiag.WithFields(syncdiag.WithRun(ctx), "operation", "account_login", "provider", provider)
	a.accountUI.loginProvider = provider
	a.accountUI.loginStatus = a.t("Waiting for the browser sign-in…", "正在等待浏览器登录…")
	a.accountUI.loginURL = ""
	a.accountUI.loginCancel = cancel
	go func() {
		defer cancel()
		syncdiag.Log(ctx, "account_login_start")
		var doc map[string]any
		var kind string
		var err error
		if provider == "antigravity" {
			doc, kind, err = accounts.AntigravityLogin(ctx, func(raw string) {
				a.post(func() {
					a.accountUI.loginURL = raw
					if a.openURL != nil {
						a.openURL(raw)
					}
				})
			}, func() {
				a.post(func() {
					if ctx.Err() == nil {
						a.accountUI.loginStatus = a.t("Google authorized. Exchanging credentials…", "Google 已授权，正在换取登录凭证…")
					}
				})
			})
		} else {
			doc, kind, err = accounts.CLILogin(ctx, provider)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			syncdiag.Log(ctx, "account_login_save_start")
			_, err = a.accountStore.Add(provider, doc, kind)
		}
		syncdiag.Log(ctx, "account_login_end", "error", syncdiag.ErrorCode(err))
		a.post(func() {
			a.accountUI.loginProvider, a.accountUI.loginStatus, a.accountUI.loginURL = "", "", ""
			a.accountUI.loginCancel = nil
			if err != nil {
				if errors.Is(err, context.Canceled) {
					a.message = a.t("Sign-in canceled.", "已取消登录。")
				} else if errors.Is(err, context.DeadlineExceeded) {
					a.message = a.t("Sign-in timed out.", "登录超时。")
					if provider == "antigravity" {
						a.message = a.t("Google sign-in timed out. Check the connection to oauth2.googleapis.com and try again.", "Google 登录超时，请检查到 oauth2.googleapis.com 的网络连接后重试。")
					}
				} else {
					a.message = err.Error()
				}
				return
			}
			a.accountUI.add = ""
			a.profiles = a.accountStore.List()
			a.message = a.t("Account added. It is ready to switch.", "账号已添加，可以切换了。")
			a.accountChanged()
		})
	}()
}

func (a *App) captureCurrentAccount(provider string) {
	a.work("capture/"+provider, func() func() {
		var err error
		found := false
		for _, live := range quota.Discover(quota.KeychainAllowed()) {
			if live.Provider != provider {
				continue
			}
			found = true
			doc, readErr := live.CredentialDoc()
			err = readErr
			if err == nil {
				_, err = a.accountStore.Add(provider, doc, live.CredentialKind())
			}
			break
		}
		return func() {
			if !found {
				a.message = a.t("No current sign-in found. Sign in to this agent first.", "未找到当前登录，请先在这个 Agent 中登录。")
				return
			}
			if err != nil {
				a.message = a.accountBackupError(err)
				return
			}
			a.accountUI.add = ""
			a.profiles = a.accountStore.List()
			a.message = a.t("Current account saved.", "已保存当前账号。")
			a.refreshQuota(true)
			a.accountChanged()
		}
	})
}

func (a *App) cancelAccountLogin() {
	if a.accountUI.loginCancel != nil {
		a.accountUI.loginCancel()
		a.accountUI.loginCancel = nil
	}
	a.accountUI.loginStatus = a.t("Cancelling…", "正在取消…")
}

func (a *App) accountsDialog(c *ui.Context) {
	if !a.accountUI.manager {
		return
	}
	t := c.Theme()
	ui.DialogBase(c, &a.accountUI.manager, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(560).MaxHeight(620).Padding(16).Gap(10).Radius(8).Background(t.Surface).Border(1, t.Border)
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Accounts", "账号")).FontSize(14).Bold().Grow(1)
			close := settingsButton(c, "", false).Padding(4, 8).Label(a.t("Close", "关闭"))
			close.Children(func() { ui.Icon(c, closeIcon).Size(13, 13) })
			if close.Clicked() {
				a.accountUI.manager = false
			}
		})
		ui.Text(c, a.t("Switches the agent's own sign-in. Running sessions keep their account until restarted.", "切换 Agent 自己的登录。已运行的会话在重启前仍使用原账号。")).FontSize(11.5).TextColor(t.TextMuted)
		a.accountSyncManagerRow(c)
		if a.accountUI.managerMsg != "" {
			color := quotaGreen
			if a.accountUI.managerErr {
				color = quotaRose
			}
			ui.Text(c, a.accountUI.managerMsg).FontSize(11).TextColor(color)
		}
		before := a.accountStore.Enabled()
		a.accountUI.enabled = before
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Account switching", "账号切换")).FontSize(12).Grow(1)
			ui.Switch(c, &a.accountUI.enabled).Label(a.t("Account switching", "账号切换"))
			if a.accountUI.enabled {
				status := a.t("On", "已启用")
				ui.Text(c, status).FontSize(10).TextColor(quotaGreen)
			} else {
				ui.Text(c, a.t("Off", "未启用")).FontSize(10).TextColor(faint(t))
			}
		})
		if a.accountUI.enabled != before {
			if err := a.accountStore.SetEnabled(a.accountUI.enabled); err != nil {
				a.message = err.Error()
			} else if a.accountUI.enabled {
				a.refreshQuota(true)
			} else {
				a.lockAccountSync()
			}
		}
		if !a.accountUI.enabled {
			ui.Text(c, a.t("Off keeps this app read-only: it only queries quotas. Enabling stores account names and copies of the agents' sign-ins on this device.", "关闭时只查询配额，不写任何登录；启用后才在本机保存账号名称和 Agent 登录副本。")).FontSize(10.5).TextColor(faint(t))
		}
		ui.Row(c).Gap(8).Children(func() {
			if settingsButton(c, a.t("Import accounts", "导入账号"), false).Disabled(!a.accountUI.enabled || a.busy["account-backup"]).Clicked() {
				a.openAccountBackup("import")
			}
			if settingsButton(c, a.t("Export accounts", "导出账号"), false).Disabled(len(a.profiles) == 0 || a.busy["account-backup"]).Clicked() {
				a.openAccountBackup("export")
			}
		})
		ui.Scroll(c).MaxHeight(380).Grow(1).Children(func() {
			ui.Column(c).Gap(10).Children(func() {
				hidden := 0
				for _, p := range a.profiles {
					if p.Hidden {
						hidden++
					}
				}
				for _, provider := range accountProviders {
					a.accountSection(c, provider)
				}
				if hidden > 0 {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						label := fmt.Sprintf(a.t("Show hidden legacy copies (%d)", "显示已隐藏的旧副本（%d）"), hidden)
						if a.accountUI.showHidden {
							label = a.t("Hide legacy copies", "隐藏旧副本")
						}
						if settingsButton(c, label, false).Clicked() {
							a.accountUI.showHidden = !a.accountUI.showHidden
						}
					})
				}
				ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(6, 0, 0).Children(func() {
					ui.Text(c, a.accountStorePath()).FontSize(9.5).TextColor(faint(t)).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
					if a.openFolder != nil {
						if settingsButton(c, a.t("Open folder", "打开目录"), false).Clicked() {
							a.openFolder(a.accountStorePath())
						}
					}
					if settingsButton(c, a.t("Remove all saved accounts", "移除全部已保存账号"), false).Clicked() {
						a.accountUI.removeAll = true
					}
				})
			})
		})
	})
	if a.accountUI.removeAll {
		text := a.t("Remove every account name and saved sign-in copy from this device? The agents' current sign-ins stay as they are.", "移除本机保存的全部账号名称和登录副本？Agent 当前的登录保持不变。")
		if syncclient.ReadAccountSyncConfig().Enabled {
			text = a.t("Remove all saved accounts and sync the removals to your other devices? Current agent sign-ins stay unchanged.", "移除全部已保存账号，并同步删除其他设备上的副本？Agent 当前的登录保持不变。")
		}
		if ui.AlertDialog(c, &a.accountUI.removeAll, a.t("Remove saved accounts", "移除已保存账号"), text, a.t("Cancel", "取消"), a.t("Remove", "移除")) == 1 {
			if err := a.accountStore.RemoveAll(); err != nil {
				a.accountUI.managerMsg, a.accountUI.managerErr = err.Error(), true
			} else {
				a.profiles = a.accountStore.List()
				a.accountUI.managerMsg, a.accountUI.managerErr = a.t("Saved accounts removed.", "已移除保存的账号。"), false
				a.accountChanged()
			}
		}
	}
}

func (a *App) accountSection(c *ui.Context, provider string) {
	t := c.Theme()
	profiles := a.profilesFor(provider)
	hidden := 0
	for _, p := range a.profiles {
		if p.Provider == provider && p.Hidden {
			hidden++
		}
	}
	if a.accountUI.showHidden {
		profiles = a.allProfilesFor(provider)
	}
	ui.Column(c).Gap(4).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, providerTitle(provider)).FontSize(12).FontWeight(600)
			ui.Text(c, fmt.Sprintf("%d", len(profiles))).FontSize(10).TextColor(faint(t))
			ui.Spacer(c)
			if a.accountStore.Enabled() {
				if settingsButton(c, "+ "+a.t("Add account", "添加账号"), false).Clicked() {
					a.openAddAccount(provider)
				}
			}
		})
		for _, p := range profiles {
			a.accountManagerRow(c, provider, p)
		}
		if len(profiles) == 0 {
			if live, ok := a.liveAccount(provider); ok {
				reason := live.LoginNote()
				if reason == "" {
					reason = live.ReadOnlyReason()
				}
				if reason == "" {
					reason = a.t("Not captured yet; enable switching and refresh quotas.", "尚未捕获；启用账号切换并刷新配额。")
				}
				if strings.Contains(reason, "API-key mode") {
					reason = a.t("Codex is in API-key mode; there is no account to switch", "当前为 API Key 直连模式，无需切换账号")
				}
				ui.Row(c).Padding(6, 10).Gap(8).Radius(4).Background(ui.Hex("#10131c")).Border(1, t.Border).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(6, 6).Radius(3).Background(faint(t).Alpha(.6))
					ui.Column(c).Grow(1).MinWidth(0).Children(func() {
						ui.Text(c, providerName(provider)).FontSize(12).SingleLine().Ellipsis("…")
						ui.Text(c, reason).FontSize(10.5).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
					})
					tag := ui.Text(c, a.t("No credential", "无需凭证")).FontSize(9.5).TextColor(faint(t)).Padding(1, 5).Radius(3).Border(1, t.Border)
					_ = tag
				})
			}
		}
	})
}

func (a *App) accountManagerRow(c *ui.Context, provider string, p accounts.Profile) {
	t := c.Theme()
	row := ui.Row(c.Key("manager/"+p.ID)).FillWidth().Height(38).Padding(4, 8).Gap(8).Radius(4).AlignItems(ui.Center)
	if row.Hovered() {
		row.Background(t.SurfaceHover)
	}
	if a.accountUI.renameID == p.ID {
		row.Children(func() {
			ui.Text(c, a.profileName(provider, p)).FontSize(11).TextColor(faint(t))
			input := ui.TextInputBase(c, &a.accountUI.renameText).Label(a.t("Account name", "账号名称")).FontSize(11).FixedLineHeight(20).Padding(3, 8).Radius(4).Grow(1).Background(ui.Hex("#101014")).Border(1, t.Border)
			if input.Focused() {
				input.Border(1, t.Accent)
			}
			if settingsButton(c, a.t("Save", "保存"), true).Clicked() {
				if err := a.accountStore.Rename(provider, p.ID, a.accountUI.renameText); err != nil {
					a.message = err.Error()
				} else {
					a.profiles = a.accountStore.List()
					a.accountUI.renameID = ""
					a.accountChanged()
				}
			}
			if settingsButton(c, a.t("Cancel", "取消"), false).Clicked() {
				a.accountUI.renameID = ""
			}
		})
		return
	}
	row.Children(func() {
		if p.Active {
			ui.Icon(c, checkIcon).Size(11, 11).Shrink(0).TextColor(t.Accent)
		} else {
			ui.Box(c).Size(7, 7).Shrink(0).Radius(3.5).Border(1, faint(t).Alpha(.6))
		}
		name := a.profileName(provider, p)
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Text(c, name).FontSize(12).TextColor(t.Text).SingleLine().Ellipsis("…")
			var subParts []string
			if p.Plan != "" {
				subParts = append(subParts, strings.ToUpper(p.Plan))
			}
			if p.Email != "" && p.Email != name {
				subParts = append(subParts, p.Email)
			}
			if p.Active {
				subParts = append(subParts, a.t("Active now", "当前使用"))
			} else {
				subParts = append(subParts, a.t("Backup copy", "备用副本"))
			}
			if p.Hidden {
				subParts = append(subParts, a.t("Hidden legacy copy", "已隐藏的旧副本"))
			}
			if p.Reason != "" {
				subParts = append(subParts, p.Reason)
			}
			if len(subParts) > 0 {
				ui.Text(c, strings.Join(subParts, " · ")).FontSize(10).TextColor(faint(t)).SingleLine().Ellipsis("…")
			}
		})
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			if !p.Active {
				switching := a.accountUI.switching[provider]
				swBtn := ui.ButtonBase(c.Key("sw-row/"+p.ID)).Label(a.t("Switch to this account", "切为此账号")).Padding(2, 8).Radius(4).Background(t.Accent.Alpha(.15)).Border(1, t.Accent.Alpha(.35)).Disabled(!p.Writable || switching)
				if swBtn.Hovered() {
					swBtn.Background(t.Accent.Alpha(.25))
				}
				swBtn.Children(func() {
					if switching {
						ui.Spinner(c)
						ui.Text(c, a.t("Switching…", "正在切换…")).FontSize(10).TextColor(t.TextMuted).NoWrap()
					} else {
						ui.Text(c, a.t("Switch", "切换")).FontSize(10.5).FontWeight(600).TextColor(t.Accent).NoWrap()
					}
				})
				if swBtn.Clicked() {
					a.switchAccount(provider, p.ID)
				}
			}
			if settingsButton(c, a.t("Rename", "重命名"), false).Clicked() {
				a.accountUI.renameID, a.accountUI.renameText = p.ID, p.DisplayName()
			}
			if !p.Active {
				if settingsButton(c, a.t("Remove", "移除"), false).Clicked() {
					if live, ok := a.liveAccount(provider); ok && p.Email != "" && strings.EqualFold(live.Label, p.Email) {
						a.accountUI.managerMsg = a.t("This is the live account. Switch to another account before removing it.", "这是当前正在使用的账号。请先切换到其他账号再移除。")
						a.accountUI.managerErr = true
					} else if err := a.accountStore.Remove(provider, p.ID); err != nil {
						a.accountUI.managerMsg, a.accountUI.managerErr = err.Error(), true
					} else {
						a.profiles = a.accountStore.List()
						a.accountUI.managerMsg, a.accountUI.managerErr = a.t("Account removed.", "账号已移除。"), false
						a.accountChanged()
					}
				}
			}
		})
	})
}

func (a *App) addAccountDialog(c *ui.Context) {
	if a.accountUI.add == "" {
		return
	}
	provider := a.accountUI.add
	t := c.Theme()
	faintText := faint(t)
	isLoggingIn := a.accountUI.loginProvider == provider
	canDirectLogin := provider == "claude" || provider == "codex" || provider == "antigravity" || provider == "grok"

	open := true
	ui.DialogBase(c.Key("dialog/add/"+provider), &open, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .6))
		panel.Width(440).Padding(18, 20).Gap(14).Radius(8).Background(t.Surface).Border(1, strongBorder).Shadow(0, 8, 24, 0, ui.Hex("#000000").Alpha(.5))

		// 1. Header
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Size(24, 24).Shrink(0).Radius(5).Background(ui.Hex("#171b24")).Border(1, ui.Hex("#232938")).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				if logo := providerLogos[providerName(provider)]; logo != nil {
					ui.Image(c, logo).Size(14, 14)
				} else {
					ui.Text(c, strings.ToUpper(providerName(provider)[:1])).FontSize(11).Bold().TextColor(providerColor(provider))
				}
			})
			ui.Column(c).Gap(1).Grow(1).Children(func() {
				title := fmt.Sprintf(a.t("Add %s backup account", "添加 %s 备用号"), providerTitle(provider))
				ui.Text(c, title).FontSize(13).FontWeight(600).NoWrap()
				ui.Text(c, a.t("Stored in an isolated copy; does not affect active sessions", "在独立副本中存储，不影响当前会话凭证")).FontSize(10.5).TextColor(faintText)
			})
			closeBtn := settingsButton(c, "", false).Padding(3, 6).Label(a.t("Close", "关闭"))
			closeBtn.Children(func() { ui.Icon(c, closeIcon).Size(12, 12) })
			if closeBtn.Clicked() {
				if isLoggingIn {
					a.cancelAccountLogin()
				}
				a.accountUI.add = ""
			}
		})

		// 2. Explanatory notice banner
		ui.Column(c).Padding(10, 12).Radius(5).Background(ui.Hex("#10131c")).Border(1, t.Border).Children(func() {
			var text string
			switch provider {
			case "antigravity":
				text = a.t(
					"Opens Google sign-in in your system browser. Google refresh tokens don't rotate, so the saved copy stays valid. You can switch to this account anytime.",
					"将在系统默认浏览器中打开 Google 账户授权页。授权完成后副本将自动保存在本机，可在卡片顶部随时切换预览或激活。")
			case "claude", "codex", "grok":
				text = a.t(
					"Signs in through the agent's own browser flow, in a separate config folder. Your current sign-in stays as it is.",
					"通过 Agent 自己的浏览器登录流程，在独立配置目录中登录；当前登录保持不变。")
			default:
				text = a.t(
					"This agent does not support direct browser login here. Please log in via terminal first, then capture the login.",
					fmt.Sprintf("%s 暂不支持在此直接发起浏览器登录。请先在终端中完成登录，然后点击下方按钮捕获凭证作为备用副本。", providerTitle(provider)))
			}
			ui.Text(c, text).FontSize(11).FixedLineHeight(16).TextColor(t.TextMuted)
		})

		// 3. Primary action card
		ui.Column(c).Padding(12, 14).Radius(6).Background(ui.Hex("#111520")).Border(1, t.Accent.Alpha(.25)).AlignItems(ui.Center).Gap(8).Children(func() {
			if isLoggingIn {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(6, 0).Children(func() {
					ui.Spinner(c)
					ui.Text(c, a.accountUI.loginStatus).FontSize(12).TextColor(t.Accent).FontWeight(500)
				})
				ui.Row(c).Gap(8).Children(func() {
					if a.accountUI.loginURL != "" {
						reopen := settingsButton(c, a.t("Open sign-in page again", "重新打开登录页"), false)
						if reopen.Clicked() {
							if a.openURL != nil {
								a.openURL(a.accountUI.loginURL)
							} else {
								c.WriteClipboard(a.accountUI.loginURL)
							}
						}
					}
					cancel := settingsButton(c, a.t("Cancel", "取消"), false)
					if cancel.Clicked() {
						a.cancelAccountLogin()
					}
				})
			} else if canDirectLogin {
				label := a.t("Sign in with ", "登录 ") + providerTitle(provider)
				if provider == "antigravity" {
					label = a.t("Sign in with Google", "使用 Google 登录")
				}
				btn := ui.ButtonBase(c.Key("btn/start-login/" + provider)).FillWidth().Height(34).Radius(5).Background(t.Accent).TextColor(t.AccentText).Label(label)
				if btn.Hovered() {
					btn.Background(t.Accent.Alpha(.85))
				}
				btn.Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
						ui.Text(c, label).FontSize(12.5).FontWeight(600).TextColor(t.AccentText)
					})
				})
				if btn.Clicked() {
					a.startAccountLogin(provider)
				}
				if provider == "antigravity" {
					ui.Text(c, a.t("Google refresh token does not rotate and remains valid long term.", "Google Refresh Token 不会轮换，授权后可长期有效查询")).FontSize(10).TextColor(faintText)
				}
			} else {
				// Agents like Devin without direct browser OAuth: capture is the primary action!
				btn := ui.ButtonBase(c.Key("btn/primary-capture/" + provider)).FillWidth().Height(34).Radius(5).Background(t.Accent).TextColor(t.AccentText).Label(a.t("Capture current login", "捕获本机当前登录凭证"))
				if btn.Hovered() {
					btn.Background(t.Accent.Alpha(.85))
				}
				btn.Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
						ui.Text(c, a.t("Capture current login", "捕获本机当前登录凭证")).FontSize(12.5).FontWeight(600).TextColor(t.AccentText)
					})
				})
				if btn.Clicked() {
					a.captureCurrentAccount(provider)
				}
				ui.Text(c, a.t("Detects active credentials on this device and saves a switchable copy.", "自动读取本机该 Agent 的当前活跃凭证并另存为可切换的独立副本")).FontSize(10).TextColor(faintText)
			}
		})

		// 4. Secondary capture entry (only when direct browser login is available)
		if canDirectLogin {
			ui.Row(c).Padding(6, 2, 0, 2).BorderWidth(1, 0, 0, 0).BorderColor(t.Border.Alpha(.7)).AlignItems(ui.Center).Children(func() {
				ui.Text(c, a.t("Already logged in via terminal?", "已经在其他终端中完成过登录？")).FontSize(11).TextColor(faintText)
				ui.Spacer(c)
				capBtn := settingsButton(c, a.t("Capture current login", "捕获当前登录"), false)
				if capBtn.Clicked() {
					a.captureCurrentAccount(provider)
				}
			})
		}
	})
	if !open {
		if isLoggingIn {
			a.cancelAccountLogin()
		}
		a.accountUI.add = ""
	}
}

func (a *App) accountStorePath() string {
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "devin-usage-metrics", "accounts")
}

func providerTitle(provider string) string {
	switch provider {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	case "grok":
		return "Grok"
	case "devin":
		return "Devin"
	}
	return provider
}
