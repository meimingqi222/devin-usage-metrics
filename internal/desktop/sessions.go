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
	cfg := map[string]any{}
	if b, err := os.ReadFile(preferencesPath()); err == nil {
		if err = json.Unmarshal(b, &cfg); err != nil {
			a.message = err.Error()
			return
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	keys := []string{}
	for key, v := range a.sessionsUI.hidden {
		if v {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	cfg["sessions_group_by_project"] = a.sessionsUI.group
	cfg["sessions_hide_subagents"] = a.sessionsUI.hideSubagents
	cfg["hidden_session_keys"] = keys
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(preferencesPath()), 0700)
	}
	if err == nil {
		var f *os.File
		f, err = os.CreateTemp(filepath.Dir(preferencesPath()), ".sessions-*")
		if err == nil {
			defer os.Remove(f.Name())
			_, err = f.Write(b)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Rename(f.Name(), preferencesPath())
			}
		}
	}
	if err != nil {
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
}

func (a *App) sessionCost(s model.Session) (float64, int, int) {
	d := a.allData()
	if a.sessionCosts == nil {
		a.sessionCosts = map[sessionIdentity]sessionUsage{}
		models := map[sessionIdentity]string{}
		for _, session := range d.Sessions {
			models[sessionIdentity{session.Agent, session.DeviceID, session.Key}] = session.DisplayModel()
		}
		for _, turn := range d.Turns {
			key := sessionIdentity{turn.Agent, turn.DeviceID, turn.SessionKey}
			usage := a.sessionCosts[key]
			usage.turns++
			if v, ok := pricing.Cost(turn, models[key]); ok {
				usage.cost += v
				usage.priced++
			}
			a.sessionCosts[key] = usage
		}
	}
	usage := a.sessionCosts[sessionIdentity{s.Agent, s.DeviceID, s.Key}]
	return usage.cost, usage.priced, usage.turns
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

func (a *App) sessionsView(c *ui.Context) {
	var sessions []model.Session
	hasModes := false
	reviews := 0
	for _, s := range a.allData().Sessions {
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
		if !strings.Contains(strings.ToLower(s.Title+" "+s.ID+" "+s.WorkingDirectory+" "+s.DisplayModel()), strings.ToLower(strings.TrimSpace(a.search))) {
			continue
		}
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastActivityAt > sessions[j].LastActivityAt })
	sessions = sessions[:min(500, len(sessions))]
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
					ui.Text(c, path).MaxWidth(320).MinWidth(0).FontSize(12).TextColor(c.Theme().TextMuted).NoWrap().Ellipsis("…")
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
			ui.Text(c, id).Grow(1).MinWidth(0).FontSize(12).NoWrap().Ellipsis("…")
			if s.Agent == model.Devin && s.Source == "cli-next" {
				ui.Text(c, "next").FontSize(12).TextColor(c.Theme().TextMuted.Alpha(.7))
			}
		})
		ui.Text(c, s.Title).Grow(1).MinWidth(0).FontSize(12).NoWrap().Ellipsis("…")
		if modes {
			ui.Text(c, s.AgentMode).Width(60).FontSize(12).NoWrap().Ellipsis("…")
		}
		modelText := s.SelectedModel
		if modelText == "" {
			modelText = s.DisplayModel()
		}
		adaptive := modelText == "adaptive"
		if adaptive {
			modelText = "adaptive → " + s.DisplayModel()
		}
		m := ui.Text(c, modelText).Width(140).FontSize(12).NoWrap().Ellipsis("…")
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
			if textButton(c, "✕", false).Label(a.t("Close session details", "关闭会话详情")).Clicked() {
				a.selected = nil
			}
		})
		ttft := []float64{}
		cost := 0.0
		turns, priced := 0, 0
		for _, turn := range a.allData().Turns {
			if turn.Agent == s.Agent && turn.DeviceID == s.DeviceID && turn.SessionKey == s.Key {
				turns++
				if turn.TTFTMs > 0 {
					ttft = append(ttft, turn.TTFTMs)
				}
				if v, ok := pricing.Cost(turn, s.DisplayModel()); ok {
					cost += v
					priced++
				}
			}
		}
		sort.Float64s(ttft)
		median := 0.0
		if len(ttft) > 0 {
			median = ttft[len(ttft)/2]
		}
		for _, v := range [][2]string{{a.t("Session", "会话"), s.ID + " · " + s.Source + " · " + s.AgentMode}, {a.t("Workdir", "工作目录"), s.WorkingDirectory}, {a.t("Model", "模型"), s.DisplayModel()}, {a.t("Time", "时间"), time.Unix(s.CreatedAt, 0).Format(time.DateTime) + " → " + time.Unix(s.LastActivityAt, 0).Format(time.DateTime)}, {"Tokens", fmt.Sprintf(a.t("In %s · Out %s · Cache Write %s · Cache Read %s · Total %s", "输入 %s · 输出 %s · 缓存写入 %s · 缓存读取 %s · 总计 %s"), formatTokens(s.InputTokens), formatTokens(s.OutputTokens), formatTokens(s.CacheCreationTokens), formatTokens(s.CachedTokens), formatTokens(s.TotalTokens()))}, {a.t("Cost", "费用"), fmt.Sprintf("$%.4f (%d/%d)", cost, priced, turns)}, {a.t("Activity", "活动"), fmt.Sprintf(a.t("%.0f messages · %d turns · Median TTFT %.0f ms", "%.0f 条消息 · %d 轮 · TTFT 中位数 %.0f ms"), s.AgentMessages, turns, median)}} {
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
