package desktop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/ext"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

type extUIState struct {
	tab, filter, query                                   string
	pasteOpen, previewOpen, confirmOpen, loaded, hasUndo bool
	previewErr                                           bool
	pasteText, confirmTitle, confirmBody, confirmAction  string
	undoKind, undoSummary                                string
	pasteOn, previewOn, configured                       map[model.Agent]bool
	expanded                                             map[string]bool
	mcpRows                                              []ext.Row
	skillRows                                            []ext.SkillRow
	hits                                                 []ext.Hit
	preview                                              ext.SkillPreview
	confirm                                              func() error
	// queued is a confirmed write waiting for the running "ext" job; work()
	// would otherwise drop it silently.
	queued func() error
}

func (a *App) extEntry(c *ui.Context) {
	t := c.Theme()
	r := ui.Row(c).Margin(0, 10, 10).Padding(7, 10).Gap(7).Radius(6).Background(controlColor).Border(1, t.Border).AlignItems(ui.Center).Label(a.t("MCP & Skills", "MCP 与 Skills")).Focusable()
	switch {
	case a.screen == "ext":
		r.Background(t.Accent.Alpha(.12)).Border(1, t.Accent.Alpha(.35))
	case r.Hovered():
		r.Background(t.SurfaceHover).Border(1, strongBorder)
	}
	if r.Clicked() {
		a.screen = "ext"
		if a.extUI.tab == "" {
			a.extUI.tab = "mcp"
		}
		a.refreshExt()
	}
	n := len(a.extUI.mcpRows) + len(a.extUI.skillRows)
	r.Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(tabActive).Shadow(0, 0, 6, 0, tabActive.Alpha(.5))
		ui.Text(c, a.t("MCP & Skills", "MCP 与 Skills")).FontSize(12).FontWeight(500).Grow(1)
		if n > 0 {
			ui.Text(c, fmt.Sprint(n)).FontSize(11).FontWeight(600).Padding(1, 6).Radius(10).Background(ui.Hex("#202737")).TextColor(t.TextMuted)
		}
	})
}

func (a *App) refreshExt() {
	a.work("ext", func() func() {
		mcp, err1 := ext.InventoryMCP()
		skills, err2 := ext.InventorySkills()
		kind, summary, ok := ext.LastUndo()
		configured := ext.ConfiguredAgents()
		return func() {
			a.extUI.configured = configured
			a.storeExtInventory(mcp, skills, kind, summary, ok)
			if note := extInventoryNote(err1, err2); note != "" {
				a.message = note
			}
		}
	})
}

func (a *App) storeExtInventory(mcp []ext.Row, skills []ext.SkillRow, kind, summary string, ok bool) {
	a.extUI.mcpRows, a.extUI.skillRows, a.extUI.loaded = mcp, skills, true
	a.extUI.undoKind, a.extUI.undoSummary, a.extUI.hasUndo = kind, summary, ok
}

func extInventoryNote(err1, err2 error) string {
	if err1 != nil {
		return err1.Error()
	}
	if err2 != nil {
		return err2.Error()
	}
	return ""
}

func (a *App) extColumns(skills bool) []model.Agent {
	var out []model.Agent
	for _, agent := range ext.MatrixAgents() {
		// An agent can have MCP or skills before its first session.
		if !a.installed[agent] && !a.extUI.configured[agent] {
			continue
		}
		if skills && !ext.SkillSupported(agent) {
			continue
		}
		if !skills && !ext.MCPSupported(agent) {
			continue
		}
		out = append(out, agent)
	}
	return out
}

