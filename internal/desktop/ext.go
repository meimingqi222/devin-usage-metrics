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
	driftOpen                                            bool
	driftName                                            string
	driftBase                                            model.Agent
	undoKind, undoSummary                                string
	pasteOn, previewOn, configured                       map[model.Agent]bool
	agentFilter                                          model.Agent
	agentMenu                                            bool
	mcpModalOpen                                         bool
	mcpModalName                                         string
	mcpModalAgent                                        model.Agent
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
	} else {
		a.extAgentFilterMenu(c)
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
	reload := ui.ButtonBase(c).Label(a.t("Reload", "刷新")).FontSize(11).Padding(4, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted).Disabled(a.busy["ext"])
	if reload.Hovered() && !a.busy["ext"] {
		reload.TextColor(t.Text).Border(1, strongBorder)
	}
	reload.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, requeryIcon).Size(11, 11)
			ui.Text(c, a.t("Reload", "刷新")).NoWrap()
		})
	})
	if reload.Clicked() && !a.busy["ext"] {
		a.refreshExt()
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

func (a *App) extAgentFilterMenu(c *ui.Context) {
	t := c.Theme()
	isSkills := a.extUI.tab == "skills"
	cols := a.extColumns(isSkills)
	selectedAgent := a.extUI.agentFilter
	validSelected := false
	if selectedAgent != "" {
		for _, col := range cols {
			if col == selectedAgent {
				validSelected = true
				break
			}
		}
	}
	label := a.t("All agents", "全部 Agent")
	if validSelected {
		label = selectedAgent.Label()
	}

	pill := ui.Row(c).Shrink(0).MinWidth(0).Gap(6).Padding(4, 8).Radius(5).AlignItems(ui.Center).Background(ui.Hex("#161b26")).Border(1, t.Border).Label(a.t("Filter by agent", "按 Agent 筛选")).Focusable()
	if pill.Hovered() || a.extUI.agentMenu {
		pill.Border(1, strongBorder)
	}
	pill.Children(func() {
		ui.Text(c, label).FontSize(11).Shrink(1).MinWidth(0).SingleLine().Ellipsis("…").TextColor(t.Text)
		ui.Text(c, "▾").FontSize(9).Shrink(0).TextColor(faint(t))
	})
	if pill.Clicked() {
		a.extUI.agentMenu = !a.extUI.agentMenu
	}
	ui.PopoverBase(c, pill, &a.extUI.agentMenu, func(panel ui.Element) {
		panel.MinWidth(140).Padding(4).Gap(2).Radius(6).Background(t.Surface).Border(1, strongBorder).Shadow(0, 8, 24, 0, ui.Hex("#000000").Alpha(.55))
		panel.Children(func() {
			a.extAgentFilterItem(c, "", a.t("All agents", "全部 Agent"), !validSelected || selectedAgent == "")
			for _, agent := range cols {
				a.extAgentFilterItem(c, agent, agent.Label(), validSelected && selectedAgent == agent)
			}
		})
	})
}

func (a *App) extAgentFilterItem(c *ui.Context, agent model.Agent, label string, selected bool) {
	t := c.Theme()
	itemLabel := a.t("Filter: ", "筛选：") + label
	r := ui.Row(c.Key("extAgentFilter/"+string(agent))).Height(26).Shrink(0).Padding(0, 8).Gap(8).Radius(4).AlignItems(ui.Center).Label(itemLabel).Focusable()
	switch {
	case selected:
		r.Background(t.Accent.Alpha(.12))
	case r.Hovered():
		r.Background(t.SurfaceHover)
	}
	if r.Clicked() {
		a.extUI.agentFilter = agent
		a.extUI.agentMenu = false
	}
	r.Children(func() {
		text := ui.Text(c, label).FontSize(11).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		if selected {
			text.FontWeight(600).TextColor(t.Accent)
			ui.Icon(c, checkIcon).Size(11, 11).TextColor(t.Accent)
		} else {
			text.TextColor(t.Text)
		}
	})
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
	activeAgentFilter := a.extUI.agentFilter
	if activeAgentFilter != "" {
		hasCol := false
		for _, col := range cols {
			if col == activeAgentFilter {
				hasCol = true
				break
			}
		}
		if !hasCol {
			activeAgentFilter = ""
		}
	}
	shown := 0
	for _, row := range a.extUI.mcpRows {
		if !matchFilter(a.extUI.filter, row.Name, row.Summary) {
			continue
		}
		if activeAgentFilter != "" {
			if _, ok := row.ByAgent[activeAgentFilter]; !ok {
				continue
			}
		}
		shown++
		a.extMCPCard(c, row, cols)
	}
	if shown == 0 {
		if activeAgentFilter != "" || strings.TrimSpace(a.extUI.filter) != "" {
			ui.Text(c, a.t("No MCP servers match the current filter.", "没有符合当前筛选条件的 MCP。")).FontSize(14).TextColor(t.TextMuted)
		} else {
			ui.Text(c, a.t("No MCP servers on the installed agents.", "已安装的 Agent 上还没有 MCP。")).FontSize(14).TextColor(t.TextMuted)
		}
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

func extTag(c *ui.Context, label string, fill, border, text ui.Color) ui.Element {
	return ui.Text(c, label).FontSize(10).FontWeight(600).Padding(2, 6).Radius(3).Background(fill).Border(1, border).TextColor(text).NoWrap()
}

// extTagButton is a tag that opens a dialog; it keeps the tag look instead of
// the button look.
func extTagButton(c *ui.Context, icon *ui.SVG, label string, fill, border, text ui.Color) ui.Element {
	b := ui.ButtonBase(c).Label(label).Padding(2, 6).Radius(3).Background(fill).Border(1, border).TextColor(text)
	if b.Hovered() {
		b.Border(1, text)
	}
	b.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).Size(11, 11).TextColor(text)
			ui.Text(c, label).FontSize(10).FontWeight(600).NoWrap()
		})
	})
	return b
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

