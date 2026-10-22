package desktop

import (
	"fmt"
	"math"
	"runtime"
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

// quotaColor colours by remaining share, like Rust's pct_color.
func quotaColor(used float64) ui.Color {
	switch remaining := 100 - used; {
	case remaining >= 66:
		return ui.Hex("#34d399")
	case remaining >= 33:
		return ui.Hex("#fbbf24")
	default:
		return ui.Hex("#f87171")
	}
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
	if runtime.GOOS == "darwin" && !quota.KeychainAllowed() && !a.keychainDismissed {
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
	// Cards in a row share its height, as with Rust's default flex stretch.
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Stretch).Children(func() {
		for _, ar := range a.accounts {
			a.quotaCard(c.Key(ar.account.Key), ar)
		}
	})
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
	muted := func(s string) { ui.Text(c, s).FontSize(12).TextColor(t.TextMuted) }
	card := ui.Column(c).Width(320).Shrink(0).Padding(12).Gap(8).Radius(6).Background(t.Surface).Border(1, t.Border)
	card.Label(providerName(ar.account.Provider) + " · " + ar.account.Label).Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(7, 7).Radius(4).Background(providerColor(ar.account.Provider))
			ui.Text(c, providerName(ar.account.Provider)).FontSize(14).Bold().NoWrap()
			ui.Text(c, ar.account.Label).FontSize(12).Grow(1).MinWidth(0).SingleLine().TextColor(t.TextMuted)
			// Saved Devin cookies are removable; the CLI login is not.
			if index, err := strconv.Atoi(strings.TrimPrefix(ar.account.Key, "devin-")); strings.HasPrefix(ar.account.Key, "devin-") && err == nil {
				remove := ui.ButtonBase(c).Padding(0, 4).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(t.TextMuted)
				if remove.Hovered() {
					remove.TextColor(ui.Hex("#f87171"))
				}
				remove.Children(func() { ui.Text(c, a.t("Remove", "移除")).FontSize(12).NoWrap() })
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
			ui.Text(c, fmt.Sprintf(a.t("Query failed: %s", "查询失败：%s"), ar.err)).FontSize(12).TextColor(ui.Hex("#f87171"))
			return
		}
		r := ar.result
		if r.Plan != "" {
			color := t.Accent
			if r.Plan == "API MODE" {
				color = ui.Hex("#34d399")
			}
			ui.Text(c, r.Plan).FontSize(12).Padding(1, 8).Radius(4).AlignSelf(ui.Start).Background(t.SurfaceHover).TextColor(color)
		}
		for _, w := range r.Windows {
			color := quotaColor(w.UsedPercent)
			ui.Column(c).Gap(4).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, a.windowLabel(w.Label)).FontSize(12).Grow(1).MinWidth(0).SingleLine()
					ui.Text(c, fmt.Sprintf("%.0f%%", w.UsedPercent)).FontSize(12).Bold().TextColor(color)
				})
				ui.Box(c).FillWidth().Height(6).Radius(3).Background(t.SurfaceHover).Draw(func(p *ui.Painter, r ui.Rect) {
					r.W *= float32(math.Max(0, math.Min(1, w.UsedPercent/100)))
					p.Fill(r, color, 3)
				})
				if left := w.ResetsAt - time.Now().Unix(); left > 0 {
					muted(fmt.Sprintf(a.t("resets in %s", "%s 后重置"), a.duration(left)))
				}
			})
		}
		if len(r.Windows) == 0 && r.Plan != "API MODE" {
			muted(a.t("No record", "无记录"))
		}
		if r.Extra != "" {
			muted(fmt.Sprintf(a.t("Extra: %s", "附加：%s"), r.Extra))
		}
		if r.Stale != "" {
			age := a.duration(max(r.StaleAge, 60))
			text := fmt.Sprintf(a.t("Cached data (%s ago) - refresh failed: %s", "缓存数据（%s前），本次查询失败：%s"), age, r.Stale)
			if r.RetryIn > 0 {
				text = fmt.Sprintf(a.t("Cached data (%s ago) - rate limited, retry in %s", "缓存数据（%s前），被限流，%s后重试"), age, a.duration(max(r.RetryIn, 60)))
			}
			ui.Text(c, text).FontSize(12).TextColor(ui.Hex("#fbbf24"))
		}
	})
}
