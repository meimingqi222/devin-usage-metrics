package desktop

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func providerName(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "grok":
		return "Grok"
	case "devin":
		return "Devin"
	case "antigravity":
		return "Antigravity"
	}
	return provider
}

// providerColor matches src/main.rs provider_color.
func providerColor(provider string) ui.Color {
	switch providerName(provider) {
	case "Claude":
		return ui.Hex("#d97757")
	case "Codex":
		return ui.Hex("#3ddc97")
	case "Devin":
		return ui.Hex("#4cc2ff")
	case "Antigravity":
		return ui.Hex("#4285f4")
	}
	return ui.Hex("#e6e6ee")
}

// Quota status colors by remaining share, the thresholds of Rust's pct_color.
var (
	quotaGreen = ui.Hex("#10b981")
	quotaAmber = ui.Hex("#f59e0b")
	quotaRose  = ui.Hex("#f43f5e")
)

func quotaColor(used float64) ui.Color {
	switch remaining := 100 - used; {
	case remaining >= 66:
		return quotaGreen
	case remaining >= 33:
		return quotaAmber
	default:
		return quotaRose
	}
}

// quotaAlert reports a window with under a third left.
func quotaAlert(r quota.Result) bool {
	for _, w := range r.Windows {
		if 100-w.UsedPercent < 33 {
			return true
		}
	}
	return false
}

// windowLabel localizes the provider-neutral window names; model-specific
// names (Antigravity) pass through with only their window part translated.
func (a *App) windowLabel(label string) string {
	names := map[string][2]string{
		"5h":            {"5-hour window", "5 小时窗口"},
		"7d":            {"7-day window", "7 天窗口"},
		"7d OAuth apps": {"7-day · OAuth apps", "7 天 · OAuth 应用"},
		"7d Opus":       {"7-day · Opus", "7 天 · Opus"},
		"7d Sonnet":     {"7-day · Sonnet", "7 天 · Sonnet"},
		"Daily":         {"Daily quota", "日配额"},
		"Weekly":        {"Weekly quota", "周配额"},
		"Monthly":       {"Monthly quota", "月配额"},
	}
	if n, ok := names[label]; ok {
		return a.t(n[0], n[1])
	}
	if model, window, ok := strings.Cut(label, " – "); ok {
		if n, ok := names[window]; ok {
			return model + " · " + a.t(n[0], n[1])
		}
	}
	return label
}

// duration matches Rust's fmt_duration: the two largest units.
func (a *App) duration(secs int64) string {
	days, hours, minutes := secs/86400, secs%86400/3600, secs%3600/60
	switch {
	case days > 0:
		return fmt.Sprintf(a.t("%dd %dh", "%d天%d小时"), days, hours)
	case hours > 0:
		return fmt.Sprintf(a.t("%dh %dm", "%d小时%d分"), hours, minutes)
	default:
		return fmt.Sprintf(a.t("%dm", "%d分钟"), minutes)
	}
}

func (a *App) quotaView(c *ui.Context) {
	t := c.Theme()
	// Like Rust, offer Keychain access only where a Keychain login may exist.
	if a.keychainLikely && !quota.KeychainAllowed() && !a.keychainDismissed {
		a.keychainBanner(c)
	}
	if a.busy["quota"] && len(a.accounts) == 0 {
		ui.Row(c).Padding(16).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Spinner(c)
			ui.Text(c, a.t("Querying subscription quotas", "正在查询订阅配额")).FontSize(14).TextColor(t.TextMuted)
		})
		return
	}
	if len(a.accounts) == 0 {
		ui.Text(c, a.t("No logged-in accounts detected. Log in via the agent CLI first.", "未检测到已登录账号。请先在对应 Agent 的 CLI 中登录。")).FontSize(14).TextColor(t.TextMuted).Padding(16)
		return
	}
	alerts := 0
	for _, ar := range a.accounts {
		if ar.err == nil && quotaAlert(ar.result) {
			alerts++
		}
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(7, 7).Radius(4).Background(quotaGreen)
			ui.Text(c, a.t("Live subscription quotas", "实时订阅配额")).FontSize(15).Bold()
		})
		summary := fmt.Sprintf(a.t("%d accounts", "%d 个已授权账号"), len(a.accounts))
		if alerts > 0 {
			summary += fmt.Sprintf(a.t(" · %d high-usage alerts", " · %d 个高用量告警"), alerts)
		}
		ui.Text(c, summary).FontSize(11).Padding(4, 10).Radius(4).Background(ui.Hex("#161a23")).Border(1, t.Border).TextColor(t.TextMuted).NoWrap()
		ui.Spacer(c)
		requery := ui.ButtonBase(c).Label(a.t("Query again", "重新查询")).FontSize(11).Padding(3, 8).Radius(4).Background(ui.Hex("#141720")).Border(1, t.Border).TextColor(t.TextMuted).Disabled(a.busy["quota"])
		if requery.Hovered() {
			requery.TextColor(t.Text)
		}
		requery.Children(func() {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, requeryIcon).Size(11, 11)
				ui.Text(c, a.t("Query again", "重新查询")).NoWrap()
			})
		})
		if requery.Clicked() {
			a.refreshQuota(true)
		}
		if a.quotaAt != "" {
			ui.Text(c, a.quotaAt).FontSize(11).FontFeatures("tnum").TextColor(faint(t))
		}
	})
	// A uniform two-column grid: cards in a row share its height.
	ui.Grid(c).Columns(2).Gap(12).Children(func() {
		for _, ar := range a.accounts {
			a.quotaCard(c.Key(ar.account.Key), ar)
		}
	})
	a.claudeResetDialog(c)
}