func (a *App) extToolbar(c *ui.Context) {
	t := c.Theme()
	control(c, 6).Children(func() {
		for _, tab := range []struct{ key, en, zh string }{{"mcp", "MCP Servers", "MCP Servers"}, {"skills", "Installed Skills", "已安装 Skills"}, {"discover", "Discover", "Discover"}} {
			b := segment(c, a.t(tab.en, tab.zh), a.extUI.tab == tab.key, 12, 12, 4, tabActive, ui.Hex("#ffffff"))
			if a.extUI.tab == tab.key {
				b.Shadow(0, 1, 3, 0, ui.Hex("#000000").Alpha(.3))
			}
			if b.Clicked() {
				a.extUI.tab = tab.key
			}
		}
	})
	placeholder := a.t("Filter", "筛选")
	if a.extUI.tab == "discover" {
		placeholder = a.t("Search skills.sh", "搜索 skills.sh")
	}
	value := &a.extUI.filter
	if a.extUI.tab == "discover" {
		value = &a.extUI.query
	}
	input := ui.TextInputBase(c, value).Label(placeholder).Placeholder(placeholder).FontSize(11).FixedLineHeight(18).Padding(4, 8).Radius(5).Width(180).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.Text)
	if input.Focused() {
		input.Border(1, t.Accent)
	}
	if a.extUI.tab == "discover" && input.Submitted() && strings.TrimSpace(a.extUI.query) != "" {
		a.searchSkills()
	}
	if a.extUI.tab == "discover" {
		goSearch := ui.ButtonBase(c).Label(a.t("Search", "搜索")).FontSize(11).Padding(4, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted).Disabled(a.busy["ext-search"] || strings.TrimSpace(a.extUI.query) == "")
		if goSearch.Hovered() {
			goSearch.TextColor(t.Text).Border(1, strongBorder)
		}
		goSearch.Children(func() { ui.Text(c, a.t("Search", "搜索")).NoWrap() })
		if goSearch.Clicked() {
			a.searchSkills()
		}
	}
	ui.Spacer(c)
	if a.extUI.tab == "mcp" {
		add := ui.ButtonBase(c).Label(a.t("Add MCP", "批量添加 MCP")).FontSize(11).Padding(4, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted)
		if add.Hovered() {
			add.TextColor(t.Text).Border(1, strongBorder)
		}
		add.Children(func() { ui.Text(c, a.t("Add MCP", "批量添加 MCP")).NoWrap() })
		if add.Clicked() {
			a.extUI.pasteOpen = true
			clip := strings.TrimSpace(c.ReadClipboard())
			if _, err := ext.ParsePaste(clip); err == nil {
				a.extUI.pasteText = clip
			} else {
				a.extUI.pasteText = ""
			}
			a.extUI.pasteOn = map[model.Agent]bool{}
			for _, agent := range a.extColumns(false) {
				a.extUI.pasteOn[agent] = true
			}
		}
	}
	undo := ui.ButtonBase(c).Label(a.t("Undo last change", "撤销上次变更")).FontSize(11).Padding(4, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted).Disabled(!a.extUI.hasUndo || a.busy["ext"])
	if undo.Hovered() && a.extUI.hasUndo {
		undo.TextColor(t.Text).Border(1, strongBorder)
	}
	undo.Children(func() { ui.Text(c, a.t("Undo last change", "撤销上次变更")).NoWrap() })
	if a.extUI.hasUndo {
		undo.Tooltip(a.extUI.undoSummary)
	}
	if undo.Clicked() && a.extUI.hasUndo {
		a.runExt(func() error { return ext.UndoLast() })
	}
}

func (a *App) searchSkills() {
	q := strings.TrimSpace(a.extUI.query)
	a.work("ext-search", func() func() {
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		defer cancel()
		hits, err := ext.SearchSkills(ctx, q)
		return func() {
			if err != nil {
				a.message = err.Error()
				return
			}
			a.extUI.hits = hits
		}
	})
}

func (a *App) runExt(fn func() error) {
	a.runExtNotice(func() (string, error) {
		return "", fn()
	})
}

func (a *App) runExtNotice(fn func() (string, error)) {
	a.work("ext", func() func() {
		notice, err := fn()
		mcp, err1 := ext.InventoryMCP()
		skills, err2 := ext.InventorySkills()
		kind, summary, ok := ext.LastUndo()
		configured := ext.ConfiguredAgents()
		return func() {
			a.extUI.configured = configured
			if err != nil {
				a.message = err.Error()
			} else if notice != "" {
				a.message = notice
			} else {
				a.message = a.t("Updated. Restart the agent session to load it.", "已更新，请开启新的 Agent 会话。")
			}
			a.storeExtInventory(mcp, skills, kind, summary, ok)
			if err == nil {
				if note := extInventoryNote(err1, err2); note != "" {
					a.message = a.message + "\n" + note
				}
			}
		}
	})
}

