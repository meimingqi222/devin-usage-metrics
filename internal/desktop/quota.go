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

func (a *App) quotaView(c *ui.Context) {
	if runtime.GOOS == "darwin" && !quota.KeychainAllowed() {
		ui.Column(c).Padding(12).Gap(8).Background(c.Theme().Surface).Border(1, c.Theme().Border).Radius(6).Children(func() {
			ui.Text(c, a.t("Allow Keychain access to discover signed-in Claude and Codex accounts.", "允许访问钥匙串以发现已登录的 Claude 和 Codex 账号。")).FontSize(12).TextColor(c.Theme().TextMuted)
			if textButton(c, a.t("Allow Keychain access", "允许访问钥匙串"), true).Clicked() {
				if err := quota.SetKeychainAllowed(true); err != nil {
					a.message = err.Error()
				} else {
					a.refreshQuota(false)
				}
			}
		})
	}
	if a.busy["quota"] && len(a.accounts) == 0 {
		ui.Text(c, a.t("Loading quotas…", "正在加载配额…")).FontSize(14).TextColor(c.Theme().TextMuted).Padding(16)
		return
	}
	if len(a.accounts) == 0 {
		ui.Text(c, a.t("No supported accounts found. Sign in with your agent or add a Devin account.", "未发现受支持的账号。请先在 Agent 登录或添加 Devin 账号。")).FontSize(14).TextColor(c.Theme().TextMuted).Padding(16)
		return
	}
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Start).Children(func() {
		for _, ar := range a.accounts {
			ui.Column(c.Key(ar.account.Key)).Width(320).Shrink(0).Padding(12).Gap(8).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					color := trayText
					switch providerName(ar.account.Provider) {
					case "Claude":
						color = ui.Hex("#d97757")
					case "Codex":
						color = ui.Hex("#3ddc97")
					case "Devin":
						color = c.Theme().Accent
					case "Antigravity":
						color = ui.Hex("#4285f4")
					}
					ui.Box(c).Size(7, 7).Radius(4).Background(color)
					ui.Text(c, providerName(ar.account.Provider)).FontSize(14).Bold()
					ui.Text(c, ar.account.Label).FontSize(12).Grow(1).MinWidth(0).NoWrap().Ellipsis("…").TextColor(c.Theme().TextMuted)
					if index, err := strconv.Atoi(strings.TrimPrefix(ar.account.Key, "devin-")); strings.HasPrefix(ar.account.Key, "devin-") && err == nil {
						if textButton(c, a.t("Remove", "移除"), false).FontSize(12).Clicked() {
							if err := quota.RemoveDevinAccount(index); err != nil {
								a.message = err.Error()
							} else {
								a.refreshQuota(true)
							}
						}
					}
				})
				if ar.err != nil {
					ui.Text(c, ar.err.Error()).FontSize(12).TextColor(c.Theme().Danger)
				}
				if ar.result.Plan != "" {
					ui.Text(c, ar.result.Plan).FontSize(12).Padding(2, 8).Radius(4).AlignSelf(ui.Start).Background(c.Theme().SurfaceHover).TextColor(c.Theme().Accent)
				}
				for _, w := range ar.result.Windows {
					remaining := 100 - w.UsedPercent
					color := ui.Hex("#f87171")
					if remaining >= 66 {
						color = ui.Hex("#34d399")
					} else if remaining >= 33 {
						color = ui.Hex("#fbbf24")
					}
					ui.Column(c).Gap(4).Children(func() {
						ui.Row(c).Children(func() {
							ui.Text(c, w.Label).FontSize(12).Grow(1)
							ui.Text(c, fmt.Sprintf("%.0f%%", w.UsedPercent)).FontSize(12).Bold().TextColor(color)
						})
						ui.Box(c).FillWidth().Height(6).Radius(3).Background(c.Theme().SurfaceHover).Draw(func(p *ui.Painter, r ui.Rect) {
							r.W *= float32(math.Max(0, math.Min(1, w.UsedPercent/100)))
							p.Fill(r, color, 3)
						})
						if w.ResetsAt > time.Now().Unix() {
							d := time.Until(time.Unix(w.ResetsAt, 0))
							ui.Text(c, fmt.Sprintf(a.t("Resets in %dh %dm", "%d 小时 %d 分钟后重置"), int(d.Hours()), int(d.Minutes())%60)).FontSize(12).TextColor(c.Theme().TextMuted)
						}
					})
				}
				if ar.result.Extra != "" {
					ui.Text(c, ar.result.Extra).FontSize(12).TextColor(c.Theme().TextMuted)
				}
				if ar.result.Stale != "" {
					ui.Text(c, ar.result.Stale).FontSize(12).TextColor(c.Theme().Warning)
				}
			})
		}
	})
}