func (a *App) keychainBanner(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Padding(12).Gap(8).Radius(6).Background(t.Surface).Border(1, t.Border).Children(func() {
		ui.Text(c, a.t(
			"Claude Code / Antigravity logins may be stored in the macOS Keychain. Reading them is required to query subscription quotas. Credentials stay on this machine. The system may show an authorization prompt on first access.",
			"检测到本机可能有 Claude Code / Antigravity 登录信息存在「钥匙串」中。读取它们才能查询订阅配额；凭证仅在本机使用，不会上传。首次读取时系统可能会弹出授权框。")).FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(8).Children(func() {
			allow := ui.ButtonBase(c).Height(24).Padding(2, 12).Radius(6).Border(0, ui.Color{}).Background(t.Accent).TextColor(t.AccentText)
			allow.Children(func() { ui.Text(c, a.t("Allow Keychain access", "允许读取钥匙串")).FontSize(12).Bold().NoWrap() })
			if allow.Clicked() {
				if err := quota.SetKeychainAllowed(true); err != nil {
					a.message = err.Error()
				} else {
					a.refreshQuota(false)
				}
			}
			if textButton(c, a.t("Not now", "暂不"), false).Height(24).Clicked() {
				a.keychainDismissed = true
			}
		})
	})
}

func (a *App) quotaCard(c *ui.Context, ar accountResult) {
	t := c.Theme()
	faintText := faint(t)
	r := ar.result
	card := ui.Column(c).MinWidth(0).Padding(14, 16).Gap(12).Radius(6).Background(t.Surface).Border(1, t.Border).Shadow(0, 1, 3, 0, ui.Hex("#000000").Alpha(.2))
	switch {
	case ar.err == nil && quotaAlert(r):
		card.Border(1, quotaRose.Alpha(.4))
	case card.Hovered():
		card.Border(1, strongBorder)
	}
	brand := providerColor(ar.account.Provider)
	card.Label(providerName(ar.account.Provider) + " · " + ar.account.Label).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Size(22, 22).Shrink(0).Radius(5).Background(ui.Hex("#171b24")).Border(1, ui.Hex("#232938")).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				if logo := providerLogos[providerName(ar.account.Provider)]; logo != nil {
					ui.Image(c, logo).Size(14, 14)
				} else {
					ui.Text(c, strings.ToUpper(providerName(ar.account.Provider)[:1])).FontSize(10).FixedLineHeight(12).Bold().TextColor(brand)
				}
			})
			ui.Text(c, providerName(ar.account.Provider)).FontSize(13).FontWeight(600).NoWrap()
			ui.Text(c, ar.account.Label).FontSize(11).Grow(1).MinWidth(0).SingleLine().TextColor(faintText)
			if r.Plan != "" && ar.err == nil {
				tag := ui.Text(c, strings.ToUpper(r.Plan)).FontSize(10).FontWeight(600).Padding(1, 6).Radius(3).Background(controlColor).Border(1, ui.Hex("#242c3d")).TextColor(faintText).NoWrap()
				if r.Plan != "API MODE" {
					tag.Background(t.Accent.Alpha(.08)).Border(1, t.Accent.Alpha(.25)).TextColor(t.Accent)
				}
			}
			// Saved Devin cookies are removable; the CLI login is not.
			if index, err := strconv.Atoi(strings.TrimPrefix(ar.account.Key, "devin-")); strings.HasPrefix(ar.account.Key, "devin-") && err == nil {
				remove := ui.ButtonBase(c).Padding(0, 4).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(faintText)
				if remove.Hovered() {
					remove.TextColor(quotaRose)
				}
				remove.Children(func() { ui.Text(c, a.t("Remove", "移除")).FontSize(11).NoWrap() })
				if remove.Clicked() {
					if err := quota.RemoveDevinAccount(index); err != nil {
						a.message = err.Error()
					} else {
						a.refreshQuota(true)
					}
				}
			}
		})
		if ar.err != nil {
			ui.Text(c, fmt.Sprintf(a.t("Query failed: %s", "查询失败：%s"), ar.err)).FontSize(11.5).TextColor(quotaRose)
			return
		}
		ui.Column(c).Gap(8).Children(func() {
			for _, w := range r.Windows {
				a.quotaWindow(c, w)
			}
			switch {
			case len(r.Windows) == 0 && r.Plan == "API MODE":
				ui.Row(c).Gap(6).Padding(4, 0).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, checkIcon).Size(12, 12).TextColor(quotaGreen)
					ui.Text(c, a.t("Pay-as-you-go API · no window limits", "按量直连模式 · 无窗口速率限制")).FontSize(11.5).TextColor(t.TextMuted)
				})
			case len(r.Windows) == 0:
				ui.Text(c, a.t("No record", "无记录")).FontSize(11.5).TextColor(faintText)
			}
		})
		if r.Extra == "" && r.Stale == "" && r.Reset == nil {
			return
		}
		ui.Spacer(c)
		ui.Column(c).Gap(4).Padding(8, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(ui.Hex("#1a1e2a")).Children(func() {
			if r.Reset != nil {
				a.claudeResetBar(c, ar.account, *r.Reset)
			}
			if r.Extra != "" {
				ui.Text(c, fmt.Sprintf(a.t("Extra: %s", "附加：%s"), r.Extra)).FontSize(11).MinWidth(0).TextColor(faintText)
			}
			if r.Stale != "" {
				age := a.duration(max(r.StaleAge, 60))
				text := fmt.Sprintf(a.t("Cached data (%s ago) - refresh failed: %s", "缓存数据（%s前），本次查询失败：%s"), age, r.Stale)
				if r.RetryIn > 0 {
					text = fmt.Sprintf(a.t("Cached data (%s ago) - rate limited, retry in %s", "缓存数据（%s前），被限流，%s后重试"), age, a.duration(max(r.RetryIn, 60)))
				}
				ui.Text(c, text).FontSize(11).MinWidth(0).TextColor(quotaAmber)
			}
		})
	})
}

