package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
)

type sessionsUIState struct {
	group, hideSubagents, showHidden bool
	hidden, collapsed                map[string]bool
}

func loadSessionUI() sessionsUIState {
	s := sessionsUIState{group: true, hideSubagents: true, hidden: map[string]bool{}, collapsed: map[string]bool{}}
	var cfg struct {
		Group  *bool    `json:"sessions_group_by_project"`
		Hide   *bool    `json:"sessions_hide_subagents"`
		Hidden []string `json:"hidden_session_keys"`
	}
	b, _ := os.ReadFile(preferencesPath())
	_ = json.Unmarshal(b, &cfg)
	if cfg.Group != nil {
		s.group = *cfg.Group
	}
	if cfg.Hide != nil {
		s.hideSubagents = *cfg.Hide
	}
	for _, key := range cfg.Hidden {
		s.hidden[key] = true
	}
	return s
}
func (a *App) saveSessionUI() {
	a.sessionList.valid = false
	keys := []string{}
	for key, v := range a.sessionsUI.hidden {
		if v {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if err := savePreferences(map[string]any{"sessions_group_by_project": a.sessionsUI.group, "sessions_hide_subagents": a.sessionsUI.hideSubagents, "hidden_session_keys": keys}); err != nil {
		a.message = err.Error()
	}
}

func (a *App) copy(c *ui.Context, text string) {
	c.WriteClipboard(text)
	a.message = a.t("Copied", "已复制")
}
func sessionProject(s model.Session) string {
	if s.WorkingDirectory == "" {
		return "—"
	}
	return filepath.Base(filepath.Clean(s.WorkingDirectory))
}
func subagent(s model.Session) bool {
	return strings.Contains(strings.ToLower(s.AgentMode), "review") || strings.Contains(s.Key, "subagent") || strings.Contains(s.Key, "/sub/")
}
func resume(s model.Session) string {
	if s.Agent == model.Claude || s.Agent == model.Codex {
		return string(s.Agent) + " resume " + s.ID
	}
	return ""
}

// Session controls intentionally follow the original div-based toolbar, not
// the shared navigation button treatment.
func sessionButton(c *ui.Context, label string, active bool) ui.Element {
	b := textButton(c, label, false).Height(28).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border)
	if b.Hovered() {
		b.Background(c.Theme().SurfaceHover).TextColor(c.Theme().Text)
	}
	if active {
		b.Background(c.Theme().SurfaceHover).TextColor(c.Theme().Accent).Border(1, c.Theme().Accent)
	}
	return b
}

type sessionIdentity struct {
	agent       model.Agent
	device, key string
}

type sessionUsage struct {
	cost          float64
	priced, turns int
	ttft          []float64
}

func (a *App) sessionCost(s model.Session) (float64, int, int) {
	u := a.sessionUsage(s)
	return u.cost, u.priced, u.turns
}

// sessionUsage aggregates every session in one pass over the turns and keeps
// the result until the data changes.
func (a *App) sessionUsage(s model.Session) sessionUsage {
	d := a.allData()
	if a.sessionCosts == nil {
		a.sessionCosts = map[sessionIdentity]sessionUsage{}
		models := map[sessionIdentity]string{}
		for _, session := range d.Sessions {
			models[sessionIdentity{session.Agent, session.DeviceID, session.Key}] = session.DisplayModel()
		}
		var calculator pricing.Calculator
		for _, turn := range d.Turns {
			key := sessionIdentity{turn.Agent, turn.DeviceID, turn.SessionKey}
			usage := a.sessionCosts[key]
			usage.turns++
			if turn.TTFTMs > 0 {
				usage.ttft = append(usage.ttft, turn.TTFTMs)
			}
			if v, ok := calculator.Cost(turn, models[key]); ok {
				usage.cost += v
				usage.priced++
			}
			a.sessionCosts[key] = usage
		}
	}
	return a.sessionCosts[sessionIdentity{s.Agent, s.DeviceID, s.Key}]
}

func (a *App) sessionCostText(s model.Session) string {
	cost, priced, turns := a.sessionCost(s)
	if priced == 0 {
		return "—"
	}
	if priced < turns {
		return "≥" + trayMoney(cost)
	}
	return trayMoney(cost)
}

type sessionListCache struct {
	valid                     bool
	search, device            string
	showHidden, hideSubagents bool
	sessions                  []model.Session
	hasModes                  bool
	reviews                   int
}

func (a *App) visibleSessions() ([]model.Session, bool, int) {
	d := a.allData()
	cache := &a.sessionList
	if cache.valid && cache.search == a.search && cache.device == a.device && cache.showHidden == a.sessionsUI.showHidden && cache.hideSubagents == a.sessionsUI.hideSubagents {
		return cache.sessions, cache.hasModes, cache.reviews
	}
	search := strings.ToLower(strings.TrimSpace(a.search))
	var sessions []model.Session
	hasModes := false
	reviews := 0
	for _, s := range d.Sessions {
		if s.Agent != a.agent || (a.device != "" && s.DeviceID != a.device) {
			continue
		}
		hasModes = hasModes || strings.TrimSpace(s.AgentMode) != ""
		if !a.sessionsUI.hidden[s.Key] && subagent(s) {
			reviews++
		}
		if a.sessionsUI.hidden[s.Key] != a.sessionsUI.showHidden {
			continue
		}
		if !a.sessionsUI.showHidden && a.sessionsUI.hideSubagents && subagent(s) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(s.Title+" "+s.ID+" "+s.WorkingDirectory+" "+s.DisplayModel()), search) {
			continue
		}
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastActivityAt > sessions[j].LastActivityAt })
	if len(sessions) > 500 {
		// Do not retain every filtered row behind the visible 500-row slice.
		sessions = append([]model.Session(nil), sessions[:500]...)
	}
	*cache = sessionListCache{valid: true, search: a.search, device: a.device, showHidden: a.sessionsUI.showHidden, hideSubagents: a.sessionsUI.hideSubagents, sessions: sessions, hasModes: hasModes, reviews: reviews}
	return sessions, hasModes, reviews
}