func (a *App) extView(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(7, 7).Radius(4).Background(tabActive)
		ui.Text(c, a.t("MCP & Skills", "MCP 与 Skills")).FontSize(15).Bold()
	})
	ui.Text(c, a.t("Local agent configs only. Writes stay on this device and never enter sync.", "只改本机 Agent 配置，不参与同步。")).FontSize(12).TextColor(t.TextMuted)
	if a.busy["ext"] && !a.extUI.loaded {
		ui.Text(c, a.t("Reading local MCP and skill directories…", "正在读取本机 MCP 与 Skill 目录…")).FontSize(14).TextColor(t.TextMuted)
		return
	}
	switch a.extUI.tab {
	case "skills":
		a.extSkillsMatrix(c)
	case "discover":
		a.extDiscover(c)
	default:
		a.extMCPMatrix(c)
	}
}

func matchFilter(filter, name, extra string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), filter) || strings.Contains(strings.ToLower(extra), filter)
}

func (a *App) extMCPMatrix(c *ui.Context) {
	t := c.Theme()
	cols := a.extColumns(false)
	if len(cols) == 0 {
		ui.Text(c, a.t("No MCP-capable agents installed on this device.", "本机没有可写入 MCP 的 Agent。")).FontSize(14).TextColor(t.TextMuted)
		return
	}
	shown := 0
	for _, row := range a.extUI.mcpRows {
		if !matchFilter(a.extUI.filter, row.Name, row.Summary) {
			continue
		}
		shown++
		a.extMCPCard(c, row, cols)
	}
	if shown == 0 {
		ui.Text(c, a.t("No MCP servers on the installed agents.", "已安装的 Agent 上还没有 MCP。")).FontSize(14).TextColor(t.TextMuted)
	}
}

func extCard(c *ui.Context, key string) ui.Element {
	t := c.Theme()
	card := ui.Column(c.Key(key)).Gap(10).Padding(12, 14).Radius(6).Background(t.Surface).Border(1, t.Border).Clip().MinWidth(0)
	if card.Hovered() {
		card.Border(1, strongBorder)
	}
	return card
}

func extTag(c *ui.Context, label string, fill, border, text ui.Color) {
	ui.Text(c, label).FontSize(10).FontWeight(600).Padding(2, 6).Radius(3).Background(fill).Border(1, border).TextColor(text).NoWrap()
}

func extGhostButton(c *ui.Context, label string) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).FontSize(11).Padding(3, 8).Radius(4).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted)
	if b.Hovered() {
		b.TextColor(t.Text).Border(1, strongBorder)
	}
	b.Children(func() { ui.Text(c, label).NoWrap() })
	return b
}

func extMiniGhostButton(c *ui.Context, label string) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).FontSize(10).Padding(2, 6).Radius(3).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted)
	if b.Hovered() {
		b.TextColor(t.Text).Border(1, strongBorder)
	}
	b.Children(func() { ui.Text(c, label).NoWrap() })
	return b
}

func mcpKind(m ext.MCP) string {
	if m.SSE() {
		return "SSE"
	}
	if m.URL != "" {
		return "Remote"
	}
	return "Stdio"
}

