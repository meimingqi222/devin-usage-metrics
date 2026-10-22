package desktop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

// Settings has two tabs in the Rust UI; About is a section of General.
// Drafts belong to an individual opening, not to the running sync client.
type settingsUIState struct {
	open          bool
	tab           string
	apiURL        string
	updateOpen    bool
	updateMessage string
	updateFailed  bool
	// downloadPercent is -1 until the download reports a total size.
	downloadPercent int
}

func (a *App) cancelSettings() {
	if a.loginCancel != nil {
		a.loginCancel()
	}
	a.settings = false
	a.settingsUI.open = false
	a.settingsUI.apiURL = syncclient.ReadConfig().APIURL
}

func (a *App) saveSettings() bool {
	if a.busy["login"] {
		return false
	}
	cfg := syncclient.ReadConfig()
	url := strings.TrimRight(strings.TrimSpace(a.settingsUI.apiURL), "/")
	changed := cfg.APIURL != url
	if changed {
		cfg.Enabled = false
	}
	cfg.APIURL = url
	// Disable the old endpoint before invalidating its credentials. Never leave
	// sync enabled after an endpoint change, including on a storage error.
	if err := syncclient.SaveConfig(cfg); err != nil {
		a.message = err.Error()
		return false
	}
	if changed {
		if err := syncclient.Logout(); err != nil {
			a.message = err.Error()
			return false
		}
		a.remote.Sessions = nil
		a.remote.Turns = nil
	}
	a.apiURL = url
	a.settings = false
	a.settingsUI.open = false
	if cfg.Enabled {
		a.sync()
	}
	return true
}

// Rust settings controls are flat divs; do not inherit raised toolkit buttons.
func settingsButton(c *ui.Context, label string, accent bool) ui.Element {
	b := ui.Button(c, label).FontSize(12).FixedLineHeight(20).Padding(4, 12).Radius(4).
		Border(0, ui.Color{}).Shadow(0, 0, 0, 0, ui.Color{}).
		Background(ui.Color{}).TextColor(c.Theme().TextMuted)
	if b.Hovered() {
		b.Background(c.Theme().SurfaceHover).TextColor(c.Theme().Text)
	}
	if accent {
		b.Background(c.Theme().Accent).TextColor(ui.Hex("#0a0a0c"))
		if b.Hovered() {
			b.Background(ui.Hex("#6ad4ff"))
		}
	}
	return b
}

func (a *App) settingsView(c *ui.Context) {
	if a.settings && !a.settingsUI.open {
		a.settingsUI.open = true
		a.settingsUI.tab = "sync"
		a.settingsUI.apiURL = syncclient.ReadConfig().APIURL
	}
	if !a.settings && a.settingsUI.open {
		a.cancelSettings()
	}
	ui.DialogBase(c, &a.settings, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(380).Padding(16).Gap(12).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border).FontSize(12).FixedLineHeight(20)
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Grow(1).Children(func() {
				ui.Text(c, "⚙").FontSize(14).FixedLineHeight(24).Bold()
				ui.Text(c, a.t("Settings", "设置")).FontSize(14).FixedLineHeight(24).Bold()
			})
			if settingsButton(c, "✕", false).Padding(4, 8).Label(a.t("Close settings", "关闭设置")).Clicked() {
				a.cancelSettings()
			}
		})
		ui.Row(c).Gap(4).Padding(0, 0, 8, 0).BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border).Children(func() {
			for _, tab := range []struct{ key, en, zh string }{{"sync", "Sync & Devices", "多设备同步"}, {"general", "General", "常规偏好"}} {
				b := settingsButton(c.Key(tab.key), a.t(tab.en, tab.zh), false)
				if a.settingsUI.tab == tab.key {
					b.Background(c.Theme().Accent.Alpha(.2)).TextColor(c.Theme().Accent).Bold()
				}
				if b.Clicked() {
					a.settingsUI.tab = tab.key
				}
			}
		})
		if a.settingsUI.tab == "general" {
			a.settingsGeneral(c)
		} else {
			a.settingsSync(c)
		}
		ui.Row(c).Justify(ui.End).Gap(8).Padding(8, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(c.Theme().Border).Children(func() {
			if settingsButton(c, a.t("Cancel", "取消"), false).Padding(4, 16).Clicked() {
				a.cancelSettings()
			}
			if settingsButton(c, a.t("Save", "保存"), true).Padding(4, 16).Disabled(a.busy["login"]).Clicked() {
				a.saveSettings()
			}
		})
	})
	if !a.settings && a.settingsUI.open {
		a.cancelSettings()
	}
	a.updateDialog(c)
}