func (a *App) sessionsView(c *ui.Context) {
	sessions, hasModes, reviews := a.visibleSessions()
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.TextInput(c, &a.search).Width(240).Height(28).FontSize(12).Radius(6).Background(ui.Hex("#101014")).Border(1, c.Theme().Border).Placeholder(a.t("Search title, ID, path, model...", "搜索标题、ID、路径、模型...")).Label(a.t("Search sessions", "搜索会话"))
		group := a.t("Flat List", "时间平铺")
		if a.sessionsUI.group {
			group = a.t("Group by Project", "按项目分组")
		}
		if sessionButton(c, group, a.sessionsUI.group).Clicked() {
			a.sessionsUI.group = !a.sessionsUI.group
			a.saveSessionUI()
		}
		if a.sessionsUI.group && len(sessions) > 0 {
			if sessionButton(c, a.t("Collapse All", "全部折叠"), false).Clicked() {
				for _, s := range sessions {
					project := sessionProject(s)
					if s.WorkingDirectory == "" {
						project = a.t("Unspecified Project", "未指定项目")
					}
					a.sessionsUI.collapsed[project] = true
				}
			}
			if sessionButton(c, a.t("Expand All", "全部展开"), false).Clicked() {
				clear(a.sessionsUI.collapsed)
			}
		}
		reviewLabel := a.t("Show Reviews", "包含审查任务")
		if a.sessionsUI.hideSubagents {
			reviewLabel = a.t("Hide Reviews", "隐藏审查任务")
			if reviews > 0 {
				reviewLabel += fmt.Sprintf(" (%d)", reviews)
			}
		}
		if sessionButton(c, reviewLabel, a.sessionsUI.hideSubagents).Clicked() {
			a.sessionsUI.hideSubagents = !a.sessionsUI.hideSubagents
			a.saveSessionUI()
		}
		recycleLabel := fmt.Sprintf(a.t("Recycle Bin (%d)", "回收站 (%d)"), len(a.sessionsUI.hidden))
		if a.sessionsUI.showHidden {
			recycleLabel = a.t("Active", "活跃会话")
		}
		recycle := sessionButton(c, recycleLabel, false)
		if a.sessionsUI.showHidden {
			red := ui.Hex("#f87171")
			recycle.Background(red.Alpha(.15)).TextColor(red).Border(1, red.Alpha(.4))
		}
		if recycle.Clicked() {
			a.sessionsUI.showHidden = !a.sessionsUI.showHidden
			a.selected = nil
		}
	})
	if a.selected != nil {
		a.sessionDetail(c, *a.selected)
	}
	if len(sessions) == 0 {
		msg := fmt.Sprintf(a.t("No %s sessions found", "没有找到 %s 会话"), a.agent.Label())
		if a.sessionsUI.showHidden {
			msg = a.t("Recycle Bin", "回收站")
		} else if query := strings.TrimSpace(a.search); query != "" {
			msg = fmt.Sprintf(a.t("No sessions matching %q", "没有找到与 \"%s\" 匹配的会话"), query)
		}
		ui.Row(c).Padding(32).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Justify(ui.Center).Children(func() {
			ui.Text(c, msg).FontSize(14).TextColor(c.Theme().TextMuted)
		})
		return
	}
	if !a.sessionsUI.group {
		ui.Column(c).Gap(0).Padding(12).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
			a.sessionHeader(c, hasModes)
			for _, s := range sessions {
				a.sessionRow(c, s, hasModes)
			}
		})
	} else {
		projects := []string{}
		groups := map[string][]model.Session{}
		for _, s := range sessions {
			p := sessionProject(s)
			if s.WorkingDirectory == "" {
				p = a.t("Unspecified Project", "未指定项目")
			}
			if _, ok := groups[p]; !ok {
				projects = append(projects, p)
			}
			groups[p] = append(groups[p], s)
		}
		for _, project := range projects {
			ui.Column(c.Key("project-card/"+project)).Gap(0).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Clip().Children(func() {
				r := ui.Row(c.Key("project/"+project)).Height(36).Padding(0, 12).Gap(8).AlignItems(ui.Center).Background(c.Theme().SurfaceHover).Label(project).Focusable()
				if !a.sessionsUI.collapsed[project] {
					r.BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border)
				}
				if r.Clicked() {
					a.sessionsUI.collapsed[project] = !a.sessionsUI.collapsed[project]
				}
				r.Children(func() {
					arrow := "▾"
					if a.sessionsUI.collapsed[project] {
						arrow = "▸"
					}
					ui.Text(c, arrow).FontSize(12).TextColor(c.Theme().TextMuted)
					ui.Text(c, project).FontSize(14).Bold()
					path := groups[project][0].WorkingDirectory
					ui.Text(c, path).MaxWidth(320).MinWidth(0).FontSize(12).TextColor(c.Theme().TextMuted).SingleLine().Ellipsis("…")
					ui.Spacer(c)
					var tokens, cost float64
					priced := 0
					for _, s := range groups[project] {
						tokens += s.TotalTokens()
						v, n, _ := a.sessionCost(s)
						cost += v
						priced += n
					}
					for _, label := range []string{fmt.Sprintf(a.t("%d sessions", "%d 会话"), len(groups[project])), formatTokens(tokens) + " Tokens"} {
						ui.Text(c, label).FontSize(12).Padding(1, 8).Radius(4).Background(c.Theme().Surface).Border(1, c.Theme().Border).TextColor(c.Theme().TextMuted)
					}
					if priced > 0 {
						ui.Text(c, trayMoney(cost)).FontSize(12).Padding(1, 8).Radius(4).Background(c.Theme().Surface).Border(1, c.Theme().Border).TextColor(c.Theme().Accent)
					}
					if path != "" && sessionButton(c, a.t("Open Folder", "打开目录"), false).Height(22).Padding(1, 8).Radius(2).Clicked() && a.openFolder != nil {
						a.openFolder(path)
					}
				})
				if !a.sessionsUI.collapsed[project] {
					a.sessionHeader(c, hasModes)
					for _, s := range groups[project] {
						a.sessionRow(c, s, hasModes)
					}
				}
			})
		}
	}
}