func (a *App) extMCPCard(c *ui.Context, row ext.Row, cols []model.Agent) {
	t := c.Theme()
	src, _ := row.Source()
	kind := mcpKind(src)
	card := extCard(c, "mcp/"+row.Name)
	card.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, row.Name).FontSize(13).FontWeight(600).Grow(1).MinWidth(0).NoWrap().Ellipsis("…")
			switch kind {
			case "SSE":
				extTag(c, "SSE", quotaRose.Alpha(.12), quotaRose.Alpha(.35), quotaRose)
			case "Remote":
				extTag(c, "Remote", t.Accent.Alpha(.1), t.Accent.Alpha(.3), t.Accent)
			default:
				extTag(c, "Stdio", controlColor, t.Border, t.TextMuted)
			}
			if row.Drift {
				extTag(c, a.t("Drift", "漂移 ⚠"), quotaRose.Alpha(.12), quotaRose.Alpha(.35), quotaRose)
			}
			if extAllDisabled(row) {
				extTag(c, a.t("Disabled", "已停用"), controlColor, t.Border, t.TextMuted)
			}
			remove := extGhostButton(c, a.t("Remove", "移除"))
			remove.Label(a.t("Remove everywhere", "全局移除") + " " + row.Name)
			remove.Disabled(a.busy["ext"])
			if remove.Clicked() && !a.busy["ext"] {
				agents := make([]model.Agent, 0, len(row.ByAgent))
				for agent := range row.ByAgent {
					agents = append(agents, agent)
				}
				a.askExt(a.t("Remove MCP", "移除 MCP"), a.t("Remove", "移除"), fmt.Sprintf(a.t("Remove %s from every listed agent on this device?", "从本机列出的 Agent 中移除 %s？"), row.Name), func() error {
					return ext.RemoveMCP(row.Name, agents)
				})
			}
		})

		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Command / source", "命令 / 服务源")).FontSize(10).FontWeight(600).TextColor(faint(t)).Grow(1)
			if row.Summary != "" {
				ui.Row(c).Gap(6).Children(func() {
					// The summary hides secrets and URL paths, so it is not a runnable command.
					copyBtn := extMiniGhostButton(c, a.t("Copy summary", "复制摘要"))
					if copyBtn.Clicked() {
						c.WriteClipboard(row.Summary)
						a.message = a.t("Copied summary; secrets are hidden.", "已复制摘要（密钥已隐去）。")
					}
					if len(row.Summary) > 60 || strings.Contains(row.Summary, "\n") {
						isExp := a.extUI.expanded != nil && a.extUI.expanded[row.Name]
						lbl := a.t("Expand", "展开")
						if isExp {
							lbl = a.t("Collapse", "收起")
						}
						expBtn := extMiniGhostButton(c, lbl)
						if expBtn.Clicked() {
							if a.extUI.expanded == nil {
								a.extUI.expanded = make(map[string]bool)
							}
							a.extUI.expanded[row.Name] = !isExp
						}
					}
				})
			}
		})

		cmd := row.Summary
		if cmd == "" {
			cmd = "—"
		}
		isExpanded := a.extUI.expanded != nil && a.extUI.expanded[row.Name]
		cap := ui.Row(c).FillWidth().MinWidth(0).Clip().Padding(6, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).AlignItems(ui.Center).Label(a.t("Copy summary", "复制摘要")).Focusable()
		if cap.Hovered() {
			cap.Border(1, strongBorder)
		}
		if cap.Clicked() && row.Summary != "" {
			c.WriteClipboard(row.Summary)
			a.message = a.t("Copied summary; secrets are hidden.", "已复制摘要（密钥已隐去）。")
		}
		cap.Children(func() {
			txt := ui.Text(c, cmd).FontSize(11).TextColor(t.TextMuted).MinWidth(0).Grow(1)
			if !isExpanded {
				txt.NoWrap().Ellipsis("…")
			} else {
				txt.Wrap()
			}
		})

		ui.Text(c, a.t("Active agents", "生效 Agent")).FontSize(10).FontWeight(600).TextColor(faint(t))
		ui.Row(c).Wrap().Gap(6).Children(func() {
			for _, agent := range cols {
				a.extMCPPill(c, row, agent)
			}
		})
	})
}