// quotaWindow is one window: its name, the share used and the time to
// reset on one line, over a thin rail.
func (a *App) quotaWindow(c *ui.Context, w quota.Window) {
	t := c.Theme()
	color := quotaColor(w.UsedPercent)
	pct := color
	if w.UsedPercent <= 0 {
		pct = faint(t)
	}
	ui.Column(c).Gap(3).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.windowLabel(w.Label)).FontSize(11.5).FontWeight(500).TextColor(t.Text.Alpha(.85)).Grow(1).MinWidth(0).SingleLine()
			ui.Text(c, fmt.Sprintf("%.0f%%", w.UsedPercent)).FontSize(11.5).FontWeight(600).FontFeatures("tnum").TextColor(pct)
			if left := w.ResetsAt - time.Now().Unix(); left > 0 {
				ui.Text(c, fmt.Sprintf(a.t("resets in %s", "%s 后重置"), a.duration(left))).FontSize(10.5).FontFeatures("tnum").TextColor(faint(t)).NoWrap()
			}
		})
		ui.Box(c).FillWidth().Height(3).Radius(1.5).Background(ui.Hex("#1c212c")).Draw(func(p *ui.Painter, r ui.Rect) {
			r.W *= float32(math.Max(0, math.Min(1, w.UsedPercent/100)))
			p.Fill(r, color, 1.5)
		})
	})
}

// resetWindows names the windows a Claude reset clears.
func (a *App) resetWindows(clears []string) string {
	var names []string
	for _, w := range clears {
		names = append(names, map[string]string{"five_hour": "5h", "seven_day": a.t("7d", "7 天"), "seven_day_overage_included": a.t("7d incl. extra", "7 天（含附加）")}[w])
	}
	return strings.Join(names, " / ")
}

func (a *App) resetBlocker(r quota.ClaudeReset) string {
	switch r.Blocker {
	case "not_limited":
		return a.t("Available once a window reaches its limit", "窗口达到上限后才可使用")
	case "cooldown":
		return fmt.Sprintf(a.t("Cooling down, available in %s", "冷却中，%s 后可用"), a.duration(max(r.CooldownUntil-time.Now().Unix(), 60)))
	case "exhausted":
		return a.t("No resets left", "重置次数已用完")
	case "ineligible":
		return a.t("This account is not eligible", "此账号暂不符合条件")
	case "paused":
		return a.t("Resets are paused", "重置已暂停")
	}
	return a.t("Not available right now", "当前不可用")
}