func (a *App) settingsSync(c *ui.Context) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Text(c, a.t("Self-hosted sync API", "同步 API 地址")).FontSize(12).TextColor(c.Theme().TextMuted)
		input := ui.TextInputBase(c, &a.settingsUI.apiURL).Label(a.t("Sync API URL", "同步 API 地址")).Placeholder("https://sync.example.com").FontSize(12).FixedLineHeight(20).Padding(8, 12).Radius(6).FillWidth().Background(ui.Hex("#101014")).Border(0, ui.Color{}).Shadow(0, 0, 0, 0, ui.Color{}).Disabled(a.busy["login"])
		if input.Focused() {
			input.Background(c.Theme().SurfaceHover).Border(1, c.Theme().Accent)
		} else if input.Hovered() {
			input.Background(c.Theme().SurfaceHover)
		}
		status := a.t("Not signed in; devices using the same GitHub account sync automatically", "未登录；同一 GitHub 账号的设备会自动同步")
		label := a.t("Sign in with GitHub", "登录 GitHub")
		if session := syncclient.SessionLabel(); session != "" {
			status = a.t("Signed in as ", "已登录 GitHub：") + session
			label = a.t("Sign in again with GitHub", "重新登录 GitHub")
		}
		if a.busy["login"] {
			status = a.t("Preparing browser login…", "正在准备浏览器登录…")
			label = a.t("Waiting for GitHub authorization…", "等待 GitHub 授权…")
		}
		if a.auth.UserCode != "" {
			status = a.auth.VerificationURI + "\n" + a.auth.UserCode
		}
		ui.Text(c, status).FontSize(12).TextColor(c.Theme().TextMuted).Selectable()
		if settingsButton(c, label, true).FillWidth().Padding(8, 12).Disabled(a.busy["login"] || strings.TrimSpace(a.settingsUI.apiURL) == "").Clicked() {
			a.apiURL = strings.TrimRight(strings.TrimSpace(a.settingsUI.apiURL), "/")
			a.beginLogin()
		}
		if a.busy["login"] && a.auth.UserCode != "" {
			if settingsButton(c, a.t("Copy code", "复制验证码"), false).FillWidth().Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).TextColor(c.Theme().Text).Clicked() {
				c.WriteClipboard(a.auth.UserCode)
			}
			if settingsButton(c, a.t("Cancel login", "取消登录"), false).Clicked() && a.loginCancel != nil {
				a.loginCancel()
			}
		}
		if syncclient.SessionLabel() != "" && settingsButton(c, a.t("Sign out", "退出 GitHub"), false).FillWidth().Justify(ui.Start).TextAlign(ui.Start).Disabled(a.busy["login"]).Clicked() {
			cfg := syncclient.ReadConfig()
			cfg.Enabled = false
			if err := syncclient.SaveConfig(cfg); err != nil {
				a.message = err.Error()
			} else if err := syncclient.Logout(); err != nil {
				a.message = err.Error()
			} else {
				a.remote.Sessions = nil
				a.remote.Turns = nil
			}
		}
		ui.Text(c, a.t("Devices using the same GitHub account merge into the same sync data", "同一 GitHub 账号的设备会合并到同一份同步数据")).FontSize(12).TextColor(c.Theme().TextMuted)
	})
}