func (a *App) extMCPPill(c *ui.Context, row ext.Row, agent model.Agent) {
	place, on := row.ByAgent[agent]
	src, ok := row.Source()
	unsupported := !on && ok && src.SSE() && !ext.AcceptsSSE(agent)
	disabled := a.busy["ext"] || unsupported
	b := a.extAgentPillState(c, "mcp/"+row.Name+"/"+string(agent), agent, on, on && place.Disabled, disabled)
	if unsupported {
		b.Tooltip(fmt.Sprintf(a.t("%s does not accept legacy SSE MCP.", "%s 不支持旧式 SSE MCP。"), agent.Label()))
	}
	if on && place.Disabled {
		b.Tooltip(fmt.Sprintf(a.t("Turned off in %s. Click to turn it back on; its other settings stay.", "%s 里已停用。点击重新启用，其它设置保留。"), agent.Label()))
	}
	if !b.Clicked() || unsupported {
		return
	}
	// A paused pill turns back on in place; an active pill removes the entry.
	if on && place.Disabled {
		a.runExt(func() error { return ext.EnableMCP(row.Name, agent) })
		return
	}
	if on {
		a.askExt(a.t("Remove MCP", "移除 MCP"), a.t("Remove", "移除"), fmt.Sprintf(a.t("Remove %s (%s)?", "移除 %s（%s）？"), row.Name, agent.Label()), func() error {
			return ext.RemoveMCP(row.Name, []model.Agent{agent})
		})
		return
	}
	if !ok {
		a.message = a.t("Paste an MCP snippet first.", "请先粘贴 MCP JSON。")
		return
	}
	a.runExt(func() error { return ext.ApplyMCP(row.Name, src, []model.Agent{agent}) })
}

func (a *App) extAgentPill(c *ui.Context, key string, agent model.Agent, on, disabled bool) ui.Element {
	return a.extAgentPillState(c, key, agent, on, false, disabled)
}

// extAgentPillState draws one agent pill; paused means the entry exists but the
// agent's own config has it turned off.
func (a *App) extAgentPillState(c *ui.Context, key string, agent model.Agent, on, paused, disabled bool) ui.Element {
	t := c.Theme()
	label := agent.Label()
	switch {
	case paused:
		label = "– " + label
	case on:
		label = "✓ " + label
	}
	bg, border, color := ui.Hex("#161b26"), t.Border, faint(t)
	if paused {
		bg, border, color = controlColor, tabActive.Alpha(.3), t.TextMuted
	} else if on {
		bg, border, color = tabActive.Alpha(.2), tabActive.Alpha(.45), t.Text
	} else if disabled {
		bg, border, color = ui.Hex("#10131a"), t.Border.Alpha(.35), faint(t).Alpha(.35)
	}
	b := ui.ButtonBase(c.Key(key)).Label(agent.Label()).FontSize(11).Padding(4, 10).Radius(12).Background(bg).Border(1, border).TextColor(color).Disabled(disabled)
	if b.Hovered() && !disabled {
		b.Border(1, strongBorder).TextColor(t.Text)
	}
	b.Children(func() { ui.Text(c, label).NoWrap() })
	return b
}

func extAllDisabled(row ext.Row) bool {
	for _, p := range row.ByAgent {
		if !p.Disabled {
			return false
		}
	}
	return len(row.ByAgent) > 0
}

func (a *App) extSkillsMatrix(c *ui.Context) {
	t := c.Theme()
	cols := a.extColumns(true)
	if len(cols) == 0 {
		ui.Text(c, a.t("No skill-capable agents installed on this device.", "本机没有可写入 Skill 的 Agent。")).FontSize(14).TextColor(t.TextMuted)
		return
	}
	shown := 0
	for _, row := range a.extUI.skillRows {
		if !matchFilter(a.extUI.filter, row.Name, "") {
			continue
		}
		shown++
		a.extSkillCard(c, row, cols)
	}
	if shown == 0 {
		ui.Text(c, a.t("No skills on the installed agents.", "已安装的 Agent 上还没有 Skill。")).FontSize(14).TextColor(t.TextMuted)
	}
}