func (a *App) sessionHeader(c *ui.Context, modes bool) {
	ui.Row(c).Height(26).Padding(0, 8).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border).Children(func() {
		ui.Text(c, a.t("Last Active", "最后活跃")).Width(80).FontSize(12).TextColor(c.Theme().TextMuted)
		ui.Text(c, a.t("Session", "会话")).Width(110).FontSize(12).TextColor(c.Theme().TextMuted)
		ui.Text(c, a.t("Title", "标题")).Grow(1).MinWidth(0).FontSize(12).TextColor(c.Theme().TextMuted)
		if modes {
			ui.Text(c, a.t("Mode", "模式")).Width(60).FontSize(12).TextColor(c.Theme().TextMuted)
		}
		for i, label := range []string{a.t("Model", "模型"), a.t("Msgs", "消息"), a.t("Tokens", "Tokens"), a.t("Cost", "窗口费用")} {
			e := ui.Text(c, label).Width([]float32{140, 42, 65, 55}[i]).FontSize(12).TextColor(c.Theme().TextMuted)
			if i > 0 {
				e.TextAlign(ui.End)
			}
		}
		ui.Box(c).Width(105)
	})
}

func (a *App) sessionRow(c *ui.Context, s model.Session, modes bool) {
	r := ui.Row(c.Key(s.DeviceID+"/"+s.Key)).Height(28).Shrink(0).Padding(0, 8).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border.Alpha(.4)).Label(s.Title).Focusable()
	if r.Hovered() || (a.selected != nil && a.selected.Key == s.Key) {
		r.Background(c.Theme().SurfaceHover)
	}
	if r.Clicked() {
		copy := s
		a.selected = &copy
	}
	r.Children(func() {
		ui.Text(c, time.Unix(s.LastActivityAt, 0).Format("01-02 15:04")).Width(80).FontSize(12).TextColor(c.Theme().TextMuted).NoWrap()
		id := s.ID
		if len([]rune(id)) > 16 {
			id = string([]rune(id)[:16]) + "…"
		}
		ui.Row(c).Width(110).Shrink(0).MinWidth(0).Gap(4).AlignItems(ui.Center).Clip().Children(func() {
			ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(modelColor(s.DisplayModel()))
			ui.Text(c, id).Grow(1).MinWidth(0).FontSize(12).SingleLine().Ellipsis("…")
			if s.Agent == model.Devin && s.Source == "cli-next" {
				ui.Text(c, "next").FontSize(12).TextColor(c.Theme().TextMuted.Alpha(.7))
			}
			if s.Agent == model.Antigravity && s.Source == "cli" {
				ui.Text(c, "CLI").FontSize(12).TextColor(c.Theme().TextMuted.Alpha(.7))
			}
		})
		ui.Text(c, s.Title).Grow(1).MinWidth(0).FontSize(12).SingleLine().Ellipsis("…")
		if modes {
			ui.Text(c, s.AgentMode).Width(60).FontSize(12).SingleLine().Ellipsis("…")
		}
		modelText := s.SelectedModel
		if modelText == "" {
			modelText = s.DisplayModel()
		}
		adaptive := modelText == "adaptive"
		if adaptive {
			modelText = "adaptive → " + s.DisplayModel()
		}
		m := ui.Text(c, modelText).Width(140).FontSize(12).SingleLine().Ellipsis("…")
		if adaptive {
			m.TextColor(c.Theme().Accent)
		}
		ui.Text(c, fmt.Sprintf("%.0f", s.AgentMessages)).Width(42).FontSize(12).TextAlign(ui.End)
		ui.Text(c, formatTokens(s.TotalTokens())).Width(65).FontSize(12).TextAlign(ui.End)
		ui.Text(c, a.sessionCostText(s)).Width(55).FontSize(12).TextAlign(ui.End)
		ui.Row(c).Width(105).Gap(4).AlignItems(ui.Center).Justify(ui.End).Children(func() {
			if sessionButton(c, "ID", false).Label(a.t("Copy ID", "复制 ID")).Height(22).Padding(1, 4).Radius(2).Clicked() {
				a.copy(c, s.ID)
			}
			if cmd := resume(s); cmd != "" {
				if sessionButton(c, a.t("Resume", "续读"), false).TextColor(c.Theme().Accent).Height(22).Padding(1, 4).Radius(2).Clicked() {
					a.copy(c, cmd)
				}
			}
			label := a.t("Hide", "隐藏")
			if a.sessionsUI.showHidden {
				label = a.t("Restore", "恢复")
			}
			toggle := sessionButton(c, label, false).Height(22).Padding(1, 4).Radius(2)
			if a.sessionsUI.showHidden {
				toggle.TextColor(ui.Hex("#3ddc97"))
			}
			if toggle.Hovered() {
				color := ui.Hex("#f87171")
				if a.sessionsUI.showHidden {
					color = ui.Hex("#3ddc97")
				}
				toggle.TextColor(color).Border(1, color)
			}
			if toggle.Clicked() {
				if a.sessionsUI.showHidden {
					delete(a.sessionsUI.hidden, s.Key)
				} else {
					a.sessionsUI.hidden[s.Key] = true
				}
				a.selected = nil
				a.saveSessionUI()
			}
		})
	})
}