// claudeResetBar shows the account's reset grants and spends one after a
// confirmation; it stays disabled with the reason while none can be spent.
func (a *App) claudeResetBar(c *ui.Context, account quota.Account, r quota.ClaudeReset) {
	t := c.Theme()
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		// Count and expiry take a line each, so neither is cut at any
		// width; the hour decides whether a grant can still be used.
		ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, fmt.Sprintf(a.t("Reset grants: %d of %d left", "储备权益：剩余 %d / %d 次"), r.Left, r.Total)).FontSize(11).TextColor(t.TextMuted)
			if r.EndsAt > 0 {
				expiry := fmt.Sprintf(a.t("Expires %s", "%s 到期"), time.Unix(r.EndsAt, 0).Format("01-02 15:04"))
				if left := r.EndsAt - time.Now().Unix(); left > 0 && left < 7*86400 {
					expiry += fmt.Sprintf(a.t(" (in %s)", "（剩 %s）"), a.duration(left))
				}
				ui.Text(c, expiry).FontSize(11).FontFeatures("tnum").TextColor(faint(t))
			}
		})
		label := a.t("Reset now", "立即重置")
		busy := a.busy["claude-reset"]
		if busy {
			label = a.t("Resetting…", "正在重置…")
		}
		disabled := busy || r.Blocker != "" || r.GrantID == ""
		b := ui.ButtonBase(c).Label(label).FontSize(11).FontWeight(500).Padding(3, 8).Radius(4).Background(t.Accent.Alpha(.08)).Border(1, t.Accent.Alpha(.25)).TextColor(t.Accent).Disabled(disabled)
		switch {
		case disabled:
			b.Background(controlColor).Border(1, t.Border).TextColor(faint(t))
		case b.Hovered():
			b.Background(t.Accent.Alpha(.18)).Border(1, t.Accent.Alpha(.45))
		}
		b.Children(func() {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, resetIcon).Size(10, 10)
				ui.Text(c, label).NoWrap()
			})
		})
		switch w := a.resetWindows(r.Clears); {
		case r.Blocker != "":
			b.Tooltip(a.resetBlocker(r))
		case w != "":
			b.Tooltip(fmt.Sprintf(a.t("Clears %s", "清空 %s"), w))
		}
		if b.Clicked() && !disabled {
			account := account
			a.resetAccount, a.resetClears, a.resetConfirm = &account, a.resetWindows(r.Clears), true
		}
	})
}

func (a *App) claudeResetDialog(c *ui.Context) {
	if a.resetAccount == nil {
		return
	}
	account := *a.resetAccount
	text := fmt.Sprintf(a.t("Spend one Claude reset grant for %s? This clears its usage window now and cannot be undone.", "为 %s 消耗 1 次 Claude 重置权益？用量窗口会立即清空，且无法撤销。"), account.Label)
	if a.resetClears != "" {
		text = fmt.Sprintf(a.t("Spend one Claude reset grant for %s? This clears %s now and cannot be undone.", "为 %s 消耗 1 次 Claude 重置权益？会立即清空 %s，且无法撤销。"), account.Label, a.resetClears)
	}
	if ui.AlertDialog(c, &a.resetConfirm, a.t("Reset Claude usage", "重置 Claude 用量"), text, a.t("Cancel", "取消"), a.t("Reset", "重置")) == 1 {
		a.resetAccount = nil
		a.claimClaudeReset(account)
	}
	if !a.resetConfirm {
		a.resetAccount = nil
	}
}

func (a *App) claimClaudeReset(account quota.Account) {
	a.work("claude-reset", func() func() {
		code, err := quota.ClaimClaudeReset(a.ctx, account)
		return func() {
			messages := map[string][2]string{
				"reset":        {"Claude usage reset.", "已重置 Claude 用量窗口。"},
				"already_used": {"This reset was already applied.", "这次重置已生效。"},
				"not_limited":  {"Not at a limit yet; no reset was spent.", "尚未达到上限，未消耗次数。"},
				"cooldown":     {"Resets are cooling down; none was spent.", "重置冷却中，未消耗次数。"},
				"ineligible":   {"This account is not eligible; none was spent.", "此账号暂不符合条件，未消耗次数。"},
				"unavailable":  {"Resets are unavailable; none was spent.", "重置暂不可用，未消耗次数。"},
				"rate_limited": {"Rate limited; no reset was spent.", "请求被限流，未消耗次数。"},
				"auth_error":   {"Claude rejected the login; no reset was spent.", "Claude 登录无效，未消耗次数。"},
			}
			switch m, ok := messages[code]; {
			case errors.Is(err, quota.ErrClaudeResetUnknown):
				a.message = a.t("The reset outcome is unknown; quotas were re-read, check them before trying again.", "重置结果未知，已重新查询配额，请确认后再操作。")
			case err != nil:
				a.message = fmt.Sprintf(a.t("Reset failed: %s", "重置失败：%s"), err)
			case ok:
				a.message = a.t(m[0], m[1])
			default:
				a.message = a.resetBlocker(quota.ClaudeReset{Blocker: code})
			}
			a.refreshQuota(true)
		}
	})
}