func (a *App) settingsGeneral(c *ui.Context) {
	ui.Column(c).Gap(16).Padding(4, 0).Children(func() {
		ui.Column(c).Gap(8).Children(func() {
			ui.Text(c, a.t("Language", "语言")).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Row(c).Gap(8).Children(func() {
				for _, lang := range []struct{ key, label string }{{"zh", "简体中文"}, {"en", "English"}} {
					b := settingsButton(c, lang.label, false)
					if a.lang == lang.key {
						b.Background(c.Theme().Accent.Alpha(.2)).TextColor(c.Theme().Accent).Bold()
					}
					if b.Clicked() {
						if err := saveLanguage(lang.key); err != nil {
							a.message = err.Error()
						} else {
							a.lang = lang.key
							if a.languageChanged != nil {
								a.languageChanged()
							}
						}
					}
				}
			})
		})
		ui.Column(c).Gap(4).Padding(8, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(c.Theme().Border).Children(func() {
			ui.Text(c, a.t("About this app", "关于本应用")).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Text(c, "Agent Usage Metrics v"+updater.CurrentVersion).FontSize(12)
			ui.Text(c, a.t("Local Coding Agent usage and quota monitor", "本地 Coding Agent 用量与配额监控工具")).FontSize(12).TextColor(c.Theme().TextMuted)
		})
	})
}

// updateVersionRow is intended for the sidebar, matching Rust's separate
// compact version action and 420px update dialog rather than a settings scroll.
func (a *App) updateVersionRow(c *ui.Context) {
	label := "v" + updater.CurrentVersion
	if a.busy["update"] {
		label = a.t("Checking for updates…", "正在检查更新…")
	} else if a.busy["download"] {
		label = a.t("Downloading…", "正在下载…")
		if p := a.settingsUI.downloadPercent; p >= 0 {
			label = fmt.Sprintf(a.t("Downloading… %d%%", "正在下载… %d%%"), p)
		}
	} else if a.settingsUI.updateFailed {
		label = a.t("Update failed", "更新失败")
	} else if a.payload != "" {
		label = a.t("Restart to install", "重启并安装")
	} else if a.release.Version != "" {
		label = a.t("Update available ", "发现更新 ") + a.release.Version
	}
	if textButton(c, label, false).Height(24).Disabled(a.busy["update"]).Clicked() {
		a.settingsUI.updateOpen = true
		if a.release.Version == "" {
			a.checkForUpdates()
		}
	}
}

func (a *App) checkForUpdates() { a.checkUpdates(true) }

// checkUpdates follows Rust: automatic checks run only for packaged installs
// with auto_check_updates on, skipped versions count as up to date, and an
// available update starts downloading at once.
func (a *App) checkUpdates(manual bool) {
	if !manual && (!a.updatePrefs.AutoCheck || !updater.Packaged()) {
		return
	}
	if a.busy["download"] || a.payload != "" {
		return
	}
	if manual {
		a.settingsUI.updateMessage, a.settingsUI.updateFailed = "", false
	}
	skipped := a.updatePrefs.Skipped
	a.work("update", func() func() {
		release, err := updater.Check(a.ctx, updater.CurrentVersion)
		return func() {
			a.updatePrefs.LastCheckAt = time.Now().Unix()
			if err := saveUpdatePrefs(a.updatePrefs); err != nil {
				a.message = err.Error()
			}
			if err != nil {
				a.settingsUI.updateMessage, a.settingsUI.updateFailed = err.Error(), true
				return
			}
			a.settingsUI.updateFailed = false
			if updater.IsSkipped(release.Version, skipped) {
				release = updater.Release{}
			}
			a.release = release
			if release.Version == "" {
				if manual {
					a.settingsUI.updateMessage = a.t("Already up to date", "已是最新版本")
				}
				return
			}
			a.downloadUpdate()
		}
	})
}

func (a *App) downloadUpdate() {
	release := a.release
	a.settingsUI.updateMessage, a.settingsUI.updateFailed, a.settingsUI.downloadPercent = "", false, -1
	a.work("download", func() func() {
		last := -1
		payload, err := updater.Download(a.ctx, release, func(done, total int64) {
			if total <= 0 {
				return
			}
			if p := int(done * 100 / total); p != last {
				last = p
				a.post(func() { a.settingsUI.downloadPercent = p })
			}
		})
		return func() {
			// The user may have skipped this version while it downloaded.
			if a.release.Version != release.Version {
				return
			}
			if err != nil {
				a.settingsUI.updateMessage, a.settingsUI.updateFailed = err.Error(), true
			} else {
				a.payload = payload
			}
		}
	})
}

func (a *App) skipUpdate() {
	a.updatePrefs.Skipped = a.release.Version
	if err := saveUpdatePrefs(a.updatePrefs); err != nil {
		a.message = err.Error()
	}
	a.release, a.payload = updater.Release{}, ""
	a.settingsUI.updateOpen, a.settingsUI.updateFailed = false, false
}

func (a *App) updateDialog(c *ui.Context) {
	ui.DialogBase(c, &a.settingsUI.updateOpen, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(420).Padding(16).Gap(16).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border)
		ui.Column(c).Children(func() {
			ui.Text(c, a.t("Application update", "应用更新")).FontSize(14).Bold()
			ui.Text(c, "v"+a.release.Version).FontSize(12).TextColor(c.Theme().TextMuted)
		})
		body := a.release.Notes
		if a.settingsUI.updateMessage != "" {
			body = a.settingsUI.updateMessage
		}
		if a.busy["download"] {
			body = a.t("Downloading and verifying…", "正在下载并校验…")
		}
		if a.payload != "" {
			body = a.t("Update ready. Restart to install.", "更新已就绪，重启以安装。")
		}
		if body != "" {
			ui.Text(c, body).FontSize(12).TextColor(c.Theme().TextMuted).MaxHeight(120)
		}
		if a.release.Version != "" {
			ui.Row(c).Gap(8).Children(func() {
				if textButton(c, a.t("Skip this version", "跳过此版本"), false).Clicked() {
					a.skipUpdate()
				}
				if a.release.URL != "" && a.openURL != nil && textButton(c, a.t("Open release page", "打开发布页面"), false).Clicked() {
					a.openURL(a.release.URL)
				}
			})
		}
		if ui.Button(c, a.t("Later", "稍后")).FontSize(12).Clicked() {
			a.settingsUI.updateOpen = false
		}
		label := a.t("Download verified update", "下载并校验更新")
		if a.payload != "" {
			label = a.t("Install and restart", "安装并重启")
		} else if a.release.Version == "" {
			label = a.t("Check for updates", "检查更新")
		}
		if ui.PrimaryButton(c, label).FontSize(12).FillWidth().Disabled(a.busy["update"] || a.busy["download"]).Clicked() {
			if a.payload != "" {
				if err := updater.Apply(a.payload); err != nil {
					a.settingsUI.updateMessage = err.Error()
				} else if a.quit != nil {
					a.quit()
				}
			} else if a.release.Version == "" {
				a.checkForUpdates()
			} else {
				a.downloadUpdate()
			}
		}
	})
}

func (a *App) beginLogin() {
	apiURL := a.apiURL
	cfg := syncclient.ReadConfig()
	if cfg.APIURL != apiURL {
		cfg.Enabled = false
		cfg.APIURL = apiURL
		if err := syncclient.SaveConfig(cfg); err != nil {
			a.message = err.Error()
			return
		}
		if err := syncclient.Logout(); err != nil {
			a.message = err.Error()
			return
		}
		a.remote.Sessions = nil
		a.remote.Turns = nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 16*time.Minute)
	a.loginCancel = cancel
	a.work("login", func() func() {
		defer cancel()
		auth, err := syncclient.BeginLogin(ctx, apiURL)
		if err == nil {
			a.post(func() {
				if ctx.Err() == nil {
					a.auth = auth
				}
			})
			err = syncclient.CompleteLogin(ctx, apiURL, auth)
		}
		return func() {
			a.auth = syncclient.Authorization{}
			a.loginCancel = nil
			if err != nil {
				a.message = err.Error()
			} else {
				a.message = fmt.Sprintf(a.t("Signed in as %s. Enable sync from the header.", "已登录 %s，请在顶栏开启同步。"), syncclient.SessionLabel())
			}
		}
	})
}