func (a *App) extSkillCard(c *ui.Context, row ext.SkillRow, cols []model.Agent) {
	t := c.Theme()
	onCount := 0
	for _, agent := range cols {
		if _, on := row.ByAgent[agent]; on {
			onCount++
		}
	}
	shared := false
	for _, p := range row.Places {
		if p.Shared {
			shared = true
			break
		}
	}
	card := extCard(c, "skill/"+row.Name)
	card.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, row.Name).FontSize(13).FontWeight(600).Grow(1).MinWidth(0).NoWrap().Ellipsis("…")
			if shared {
				extTag(c, a.t("shared", "共享"), controlColor, t.Border, t.TextMuted)
			}
			extTag(c, fmt.Sprintf(a.t("%d agents on", "已启用 %d 个"), onCount), controlColor, t.Border, t.TextMuted)
			if shared {
				unshare := extGhostButton(c, a.t("Unshare", "取消共享"))
				unshare.Label(a.t("Unshare", "取消共享") + " " + row.Name)
				unshare.Disabled(a.busy["ext"])
				if unshare.Clicked() && !a.busy["ext"] {
					a.askExt(a.t("Unshare skill", "取消共享 Skill"), a.t("Unshare", "取消共享"), fmt.Sprintf(a.t("Remove %s from ~/.agents/skills? Codex, OpenCode and Pi will lose the auto-loaded copy. Copies in each agent's own directory stay.", "从 ~/.agents/skills 移除 %s？Codex、OpenCode、Pi 会一起关掉自动加载的副本；各 Agent 自己目录里的副本保留。"), row.Name), func() error {
						return ext.UnshareSkill(row.Name)
					})
				}
			}
			remove := extGhostButton(c, a.t("Remove", "移除"))
			remove.Label(a.t("Remove everywhere", "全局移除") + " " + row.Name)
			remove.Disabled(a.busy["ext"])
			if remove.Clicked() && !a.busy["ext"] {
				a.askExt(a.t("Remove skill", "移除 Skill"), a.t("Remove", "移除"), fmt.Sprintf(a.t("Remove %s from every agent directory and ~/.agents/skills?", "从各 Agent 目录和 ~/.agents/skills 移除 %s？"), row.Name), func() error {
					return ext.RemoveSkill(row.Name, nil, true)
				})
			}
		})
		ui.Text(c, a.t("Active agents", "生效 Agent")).FontSize(10).FontWeight(600).TextColor(faint(t))
		ui.Row(c).Wrap().Gap(6).Children(func() {
			for _, agent := range cols {
				place, on := row.ByAgent[agent]
				canEnable := !on && (shared || len(row.Places) > 0)
				b := a.extAgentPill(c, "skill/"+row.Name+"/"+string(agent), agent, on, a.busy["ext"] || (!on && !canEnable))
				if !b.Clicked() {
					continue
				}
				if on {
					if place.Inherited {
						if place.Shared {
							a.message = fmt.Sprintf(a.t("%s loads this skill from ~/.agents/skills. Turning off this pill does not delete the shared folder, so other agents stay on. Use Unshare to remove the shared copy.", "%s 从 ~/.agents/skills 自动加载。点掉这个胶囊不会删除共享目录，以免其它 Agent 一起关掉。要去掉共享副本，请用「取消共享」。"), agent.Label())
						} else {
							a.message = fmt.Sprintf(a.t("%s loads this skill from Claude's skills folder. Remove it from Claude to turn it off here.", "%s 从 Claude 的 skills 目录自动加载。要从这里关闭，请先从 Claude 移除。"), agent.Label())
						}
						continue
					}
					a.askExt(a.t("Remove skill", "移除 Skill"), a.t("Remove", "移除"), fmt.Sprintf(a.t("Remove %s (%s)?", "移除 %s（%s）？"), row.Name, agent.Label()), func() error {
						return ext.RemoveSkill(row.Name, []model.Agent{agent}, false)
					})
					continue
				}
				a.runExt(func() error { return ext.EnableSkill(row.Name, agent) })
			}
		})
	})
}