func mcpKind(m ext.MCP) string {
	if m.SSE() {
		return "SSE"
	}
	if m.URL != "" {
		return "Remote"
	}
	return "Stdio"
}

// mcpCompatible splits targets that can take src's transport. Legacy SSE has
// no equivalent in Codex or Antigravity.
func mcpCompatible(src ext.MCP, agents []model.Agent) (ok, blocked []model.Agent) {
	if !src.SSE() {
		return agents, nil
	}
	for _, agent := range agents {
		if ext.AcceptsSSE(agent) {
			ok = append(ok, agent)
		} else {
			blocked = append(blocked, agent)
		}
	}
	return ok, blocked
}

func mcpAgentLabels(agents []model.Agent) string {
	labels := make([]string, len(agents))
	for i, agent := range agents {
		labels[i] = agent.Label()
	}
	return strings.Join(labels, ", ")
}

func (a *App) extMCPCard(c *ui.Context, row ext.Row, cols []model.Agent) {
	t := c.Theme()
	srcPlace, _ := row.SourcePlacement()
	src := srcPlace.MCP
	drifted := row.DriftedAgents()
	kind := mcpKind(src)
	card := extCard(c, "mcp/"+row.Name)
	card.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, row.Name).FontSize(13).FontWeight(600).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
			switch kind {
			case "SSE":
				extTag(c, "SSE", quotaRose.Alpha(.12), quotaRose.Alpha(.35), quotaRose)
			case "Remote":
				extTag(c, "Remote", t.Accent.Alpha(.1), t.Accent.Alpha(.3), t.Accent)
			default:
				extTag(c, "Stdio", controlColor, t.Border, t.TextMuted)
			}
			if len(drifted) > 0 {
				tag := extTagButton(c, warningIcon, fmt.Sprintf(a.t("Configs differ: %d", "配置不一致：%d 处"), len(drifted)), quotaRose.Alpha(.12), quotaRose.Alpha(.35), quotaRose)
				tag.Label(a.t("Compare MCP configs", "对比 MCP 配置") + " " + row.Name)
				tag.Tooltip(fmt.Sprintf(a.t("Base: %s. Differs: %s. Click to compare and fix.", "基准：%s；不一致：%s。点击查看并修复。"), srcPlace.Agent.Label(), mcpAgentLabels(drifted)))
				if tag.Clicked() {
					a.openDriftDialog(row)
				}
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

		ui.Text(c, a.t("Command / source", "命令 / 服务源")).FontSize(10).FontWeight(600).TextColor(faint(t))

		cmd := row.Summary
		if cmd == "" {
			cmd = "—"
		}
		cap := ui.Row(c).FillWidth().MinWidth(0).Gap(8).Padding(6, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).AlignItems(ui.Center).Label(a.t("Copy summary", "复制摘要")).Focusable()
		if row.Summary != "" {
			cap.Tooltip(a.t("Click to copy summary (secrets hidden)", "点击复制摘要（密钥已隐去）"))
		}
		if cap.Hovered() {
			cap.Border(1, strongBorder)
		}
		if cap.Clicked() && row.Summary != "" {
			c.WriteClipboard(row.Summary)
			a.message = a.t("Copied summary; secrets are hidden.", "已复制摘要（密钥已隐去）。")
		}
		cap.Children(func() {
			ui.Text(c, cmd).FontSize(11).TextColor(t.TextMuted).MinWidth(0).Grow(1).Wrap()
			if row.Summary != "" {
				copyColor := faint(t)
				if cap.Hovered() {
					copyColor = t.Text
				}
				ui.Icon(c, copyIcon).Size(12, 12).Shrink(0).TextColor(copyColor)
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

// openDriftDialog defaults to the first enabled entry, which is also the row's
// source, so the dialog opens on the config the card already shows.
func (a *App) openDriftDialog(row ext.Row) {
	a.extUI.driftOpen = true
	a.extUI.driftName = row.Name
	a.extUI.driftBase = ""
	for _, agent := range ext.MatrixAgents() {
		if p, ok := row.ByAgent[agent]; ok && !p.Disabled {
			a.extUI.driftBase = agent
			return
		}
	}
}

func (a *App) extRow(name string) (ext.Row, bool) {
	for _, row := range a.extUI.mcpRows {
		if row.Name == name {
			return row, true
		}
	}
	return ext.Row{}, false
}

// extDriftDialog shows every stored copy side by side and writes the selected
// one over the others. It replaces the old implicit source pick.
func (a *App) extDriftDialog(c *ui.Context) {
	if !a.extUI.driftOpen {
		return
	}
	row, ok := a.extRow(a.extUI.driftName)
	if !ok {
		a.extUI.driftOpen = false
		return
	}
	base, hasBase := row.ByAgent[a.extUI.driftBase]
	if !hasBase || base.Disabled {
		a.extUI.driftBase = ""
		for _, agent := range ext.MatrixAgents() {
			if p, ok := row.ByAgent[agent]; ok && !p.Disabled {
				a.extUI.driftBase, base, hasBase = agent, p, true
				break
			}
		}
	}
	t := c.Theme()
	ui.DialogBase(c, &a.extUI.driftOpen, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(620).Padding(16).Gap(10).Radius(8).Background(t.Surface).Border(1, t.Border)
		ui.Text(c, a.t("MCP configs differ", "MCP 配置不一致")).FontSize(14).Bold()
		ui.Text(c, fmt.Sprintf(a.t("%s is stored differently in each agent. Pick the entry to keep; the others are overwritten and turned on.", "%s 在各 Agent 中的配置不同。选择要保留的一份，其余条目会被覆盖并启用。"), row.Name)).FontSize(12).TextColor(t.TextMuted)
		ui.Text(c, a.t("Secrets stay on this device. Undo last change can revert this write.", "密钥只留在本机；写入后可用“撤销上次变更”回退。")).FontSize(11).TextColor(faint(t))

		srcPlace, _ := row.SourcePlacement()
		srcFP := srcPlace.MCP.Fingerprint()
		for _, agent := range ext.MatrixAgents() {
			place, ok := row.ByAgent[agent]
			if !ok {
				continue
			}
			selected := hasBase && agent == a.extUI.driftBase
			bg, border := ui.Hex("#161b26"), t.Border
			if selected {
				bg, border = t.Accent.Alpha(.12), t.Accent.Alpha(.4)
			}
			line := ui.ButtonBase(c.Key("drift/"+string(agent))).FillWidth().Padding(6, 8).Gap(8).Radius(5).AlignItems(ui.Center).Background(bg).Border(1, border)
			line.Label(agent.Label() + " " + place.MCP.Summary())
			line.Disabled(place.Disabled)
			line.Tooltip(place.Path)
			line.Children(func() {
				dot := ui.Box(c).Size(8, 8).Radius(4).Border(1, t.Border).Background(ui.Hex("#161b26"))
				if selected {
					dot.Border(1, t.Accent).Background(t.Accent)
				}
				ui.Text(c, agent.Label()).FontSize(12).FontWeight(600).TextColor(t.Text).Width(88).NoWrap()
				if place.Disabled {
					extTag(c, a.t("Disabled", "已停用"), controlColor, t.Border, t.TextMuted)
				}
				summary := place.MCP.Summary()
				color := t.TextMuted
				if place.MCP.Fingerprint() != srcFP {
					color = quotaRose
				}
				txt := ui.Text(c, summary).FontSize(11).TextColor(color).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
				if summary != "" {
					txt.Tooltip(summary)
				}
			})
			if line.Clicked() && !place.Disabled {
				a.extUI.driftBase = agent
			}
		}

		var targets, blocked []model.Agent
		if hasBase && !base.Disabled {
			fp := base.MCP.Fingerprint()
			for _, agent := range ext.MatrixAgents() {
				if agent == a.extUI.driftBase {
					continue
				}
				if p, ok := row.ByAgent[agent]; ok && p.MCP.Fingerprint() != fp {
					targets = append(targets, agent)
				}
			}
			targets, blocked = mcpCompatible(base.MCP, targets)
		}
		switch {
		case !hasBase:
			ui.Text(c, a.t("Every entry is turned off. Turn one on in the card first.", "所有条目都已停用，先在卡片上启用一个条目。")).FontSize(11).TextColor(quotaRose)
		case len(targets) == 0:
			ui.Text(c, a.t("The rest already match this entry.", "其余条目已与这份配置一致。")).FontSize(11).TextColor(t.TextMuted)
		case len(blocked) > 0:
			ui.Text(c, fmt.Sprintf(a.t("%s does not accept legacy SSE; those entries are skipped.", "%s 不支持旧式 SSE，这些条目会被跳过。"), mcpAgentLabels(blocked))).FontSize(11).TextColor(quotaRose)
		}

		ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
			if settingsButton(c, a.t("Cancel", "取消"), false).Clicked() {
				a.extUI.driftOpen = false
			}
			label := a.t("Sync from the selected entry", "以所选条目为基准统一")
			if hasBase {
				label = fmt.Sprintf(a.t("Sync from %s", "以 %s 为基准统一"), base.Agent.Label())
			}
			apply := settingsButton(c, label, true)
			apply.Disabled(a.busy["ext"] || !hasBase || base.Disabled || len(targets) == 0)
			if apply.Clicked() && !a.busy["ext"] && hasBase && !base.Disabled && len(targets) > 0 {
				name, src, agents := row.Name, base.MCP, targets
				a.extUI.driftOpen = false
				a.runExt(func() error { return ext.ApplyMCP(name, src, agents) })
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
	switch {
	case unsupported:
		b.Tooltip(fmt.Sprintf(a.t("%s does not accept legacy SSE MCP.", "%s 不支持旧式 SSE MCP。"), agent.Label()))
	case on && place.Disabled:
		b.Tooltip(fmt.Sprintf(a.t("%s\nTurned off in %s. Click to turn it back on; its other settings stay.", "%s\n%s 里已停用。点击重新启用，其它设置保留。"), place.Path, agent.Label()))
	case on:
		if ext.SupportsMCPDisable(agent) {
			b.Tooltip(fmt.Sprintf(a.t("%s\nClick to pause or remove.", "%s\n点击可停用或移除。"), place.Path))
		} else {
			b.Tooltip(fmt.Sprintf(a.t("%s\nClick to remove.", "%s\n点击移除。"), place.Path))
		}
	default:
		b.Tooltip(fmt.Sprintf(a.t("Click to configure %s in %s.", "点击将 %s 配置写入 %s。"), row.Name, agent.Label()))
	}
	if !b.Clicked() || unsupported {
		return
	}
	// A paused pill turns back on in place.
	if on && place.Disabled {
		a.runExt(func() error { return ext.EnableMCP(row.Name, agent) })
		return
	}
	if on {
		if ext.SupportsMCPDisable(agent) {
			a.extUI.mcpModalOpen = true
			a.extUI.mcpModalName = row.Name
			a.extUI.mcpModalAgent = agent
		} else {
			a.askExt(a.t("Remove MCP", "移除 MCP"), a.t("Remove", "移除"), fmt.Sprintf(a.t("Remove %s (%s)?", "移除 %s（%s）？"), row.Name, agent.Label()), func() error {
				return ext.RemoveMCP(row.Name, []model.Agent{agent})
			})
		}
		return
	}
	if !ok {
		a.message = a.t("Paste an MCP snippet first.", "请先粘贴 MCP JSON。")
		return
	}
	a.runExt(func() error { return ext.ApplyMCP(row.Name, src, []model.Agent{agent}) })
}

func (a *App) extMCPModal(c *ui.Context) {
	if !a.extUI.mcpModalOpen {
		return
	}
	t := c.Theme()
	name := a.extUI.mcpModalName
	agent := a.extUI.mcpModalAgent
	ui.DialogBase(c, &a.extUI.mcpModalOpen, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .55))
		panel.Width(440).Padding(16).Gap(12).Radius(8).Background(t.Surface).Border(1, t.Border)
		ui.Text(c, fmt.Sprintf(a.t("Manage %s (%s)", "管理 %s（%s）"), name, agent.Label())).FontSize(14).Bold()
		ui.Text(c, a.t("Pause keeps this entry and its custom settings (timeouts, tool filters) in the config file with its switch turned off. Remove deletes the entry.", "「停用」会在配置文件中原地关闭开关，保留超时、工具过滤等自定义字段；「移除」会删除该条目。")).FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Justify(ui.End).Gap(8).Children(func() {
			if settingsButton(c, a.t("Cancel", "取消"), false).Clicked() {
				a.extUI.mcpModalOpen = false
			}
			pauseBtn := ui.ButtonBase(c).Label(a.t("Pause", "停用")).FontSize(12).FixedLineHeight(20).Padding(4, 12).Radius(4).Background(controlColor).Border(1, t.Border).TextColor(t.Text)
			if pauseBtn.Hovered() {
				pauseBtn.Border(1, strongBorder)
			}
			pauseBtn.Children(func() { ui.Text(c, a.t("Pause", "停用")).NoWrap() })
			if pauseBtn.Clicked() {
				a.extUI.mcpModalOpen = false
				a.extUI.queued = func() error { return ext.DisableMCP(name, agent) }
			}
			removeBtn := ui.ButtonBase(c).Label(a.t("Remove", "移除")).FontSize(12).FixedLineHeight(20).Padding(4, 12).Radius(4).Background(quotaRose.Alpha(.12)).Border(1, quotaRose.Alpha(.35)).TextColor(quotaRose)
			if removeBtn.Hovered() {
				removeBtn.Background(quotaRose.Alpha(.22)).Border(1, quotaRose)
			}
			removeBtn.Children(func() { ui.Text(c, a.t("Remove", "移除")).NoWrap() })
			if removeBtn.Clicked() {
				a.extUI.mcpModalOpen = false
				a.extUI.queued = func() error { return ext.RemoveMCP(name, []model.Agent{agent}) }
			}
		})
	})
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
	b := ui.ButtonBase(c.Key(key)).Label(label).FontSize(11).Padding(4, 10).Radius(12).Background(bg).Border(1, border).TextColor(color).Disabled(disabled)
	if b.Hovered() && !disabled {
		b.Border(1, strongBorder).TextColor(t.Text)
	}
	b.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			switch {
			case paused:
				ui.Icon(c, minusIcon).Size(10, 10).TextColor(color)
			case on:
				ui.Icon(c, checkIcon).Size(10, 10).TextColor(color)
			}
			ui.Text(c, agent.Label()).NoWrap()
		})
	})
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
	activeAgentFilter := a.extUI.agentFilter
	if activeAgentFilter != "" {
		hasCol := false
		for _, col := range cols {
			if col == activeAgentFilter {
				hasCol = true
				break
			}
		}
		if !hasCol {
			activeAgentFilter = ""
		}
	}
	shown := 0
	for _, row := range a.extUI.skillRows {
		if !matchFilter(a.extUI.filter, row.Name, row.Description) {
			continue
		}
		if activeAgentFilter != "" {
			if _, ok := row.ByAgent[activeAgentFilter]; !ok {
				continue
			}
		}
		shown++
		a.extSkillCard(c, row, cols)
	}
	if shown == 0 {
		if activeAgentFilter != "" || strings.TrimSpace(a.extUI.filter) != "" {
			ui.Text(c, a.t("No skills match the current filter.", "没有符合当前筛选条件的 Skill。")).FontSize(14).TextColor(t.TextMuted)
		} else {
			ui.Text(c, a.t("No skills on the installed agents.", "已安装的 Agent 上还没有 Skill。")).FontSize(14).TextColor(t.TextMuted)
		}
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
			ui.Text(c, row.Name).FontSize(13).FontWeight(600).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
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
		if row.Description != "" {
			ui.Text(c, row.Description).FontSize(11).TextColor(t.TextMuted).Wrap()
		}
		ui.Text(c, a.t("Active agents", "生效 Agent")).FontSize(10).FontWeight(600).TextColor(faint(t))
		ui.Row(c).Wrap().Gap(6).Children(func() {
			for _, agent := range cols {
				place, on := row.ByAgent[agent]
				canEnable := !on && (shared || len(row.Places) > 0)
				b := a.extAgentPill(c, "skill/"+row.Name+"/"+string(agent), agent, on, a.busy["ext"] || (!on && !canEnable))
				switch {
				case on && place.Inherited && place.Shared:
					b.Tooltip(a.t("Loads automatically from ~/.agents/skills (shared). Use Unshare at top right to turn off.", "从 ~/.agents/skills 自动加载（共享），如需关闭共享副本请点右上角「取消共享」。"))
				case on && place.Inherited && !place.Shared:
					b.Tooltip(fmt.Sprintf(a.t("%s\nLoads automatically from Claude's skills folder.", "%s\n从 Claude 的 skills 目录自动加载。"), place.Dir))
				case on && !place.Inherited:
					b.Tooltip(fmt.Sprintf(a.t("%s\nClick to remove this agent's copy.", "%s\n点击可移除该 Agent 副本。"), place.Dir))
				case !on && canEnable:
					b.Tooltip(fmt.Sprintf(a.t("Click to copy to %s's skills folder.", "点击复制到 %s 的 skills 目录。"), agent.Label()))
				}
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

func skillRepoURL(source string) string {
	source = strings.TrimSpace(source)
	if strings.ContainsAny(source, " \t\r\n?#") || strings.Contains(source, "..") {
		return ""
	}
	parts := strings.Split(source, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return "https://github.com/" + source
}

func (a *App) extDiscover(c *ui.Context) {
	t := c.Theme()
	if a.busy["ext-search"] {
		ui.Text(c, a.t("Searching skills.sh…", "正在搜索 skills.sh…")).FontSize(14).TextColor(t.TextMuted)
	}
	if strings.TrimSpace(a.extUI.query) == "" {
		ui.Text(c, a.t("Type a keyword in the toolbar to search skills.sh.", "在顶栏输入关键词，从 skills.sh 检索 Skill。")).FontSize(14).TextColor(t.TextMuted)
		if !a.busy["ext-search"] {
			ui.Row(c).Wrap().Gap(6).Children(func() {
				for _, tag := range []string{"mcp", "go", "react", "typescript", "testing", "git", "browser"} {
					kw := tag
					pill := ui.ButtonBase(c.Key("discoverTag/"+kw)).Label(kw).FontSize(11).Padding(4, 10).Radius(12).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(faint(t))
					if pill.Hovered() {
						pill.Border(1, strongBorder).TextColor(t.Text)
					}
					pill.Children(func() { ui.Text(c, kw).NoWrap() })
					if pill.Clicked() {
						a.extUI.query = kw
						a.searchSkills()
					}
				}
			})
		}
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
					ui.Text(c, h.Source+"@"+h.SkillID).FontSize(13).FontWeight(600).SingleLine().Ellipsis("…").MinWidth(0)
					ui.Text(c, fmt.Sprintf(a.t("%d installs", "%d 次安装"), h.Installs)).FontSize(11).TextColor(faint(t))
				})
				repoURL := skillRepoURL(h.Source)
				if a.openURL != nil && repoURL != "" {
					gh := ui.ButtonBase(c.Key("gh/"+h.ID)).Label("GitHub "+h.Source).FontSize(11).Padding(4, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted)
					if gh.Hovered() {
						gh.TextColor(t.Text).Border(1, strongBorder)
					}
					gh.Children(func() {
						ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
							ui.Icon(c, externalLinkIcon).Size(11, 11)
							ui.Text(c, "GitHub").NoWrap()
						})
					})
					if gh.Clicked() {
						a.openURL(repoURL)
					}
				}
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
	a.extMCPModal(c)
	if a.extUI.queued != nil && !a.busy["ext"] {
		fn := a.extUI.queued
		a.extUI.queued = nil
		a.runExt(fn)
	}
	a.extDriftDialog(c)
	if a.extUI.pasteOpen {
		ui.DialogBase(c, &a.extUI.pasteOpen, func(back, panel ui.Element) {
			back.Background(ui.RGBA(0, 0, 0, .55))
			panel.Width(480).Padding(16).Gap(10).Radius(8).Background(c.Theme().Surface).Border(1, c.Theme().Border)
			ui.Text(c, a.t("Add MCP from JSON", "从 JSON 批量添加 MCP")).FontSize(14).Bold()
			ui.Text(c, a.t("Paste a {\"mcpServers\":{...}} snippet. Secrets stay on this device.", "粘贴 {\"mcpServers\":{...}} 片段。密钥只留在本机。")).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.TextArea(c, &a.extUI.pasteText).Label(a.t("MCP JSON", "MCP JSON")).MinHeight(140).FillWidth()
			ui.Row(c).Gap(8).Wrap().Children(func() {
				for _, agent := range a.extColumns(false) {
					on := a.extUI.pasteOn[agent]
					pill := a.extAgentPill(c, "paste/"+string(agent), agent, on, false)
					if pill.Clicked() {
						a.extUI.pasteOn[agent] = !on
					}
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
			repoURL := skillRepoURL(a.extUI.preview.Hit.Source)
			ui.Row(c).AlignItems(ui.Center).Children(func() {
				ui.Text(c, a.extUI.preview.Hit.Source+"@"+a.extUI.preview.Hit.SkillID).FontSize(14).Bold().Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
				if a.openURL != nil && repoURL != "" {
					gh := ui.ButtonBase(c).Label("GitHub").FontSize(11).Padding(3, 8).Radius(4).Background(ui.Hex("#161b26")).Border(1, c.Theme().Border).TextColor(c.Theme().TextMuted)
					if gh.Hovered() {
						gh.TextColor(c.Theme().Text).Border(1, strongBorder)
					}
					gh.Children(func() {
						ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
							ui.Icon(c, externalLinkIcon).Size(11, 11)
							ui.Text(c, "GitHub").NoWrap()
						})
					})
					if gh.Clicked() {
						a.openURL(repoURL)
					}
				}
			})
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
			ui.Row(c).Gap(8).Wrap().Children(func() {
				for _, agent := range a.extColumns(true) {
					on := a.extUI.previewOn[agent]
					pill := a.extAgentPill(c, "preview/"+string(agent), agent, on, false)
					if pill.Clicked() {
						a.extUI.previewOn[agent] = !on
					}
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