func (a *App) sessionDetail(c *ui.Context, s model.Session) {
	ui.Column(c).Padding(12).Gap(8).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Accent.Alpha(.4)).Children(func() {
		ui.Row(c).Children(func() {
			ui.Text(c, s.Title).FontSize(14).Bold().Grow(1)
			closeBtn := textButton(c, "", false).Label(a.t("Close session details", "关闭会话详情"))
			closeBtn.Children(func() { ui.Icon(c, closeIcon).Size(12, 12) })
			if closeBtn.Clicked() {
				a.selected = nil
			}
		})
		u := a.sessionUsage(s)
		costText := a.t("unknown (no pricing match)", "未知（无匹配定价）")
		if u.priced == u.turns && u.turns > 0 {
			costText = fmt.Sprintf(a.t("%s (per-turn pricing over %d turns in window)", "%s（窗口内 %d 轮逐轮计价）"), fmt.Sprintf("$%.4f", u.cost), u.turns)
		} else if u.priced > 0 {
			costText = fmt.Sprintf(a.t("%s (only %d/%d turns are priced; actual cost is at least this amount)", "%s（仅 %d/%d 轮可定价，实际费用不低于此值）"), fmt.Sprintf("$%.4f", u.cost), u.priced, u.turns)
		}
		activity := fmt.Sprintf(a.t("%.0f agent messages · %d turns in window", "%.0f 条 agent 消息 · 窗口内 %d 轮"), s.AgentMessages, u.turns)
		if len(u.ttft) > 0 {
			ttft := append([]float64(nil), u.ttft...)
			sort.Float64s(ttft)
			activity = fmt.Sprintf(a.t("%.0f agent messages · %d turns in window · median TTFT %.0f ms", "%.0f 条 agent 消息 · 窗口内 %d 轮 · TTFT 中位 %.0f ms"), s.AgentMessages, u.turns, ttft[len(ttft)/2])
		}
		for _, v := range [][2]string{{a.t("Session", "会话"), s.ID + " · " + s.Source + " · " + s.AgentMode}, {a.t("Workdir", "工作目录"), s.WorkingDirectory}, {a.t("Model", "模型"), s.DisplayModel()}, {a.t("Time", "时间"), time.Unix(s.CreatedAt, 0).Format(time.DateTime) + " → " + time.Unix(s.LastActivityAt, 0).Format(time.DateTime)}, {"Tokens", fmt.Sprintf(a.t("In %s · Out %s · Cache Write %s · Cache Read %s · Total %s", "输入 %s · 输出 %s · 缓存写入 %s · 缓存读取 %s · 总计 %s"), formatTokens(s.InputTokens), formatTokens(s.OutputTokens), formatTokens(s.CacheCreationTokens), formatTokens(s.CachedTokens), formatTokens(s.TotalTokens()))}, {a.t("Cost", "费用"), costText}, {a.t("Activity", "活动"), activity}} {
			ui.Row(c).Gap(8).Children(func() {
				ui.Text(c, v[0]).Width(84).FontSize(12).TextColor(c.Theme().TextMuted)
				ui.Text(c, v[1]).Grow(1).FontSize(12)
			})
		}
		ui.Row(c).Gap(8).Padding(8, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(c.Theme().Border.Alpha(.4)).Children(func() {
			if textButton(c, a.t("Copy ID", "复制 ID"), true).Clicked() {
				a.copy(c, s.ID)
			}
			if cmd := resume(s); cmd != "" {
				if textButton(c, a.t("Resume", "续读"), true).Tooltip(cmd).Clicked() {
					a.copy(c, cmd)
				}
			}
			if textButton(c, a.t("Copy path", "复制路径"), true).Clicked() {
				a.copy(c, s.WorkingDirectory)
			}
		})
	})
}