func (a *App) extDiscover(c *ui.Context) {
	t := c.Theme()
	if a.busy["ext-search"] {
		ui.Text(c, a.t("Searching skills.sh…", "正在搜索 skills.sh…")).FontSize(14).TextColor(t.TextMuted)
	}
	if strings.TrimSpace(a.extUI.query) == "" {
		ui.Text(c, a.t("Type a keyword in the toolbar to search skills.sh.", "在顶栏输入关键词，从 skills.sh 检索 Skill。")).FontSize(14).TextColor(t.TextMuted)
		return
	}
	if len(a.extUI.hits) == 0 && !a.busy["ext-search"] {
		ui.Text(c, a.t("No matching skills.", "没有匹配的 Skill。")).FontSize(14).TextColor(t.TextMuted)
		return
	}
	for _, hit := range a.extUI.hits {
		h := hit
		card := extCard(c, "hit/"+h.ID)
		card.Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Column(c).Grow(1).MinWidth(0).Clip().Children(func() {
					ui.Text(c, h.Source+"@"+h.SkillID).FontSize(13).FontWeight(600).NoWrap().Ellipsis("…").MinWidth(0)
					ui.Text(c, fmt.Sprintf(a.t("%d installs", "%d 次安装"), h.Installs)).FontSize(11).TextColor(faint(t))
				})
				install := ui.ButtonBase(c).Label(a.t("Install", "安装")+" "+h.ID).FontSize(11).Padding(4, 10).Radius(5).Background(tabActive).TextColor(ui.Hex("#ffffff"))
				install.Children(func() { ui.Text(c, a.t("Install", "安装")).NoWrap() })
				if install.Clicked() {
					a.openSkillPreview(h)
				}
			})
		})
	}
}

func (a *App) openSkillPreview(hit ext.Hit) {
	a.work("ext-preview", func() func() {
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		defer cancel()
		p, err := ext.PreviewSkill(ctx, hit)
		return func() {
			a.extUI.previewErr = err != nil
			if err != nil {
				p = ext.SkillPreview{Hit: hit, Description: err.Error()}
			}
			a.extUI.preview = p
			a.extUI.previewOpen = true
			a.extUI.previewOn = map[model.Agent]bool{}
			for _, agent := range a.extColumns(true) {
				a.extUI.previewOn[agent] = true
			}
		}
	})
}

func (a *App) askExt(title, action, body string, fn func() error) {
	a.extUI.confirmTitle, a.extUI.confirmAction, a.extUI.confirmBody, a.extUI.confirm, a.extUI.confirmOpen = title, action, body, fn, true
}

func (a *App) extDialogs(c *ui.Context) {
	if a.extUI.confirmOpen {
		if ui.AlertDialog(c, &a.extUI.confirmOpen, a.extUI.confirmTitle, a.extUI.confirmBody, a.t("Cancel", "取消"), a.extUI.confirmAction) == 1 {
			a.extUI.queued, a.extUI.confirm = a.extUI.confirm, nil
		}
	}
	if a.extUI.queued != nil && !a.busy["ext"] {
		fn := a.extUI.queued
		a.extUI.queued = nil
		a.runExt(fn)
	}
	if a.extUI.pasteOpen {
		ui.DialogBase(c, &a.extUI.pasteOpen, func(back, panel ui.Element) {
			back.Background(ui.RGBA(0, 0, 0, .55))
			panel.Width(480).Padding(16).Gap(10).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border)
			ui.Text(c, a.t("Add MCP from JSON", "从 JSON 批量添加 MCP")).FontSize(14).Bold()
			ui.Text(c, a.t("Paste a {\"mcpServers\":{...}} snippet. Secrets stay on this device.", "粘贴 {\"mcpServers\":{...}} 片段。密钥只留在本机。")).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.TextArea(c, &a.extUI.pasteText).Label(a.t("MCP JSON", "MCP JSON")).MinHeight(140).FillWidth()
			ui.Row(c).Gap(8).Children(func() {
				for _, agent := range a.extColumns(false) {
					on := a.extUI.pasteOn[agent]
					ui.Checkbox(c.Key("paste/"+string(agent)), &on, agent.Label())
					a.extUI.pasteOn[agent] = on
				}
			})
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				if settingsButton(c, a.t("Cancel", "取消"), false).Clicked() {
					a.extUI.pasteOpen = false
				}
				apply := settingsButton(c, a.t("Apply", "应用"), true)
				apply.Disabled(a.busy["ext"])
				if apply.Clicked() {
					a.applyPaste()
				}
			})
		})
	}
	if a.extUI.previewOpen {
		ui.DialogBase(c, &a.extUI.previewOpen, func(back, panel ui.Element) {
			back.Background(ui.RGBA(0, 0, 0, .55))
			panel.Width(480).Padding(16).Gap(10).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border)
			ui.Text(c, a.extUI.preview.Hit.Source+"@"+a.extUI.preview.Hit.SkillID).FontSize(14).Bold()
			desc := a.extUI.preview.Description
			if desc == "" && !a.extUI.previewErr {
				desc = a.t("No description in SKILL.md.", "SKILL.md 没有 description。")
			}
			ui.Text(c, desc).FontSize(12).TextColor(c.Theme().TextMuted)
			if n := len(a.extUI.preview.Files); n > 0 {
				shown := a.extUI.preview.Files
				if len(shown) > 8 {
					shown = shown[:8]
				}
				ui.Text(c, strings.Join(shown, "\n")).FontSize(11).TextColor(c.Theme().TextMuted)
				if n > 8 {
					ui.Text(c, fmt.Sprintf(a.t("%d files", "%d 个文件"), n)).FontSize(11).TextColor(c.Theme().TextMuted)
				}
			}
			ui.Row(c).Gap(8).Children(func() {
				for _, agent := range a.extColumns(true) {
					on := a.extUI.previewOn[agent]
					ui.Checkbox(c.Key("preview/"+string(agent)), &on, agent.Label())
					a.extUI.previewOn[agent] = on
				}
			})
			ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
				if settingsButton(c, a.t("Cancel", "取消"), false).Clicked() {
					a.extUI.previewOpen = false
				}
				install := settingsButton(c, a.t("Install", "安装"), true)
				install.Disabled(a.extUI.previewErr || a.busy["ext"])
				if install.Clicked() && !a.extUI.previewErr {
					a.installPreview()
				}
			})
		})
	}
}

func (a *App) applyPaste() {
	list, err := ext.ParsePaste(a.extUI.pasteText)
	if err != nil {
		a.message = err.Error()
		return
	}
	var agents []model.Agent
	for agent, on := range a.extUI.pasteOn {
		if on {
			agents = append(agents, agent)
		}
	}
	if len(agents) == 0 {
		a.message = a.t("Select at least one agent.", "请至少选择一个 Agent。")
		return
	}
	a.extUI.pasteOpen = false
	a.runExtNotice(func() (string, error) {
		skipped, err := ext.ApplyMCPs(list, agents)
		if err != nil {
			return "", err
		}
		if skipped {
			return a.t("Updated. Skipped agents that do not accept legacy SSE URLs.", "已更新，并跳过不支持旧式 SSE 的 Agent。"), nil
		}
		return "", nil
	})
}

func (a *App) installPreview() {
	var agents []model.Agent
	for agent, on := range a.extUI.previewOn {
		if on {
			agents = append(agents, agent)
		}
	}
	if len(agents) == 0 {
		a.message = a.t("Select at least one agent.", "请至少选择一个 Agent。")
		return
	}
	preview := a.extUI.preview
	a.extUI.previewOpen = false
	a.work("ext", func() func() {
		var err error
		if len(preview.Zip) > 0 {
			err = ext.InstallSkillArchive(ext.SkillArchive{Hit: preview.Hit, Zip: preview.Zip, Rel: preview.RelPath, Files: preview.Files, Markdown: preview.Markdown}, agents)
		} else {
			ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
			defer cancel()
			err = ext.InstallSkill(ctx, preview.Hit, agents)
		}
		mcp, err1 := ext.InventoryMCP()
		skills, err2 := ext.InventorySkills()
		kind, summary, ok := ext.LastUndo()
		configured := ext.ConfiguredAgents()
		return func() {
			a.extUI.configured = configured
			if err != nil {
				a.message = err.Error()
			} else {
				a.message = a.t("Installed. Restart the agent session to load it.", "已安装，请开启新的 Agent 会话。")
			}
			a.storeExtInventory(mcp, skills, kind, summary, ok)
			if err == nil {
				if note := extInventoryNote(err1, err2); note != "" {
					a.message = a.message + "\n" + note
				}
			}
		}
	})
}
