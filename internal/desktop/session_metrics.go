package desktop

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// requestPage is how many requests the table shows at first and adds per
// "Show more", so a session of thousands of calls stays responsive.
const requestPage = 20

// transcriptState is the conversation dialog: read on demand from the
// session's own files, kept only while the dialog is open.
type transcriptState struct {
	open, loading bool
	session       model.Session
	parts         []data.Part
	cut           bool
	err           error
	// expanded flips a steps block (negative keys) from allSteps, and
	// opens a long step (its part index) in full.
	expanded map[int]bool
	allSteps bool
	// query is the search; matches are its hits in order, cur the one
	// shown, and seen the query they were found for.
	query, seen string
	matches     []transcriptMatch
	cur         int
	// round is the round the rail marks: the one at the top of the view
	// as it scrolls, or one jumped to, which stays pinned until it has
	// shown and gone, or has not shown within pinFor of pinAt because the
	// user scrolled away while the view glided to it. jump scrolls to it
	// or to the current match in this frame, and railMoved brings its rail
	// item into view.
	round           int
	jump            transcriptJump
	pinned, pinSeen bool
	pinAt           time.Time
	// md renders the agent's replies and thinking, which are Markdown.
	md, thinkMD *markdown.Options
	railMoved   bool
}

type transcriptMatch struct{ part, at int }

type transcriptJump int

const (
	jumpNone transcriptJump = iota
	jumpMatch
	jumpRound
)

// findAll returns where q occurs in text, ignoring case where lowering
// keeps the text's byte offsets.
func findAll(text, q string) []int {
	lt, lq := strings.ToLower(text), strings.ToLower(q)
	if len(lt) != len(text) || len(lq) != len(q) {
		lt, lq = text, q
	}
	var at []int
	for i := 0; ; {
		j := strings.Index(lt[i:], lq)
		if j < 0 {
			return at
		}
		at = append(at, i+j)
		i += j + len(lq)
	}
}

func findMatches(parts []data.Part, q string) []transcriptMatch {
	if strings.TrimSpace(q) == "" {
		return nil
	}
	var ms []transcriptMatch
	for i, p := range parts {
		for _, at := range findAll(p.Text, q) {
			ms = append(ms, transcriptMatch{i, at})
		}
	}
	return ms
}

// partMatches returns the index of part's first match and how many it has.
func (st *transcriptState) partMatches(part int) (first, n int) {
	first = sort.Search(len(st.matches), func(k int) bool { return st.matches[k].part >= part })
	for n = 0; first+n < len(st.matches) && st.matches[first+n].part == part; n++ {
	}
	return first, n
}

func (st *transcriptState) curPart() int {
	if st.cur < len(st.matches) {
		return st.matches[st.cur].part
	}
	return -1
}

// marked draws text with the search's hits in it highlighted, the current
// one strongest.
func (a *App) marked(c *ui.Context, text string, part int) ui.Element {
	return a.markedRange(c, text, part, 0, len(text))
}

// markedRange draws text[at:end], a piece of the part's text, with the
// hits that fall inside it highlighted.
func (a *App) markedRange(c *ui.Context, text string, part, at, end int) ui.Element {
	st := &a.sessionsUI.transcript
	first, n := st.partMatches(part)
	var spans []ui.Span
	last := at
	for k := first; k < first+n; k++ {
		m, me := st.matches[k].at, st.matches[k].at+len(st.query)
		if m < last || me > end {
			continue
		}
		spans = append(spans, ui.Span{Text: text[last:m]})
		hit := ui.Span{Text: text[m:me], Background: ui.Hex("#d9a441").Alpha(.3), Radius: 2}
		if k == st.cur {
			hit.Background, hit.Color = ui.Hex("#f0b44c"), ui.Hex("#141414")
		}
		spans = append(spans, hit)
		last = me
	}
	if len(spans) == 0 {
		return ui.Text(c, text[at:end])
	}
	spans = append(spans, ui.Span{Text: text[last:end]})
	return ui.RichText(c, spans...)
}

func (a *App) sessionCacheHit(c *ui.Context, s model.Session) {
	t := c.Theme()
	r, ok := analytics.CacheHitRate(s.InputTokens, s.CachedTokens, s.CacheCreationTokens)
	if !ok {
		ui.Text(c, "—").FontSize(12).TextColor(t.TextMuted)
		return
	}
	ui.Box(c).Width(72).Height(4).Shrink(0).Radius(2).Background(ui.Hex("#272733")).Clip().Children(func() {
		ui.Box(c).Width(float32(72 * r)).Height(4).Background(cacheReadColor)
	})
	ui.Text(c, fmt.Sprintf("%.1f%%", r*100)).FontSize(12).TextColor(t.Accent)
	ui.Text(c, fmt.Sprintf(a.t("· read %s / prompt %s", "· 读取 %s / 提示词 %s"), formatTokens(s.CachedTokens), formatTokens(s.InputTokens+s.CachedTokens+s.CacheCreationTokens))).Grow(1).FontSize(12).TextColor(t.TextMuted)
}

func (a *App) speedWhy(r analytics.SpeedResult) string {
	switch r {
	case analytics.SpeedNoTiming:
		return a.t("no timing in the local history", "本地记录没有耗时")
	case analytics.SpeedNoTTFT:
		return a.t("no time to first token", "缺首 token 时间")
	case analytics.SpeedBurst:
		return a.t("arrived in one piece, no speed", "整段一次返回，无法测速")
	}
	return ""
}

func (a *App) sessionSpeed(c *ui.Context, u sessionUsage) {
	t := c.Theme()
	measured := u.results[analytics.SpeedMeasured]
	if measured > 0 && u.speedMs > 0 {
		ui.Text(c, fmt.Sprintf("%.0f tok/s", u.speedOut*1000/u.speedMs)).FontSize(12).TextColor(t.Accent)
		ui.Text(c, fmt.Sprintf(a.t("· %d/%d requests timed · excludes the wait for the first token", "· %d/%d 条有可用计时 · 不含等待首 token"), measured, u.turns)).Grow(1).FontSize(12).TextColor(t.TextMuted)
		return
	}
	if u.floorCount > 0 && u.floorMs > 0 {
		ui.Text(c, fmt.Sprintf("≥ %.0f tok/s", u.floorOut*1000/u.floorMs)).FontSize(12).TextColor(t.Accent)
		ui.Text(c, fmt.Sprintf(a.t("· %d/%d lower-bound · includes wait for first token", "· %d/%d 条下限估算 · 含等待首 token"), u.floorCount, u.turns)).Grow(1).FontSize(12).TextColor(t.TextMuted)
		return
	}
	why := a.speedWhy(analytics.SpeedNoTiming)
	if u.results[analytics.SpeedBurst] > 0 {
		why = a.speedWhy(analytics.SpeedBurst)
	} else if u.results[analytics.SpeedNoTTFT] > 0 {
		why = a.speedWhy(analytics.SpeedNoTTFT)
	}
	ui.Text(c, "— · "+why).Grow(1).FontSize(12).TextColor(t.TextMuted)
}

// sessionRequests is the expandable table of the session's model calls in
// the loaded window, newest first.
func (a *App) sessionRequests(c *ui.Context, u sessionUsage) {
	t := c.Theme()
	if u.turns == 0 {
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(8, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(t.Border.Alpha(.4)).Children(func() {
		ui.Text(c, fmt.Sprintf(a.t("Requests in window: %d", "窗口内请求明细 %d 条"), u.turns)).FontSize(12)
		ui.Spacer(c)
		label := a.t("Expand", "展开")
		if a.sessionsUI.requestsOpen {
			label = a.t("Collapse", "收起")
		}
		if textButton(c, label, false).TextColor(t.Accent).Clicked() {
			a.sessionsUI.requestsOpen = !a.sessionsUI.requestsOpen
			if a.sessionsUI.requestLimit == 0 {
				a.sessionsUI.requestLimit = requestPage
			}
		}
	})
	if !a.sessionsUI.requestsOpen {
		return
	}
	turns := a.allData().Turns
	widths := []float32{90, 64, 64, 64, 60, 64, 64, 80}
	cell := func(s string, i int, color ui.Color, tip string) {
		e := ui.Text(c, s).Width(widths[i]).Shrink(0).FontSize(12).TextColor(color).NoWrap()
		if i > 0 {
			e.TextAlign(ui.End)
		}
		if tip != "" {
			e.Tooltip(tip)
		}
	}
	ui.Column(c).Gap(0).Padding(4, 10).Radius(4).Background(ui.Hex("#131318")).Label(a.t("Requests", "请求明细")).Children(func() {
		ui.Row(c).Gap(8).Padding(4, 0).BorderWidth(0, 0, 1, 0).BorderColor(ui.Hex("#242430")).Children(func() {
			for i, h := range []string{a.t("Time", "时间"), a.t("New in", "新输入"), a.t("Cache rd", "缓存读"), a.t("Cache wr", "缓存写"), a.t("Out", "输出"), a.t("Hit rate", "命中率"), "TTFT", a.t("Speed", "速度")} {
				cell(h, i, t.TextMuted, "")
			}
		})
		limit := max(a.sessionsUI.requestLimit, requestPage)
		shown := 0
		for j := len(u.turnIdx) - 1; j >= 0 && shown < limit; j-- {
			shown++
			turn := turns[u.turnIdx[j]]
			ui.Row(c.Key("req/"+strconv.Itoa(u.turnIdx[j]))).Gap(8).Padding(3, 0).BorderWidth(0, 0, 1, 0).BorderColor(ui.Hex("#242430")).Children(func() {
				cell(time.Unix(turn.CreatedAt, 0).Format("01-02 15:04"), 0, t.TextMuted, turn.Model)
				cell(formatTokens(turn.InputTokens), 1, t.Text, "")
				cell(formatTokens(turn.CacheReadTokens), 2, t.Text, "")
				cell(formatTokens(turn.CacheCreationTokens), 3, t.Text, "")
				cell(formatTokens(turn.OutputTokens), 4, t.Text, "")
				if r, ok := analytics.CacheHitRate(turn.InputTokens, turn.CacheReadTokens, turn.CacheCreationTokens); ok {
					cell(fmt.Sprintf("%.1f%%", r*100), 5, t.Text, "")
				} else {
					cell("—", 5, t.TextMuted, "")
				}
				if turn.TTFTMs > 0 {
					cell(fmt.Sprintf("%.1f s", turn.TTFTMs/1000), 6, t.Text, "")
				} else {
					cell("—", 6, t.TextMuted, a.speedWhy(analytics.SpeedNoTTFT))
				}
				if w, r := analytics.Decode(turn.OutputTokens, turn.TTFTMs, turn.TotalTimeMs); r == analytics.SpeedMeasured {
					cell(fmt.Sprintf("%.0f tok/s", turn.OutputTokens*1000/w), 7, t.Text, "")
				} else if fw, ok := analytics.Floor(turn.OutputTokens, turn.TotalTimeMs); ok {
					cell(fmt.Sprintf("≥ %.0f tok/s", turn.OutputTokens*1000/fw), 7, t.TextMuted, a.t("includes wait for first token; actual speed is at least this", "含等待首 token，真实速度不低于此值"))
				} else {
					cell("—", 7, t.TextMuted, a.speedWhy(r))
				}
			})
		}
		if shown < len(u.turnIdx) {
			if textButton(c, fmt.Sprintf(a.t("Show more (%d left)", "显示更多（还有 %d 条）"), len(u.turnIdx)-shown), false).TextColor(t.Accent).Clicked() {
				a.sessionsUI.requestLimit = limit + 50
			}
		}
	})
}

// canShowTranscript: only this device's sessions have files to read.
func (a *App) canShowTranscript(s model.Session) bool {
	return data.TranscriptSupported(s.Agent) && s.DeviceID == a.localID
}

func (a *App) openTranscript(s model.Session) {
	files := a.loaded.FileSessions
	a.sessionsUI.transcript = transcriptState{open: true, loading: true, session: s, expanded: map[int]bool{}, round: -1}
	ctx := a.ctx
	a.work("transcript", func() func() {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		parts, cut, err := data.Transcript(c, s, files)
		return func() {
			st := &a.sessionsUI.transcript
			if !st.open || st.session.Key != s.Key {
				return
			}
			st.loading, st.parts, st.cut, st.err = false, parts, cut, err
		}
	})
}

// transcriptBlock is what the dialog draws: a user's message, the agent's
// reply, or a run of thinking and tool steps between them, folded.
type transcriptBlock struct {
	kind  data.PartKind // PartUser, PartAssistant, or PartToolCall for steps
	start int           // index of its first part
	parts []data.Part
}

func transcriptBlocks(parts []data.Part) [][]transcriptBlock {
	var turns [][]transcriptBlock
	var cur []transcriptBlock
	for i, p := range parts {
		switch p.Kind {
		case data.PartUser:
			if len(cur) > 0 {
				turns = append(turns, cur)
			}
			cur = []transcriptBlock{{kind: data.PartUser, start: i, parts: []data.Part{p}}}
		case data.PartAssistant:
			cur = append(cur, transcriptBlock{kind: data.PartAssistant, start: i, parts: []data.Part{p}})
		default:
			if n := len(cur); n > 0 && cur[n-1].kind == data.PartToolCall {
				cur[n-1].parts = append(cur[n-1].parts, p)
			} else {
				cur = append(cur, transcriptBlock{kind: data.PartToolCall, start: i, parts: []data.Part{p}})
			}
		}
	}
	if len(cur) > 0 {
		turns = append(turns, cur)
	}
	return turns
}

const monoFont = "SF Mono, Menlo, Consolas, monospace"

var (
	userBubble = ui.Hex("#1c2a3f")
	guideColor = ui.Hex("#2a3142")
)

func (a *App) transcriptDialog(c *ui.Context) {
	st := &a.sessionsUI.transcript
	if !st.open {
		return
	}
	t := c.Theme()
	ui.DialogBase(c, &st.open, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .6))
		turns := transcriptBlocks(st.parts)
		width := float32(760)
		if len(turns) > 1 {
			width += railWidth
		}
		panel.Width(width).Padding(0).Gap(0).Radius(10).Background(t.Surface).Border(1, t.Border).Clip().Label(a.t("Conversation", "对话"))
		tools := 0
		for _, p := range st.parts {
			if p.Kind == data.PartToolCall {
				tools++
			}
		}
		ui.Row(c).Gap(12).Padding(14, 18).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
				ui.Text(c, st.session.Title).FontSize(14).Bold().SingleLine().Ellipsis("…")
				meta := st.session.Agent.Label() + " · " + sessionModelText(st.session)
				if !st.loading && st.err == nil {
					meta += fmt.Sprintf(a.t(" · %d rounds · %d tool calls", " · %d 轮 · %d 次工具调用"), len(turns), tools)
				}
				ui.Text(c, meta).FontSize(12).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
			})
			if !st.loading && st.err == nil && tools > 0 {
				label := a.t("Expand steps", "展开全部过程")
				if st.allSteps {
					label = a.t("Collapse steps", "收起过程")
				}
				if sessionButton(c, label, st.allSteps).Clicked() {
					st.allSteps = !st.allSteps
					clear(st.expanded)
				}
			}
			if sessionButton(c, a.t("Close", "关闭"), false).Clicked() {
				st.open = false
			}
		})
		switch {
		case st.loading:
			ui.Row(c).Padding(40).Justify(ui.Center).Children(func() { ui.Spinner(c) })
			return
		case st.err == data.ErrNoTranscript:
			ui.Text(c, a.t("This session's conversation is not on this device.", "本机找不到这个会话的对话记录。")).Padding(24, 18).FontSize(12).TextColor(t.TextMuted)
			return
		case st.err != nil:
			ui.Text(c, st.err.Error()).Padding(24, 18).FontSize(12).TextColor(t.Danger)
			return
		}
		if st.query != st.seen {
			st.seen, st.matches, st.cur = st.query, findMatches(st.parts, st.query), 0
			if len(st.matches) > 0 {
				st.jump = jumpMatch
			}
		}
		a.transcriptSearch(c)
		if st.jump == jumpMatch {
			st.round = turnOf(turns, st.curPart())
			st.pinned, st.pinSeen, st.pinAt = true, false, c.Now()
		}
		ui.Row(c).Gap(0).AlignItems(ui.Stretch).Background(t.Background.Mix(t.Surface, .5)).Children(func() {
			if len(turns) > 1 {
				a.transcriptRail(c, turns)
			}
			sc := ui.Scroll(c.Key("transcript")).Grow(1).MinWidth(0).MaxHeight(560).Padding(8, 18, 16, 18).Gap(0)
			sc.Children(func() {
				if st.cut {
					ui.Text(c, a.t("Earlier messages are left out; showing the latest.", "较早的消息已省略，只显示最近部分。")).Padding(6, 0).FontSize(11).TextColor(t.TextMuted)
				}
				// Where each round was in the last frame tells which one
				// is at the top of the view now.
				view, reading := sc.Bounds(), -1
				for n, turn := range turns {
					col := a.transcriptTurn(c, n, turn)
					// Above the first round (the note on left-out messages)
					// still reads as the first round.
					if r := col.Bounds(); view.H > 0 && r.H > 0 && (n == 0 || r.Y <= view.Y+32) {
						reading = n
					}
					if st.pinned && n == st.round {
						if col.InView() {
							st.pinSeen = true
						} else if st.pinSeen || c.Now().Sub(st.pinAt) > pinFor {
							st.pinned = false
						}
					}
				}
				if !st.pinned && reading >= 0 && reading != st.round {
					// The rail is already drawn: show the change next frame.
					st.round, st.railMoved = reading, true
					c.AnimationFrame()
				}
				ui.Text(c, a.t("Read from this device's files only; nothing is kept or synced.", "只读取本机源文件，关闭后不保留，也不会同步。")).Padding(16, 0, 0).FontSize(11).TextColor(t.TextMuted)
			})
		})
		st.jump = jumpNone
	})
	if !st.open {
		*st = transcriptState{}
	}
}

const railWidth = 190

// pinFor is longer than a glide to a round or match takes.
const pinFor = time.Second

// turnOf returns the round holding part.
func turnOf(turns [][]transcriptBlock, part int) int {
	return sort.Search(len(turns), func(n int) bool { return turns[n][0].start > part }) - 1
}

// transcriptSearch is the bar under the header: the field, where the
// current match is, and steps to the others. Enter and Shift+Enter step
// too, and Cmd+F comes back to the field.
func (a *App) transcriptSearch(c *ui.Context) {
	t := c.Theme()
	st := &a.sessionsUI.transcript
	step := func(d int) {
		if n := len(st.matches); n > 0 {
			st.cur, st.jump = (st.cur+d+n)%n, jumpMatch
		}
	}
	ui.Row(c).Gap(8).Padding(8, 18).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		field := ui.SearchField(c, &st.query).Grow(1).Height(28).FontSize(12).Radius(6).Background(ui.Hex("#101014")).Border(1, t.Border).Placeholder(a.t("Search this conversation", "搜索对话内容")).Label(a.t("Search conversation", "搜索对话"))
		if c.Shortcut(ui.Cmd, ui.KeyF) {
			field.Focus()
		}
		if field.Submitted() {
			if c.Modifiers()&ui.Shift != 0 {
				step(-1)
			} else {
				step(1)
			}
		}
		if strings.TrimSpace(st.query) == "" {
			return
		}
		count, color := a.t("No matches", "无匹配"), t.TextMuted
		if n := len(st.matches); n > 0 {
			count, color = fmt.Sprintf("%d / %d", st.cur+1, n), t.Text
		}
		ui.Text(c, count).FontSize(12).TextColor(color).NoWrap().MinWidth(64).TextAlign(ui.End)
		if sessionButton(c, "↑", false).Label(a.t("Previous match", "上一处")).Width(30).Disabled(len(st.matches) == 0).Clicked() {
			step(-1)
		}
		if sessionButton(c, "↓", false).Label(a.t("Next match", "下一处")).Width(30).Disabled(len(st.matches) == 0).Clicked() {
			step(1)
		}
	})
}

// transcriptRail lists the rounds by what the user asked, marks the one
// last jumped to, and jumps to one when chosen. While searching, it counts
// each round's matches and dims the rounds without any.
func (a *App) transcriptRail(c *ui.Context, turns [][]transcriptBlock) {
	t := c.Theme()
	st := &a.sessionsUI.transcript
	searching := strings.TrimSpace(st.query) != ""
	counts := make([]int, len(turns))
	for _, m := range st.matches {
		counts[turnOf(turns, m.part)]++
	}
	ui.Scroll(c.Key("rail")).Width(railWidth).MaxHeight(560).Padding(10, 8).Gap(2).BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		ui.Text(c, a.t("Rounds", "轮次")).FontSize(11).TextColor(t.TextMuted).Padding(0, 8, 4)
		for n, turn := range turns {
			ask := a.t("(no message)", "（无提问）")
			if turn[0].kind == data.PartUser {
				ask = strings.Join(strings.Fields(turn[0].parts[0].Text), " ")
			}
			active := n == st.round
			item := ui.Row(c.Key("r/"+strconv.Itoa(n))).Gap(8).Height(30).Padding(0, 8).Radius(5).AlignItems(ui.Center).Focusable().Label(fmt.Sprintf(a.t("Go to round %d", "跳到第 %d 轮"), n+1))
			if item.Hovered() {
				item.Background(t.SurfaceHover)
			}
			if active {
				item.Background(t.Accent.Alpha(.12))
				if st.jump != jumpNone || st.railMoved {
					item.ScrollIntoView()
				}
			}
			if item.Clicked() {
				st.round, st.jump = n, jumpRound
				st.pinned, st.pinSeen, st.pinAt = true, false, c.Now()
				if searching {
					// Choosing a round moves to its first match, so the
					// count and the rail agree.
					if k := slices.IndexFunc(st.matches, func(m transcriptMatch) bool { return turnOf(turns, m.part) == n }); k >= 0 {
						st.cur = k
					}
				}
			}
			dim := searching && counts[n] == 0
			item.Children(func() {
				num := ui.Text(c, strconv.Itoa(n+1)).Font(monoFont).FontSize(11).Width(18).NoWrap().TextColor(t.TextMuted)
				txt := ui.Text(c, ask).Grow(1).MinWidth(0).FontSize(12).SingleLine().Ellipsis("…").TextColor(t.Text.Alpha(.85))
				switch {
				case active:
					num.TextColor(t.Accent)
					txt.TextColor(t.Text)
				case dim:
					num.TextColor(t.TextMuted.Alpha(.4))
					txt.TextColor(t.TextMuted.Alpha(.5))
				}
				if counts[n] > 0 {
					ui.Text(c, strconv.Itoa(counts[n])).FontSize(10).Padding(0, 5).Radius(7).Background(ui.Hex("#d9a441").Alpha(.25)).TextColor(ui.Hex("#f0c674")).NoWrap()
				}
			})
		}
		st.railMoved = false
	})
}

// replyMarkdown is how the agent's replies look, or its thinking, quieter:
// the panel's size and colors, headings no larger than a reply's emphasis,
// code wrapped to the panel, and only web links followed, since a reply's
// relative links name files in its workspace, not places in this app.
func (a *App) replyMarkdown(c *ui.Context, thinking bool) *markdown.Options {
	st := &a.sessionsUI.transcript
	o := &st.md
	if thinking {
		o = &st.thinkMD
	}
	if *o != nil {
		return *o
	}
	t := c.Theme()
	th := &markdown.Theme{FontSize: 13, LineHeight: 1.6, CodeFont: monoFont, CodeBackground: t.Text.Alpha(.08), CodeBlockBackground: ui.Hex("#0f1117"), CodeBlockBorder: t.Border, BlockSpacing: .6}
	if thinking {
		th.FontSize, th.LineHeight, th.BlockSpacing = 12, 1.5, .5
		th.Text, th.Strong, th.Code = t.TextMuted, t.TextMuted.Mix(t.Text, .35), t.TextMuted
	}
	for i, size := range []float32{1.25, 1.15, 1.05, 1, 1, 1} {
		th.Headings[i] = markdown.HeadingStyle{Size: size, Weight: 600, Top: .9, Bottom: .35}
	}
	*o = &markdown.Options{Theme: th, WrapCode: true, OnLink: func(url string) {
		if a.openURL != nil && (strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "mailto:")) {
			a.openURL(url)
		}
	}}
	return *o
}

// reveal scrolls the current match's part into view in the frame it
// becomes current.
func (a *App) reveal(e ui.Element, part int) {
	st := &a.sessionsUI.transcript
	if st.jump == jumpMatch && st.curPart() == part {
		e.ScrollIntoView(ui.ScrollIntoViewOptions{Block: ui.ScrollCenter, Animation: &ui.Smooth})
	}
}

func (a *App) transcriptTurn(c *ui.Context, n int, turn []transcriptBlock) ui.Element {
	t := c.Theme()
	st := &a.sessionsUI.transcript
	col := ui.Column(c.Key("turn/"+strconv.Itoa(n))).Gap(10).Padding(14, 0, 6)
	if st.jump == jumpRound && st.round == n {
		col.ScrollIntoView(ui.ScrollIntoViewOptions{Block: ui.ScrollStart, Animation: &ui.Smooth})
	}
	return col.Children(func() {
		if turn[0].kind == data.PartUser {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf(a.t("Round %d", "第 %d 轮"), n+1)).FontSize(11).TextColor(t.TextMuted).NoWrap()
				ui.Box(c).Grow(1).Height(1).Background(t.Border)
			})
		}
		for _, b := range turn {
			switch b.kind {
			case data.PartUser:
				ui.Row(c.Key("b/" + strconv.Itoa(b.start))).Justify(ui.End).Children(func() {
					bubble := ui.Box(c).MaxWidth(560).Padding(8, 12).Radius(10).Background(userBubble).Border(1, t.Accent.Alpha(.25))
					bubble.Children(func() {
						a.marked(c, b.parts[0].Text, b.start).FontSize(13).LineHeight(1.5).Selectable()
					})
					a.reveal(bubble, b.start)
				})
			case data.PartAssistant:
				ui.Column(c.Key("b/" + strconv.Itoa(b.start))).Gap(4).Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(6, 6).Radius(3).Background(t.Accent)
						ui.Text(c, a.sessionsUI.transcript.session.Agent.Label()).FontSize(11).TextColor(t.Accent)
					})
					ui.Column(c).Padding(0, 0, 0, 12).Children(func() {
						// A reply the search hits shows as its text, so each
						// hit is marked where it is; the rest read as Markdown.
						if _, hits := st.partMatches(b.start); hits > 0 {
							a.reveal(a.marked(c, b.parts[0].Text, b.start).FontSize(13).LineHeight(1.6).Selectable(), b.start)
						} else {
							markdown.View(c, b.parts[0].Text, a.replyMarkdown(c, false)).FillWidth()
						}
					})
				})
			default:
				a.transcriptSteps(c, b)
			}
		}
	})
}

// transcriptSteps draws a run of thinking and tool steps as one quiet line,
// with a guide down its left once opened.
func (a *App) transcriptSteps(c *ui.Context, b transcriptBlock) {
	t := c.Theme()
	st := &a.sessionsUI.transcript
	key := -1 - b.start // block keys are negative, part keys their index
	// The block holding the current match opens so it shows.
	cur := st.curPart()
	holds := cur >= b.start && cur < b.start+len(b.parts)
	open := st.allSteps != st.expanded[key] || holds
	hits := 0
	for j := range b.parts {
		_, n := st.partMatches(b.start + j)
		hits += n
	}
	var names []string
	calls, thinking := 0, false
	for _, p := range b.parts {
		switch p.Kind {
		case data.PartToolCall:
			calls++
			if len(names) < 4 && !slices.Contains(names, p.Name) {
				names = append(names, p.Name)
			}
		case data.PartThinking:
			thinking = true
		}
	}
	var summary []string
	if thinking {
		summary = append(summary, a.t("thinking", "思考"))
	}
	if calls > 0 {
		summary = append(summary, fmt.Sprintf(a.t("%d tool calls", "%d 次工具调用"), calls))
	}
	arrow := "▸"
	if open {
		arrow = "▾"
	}
	ui.Column(c.Key("b/"+strconv.Itoa(b.start))).Gap(6).Padding(0, 0, 0, 12).Children(func() {
		h := ui.Row(c).Gap(8).Height(24).Padding(0, 8).Radius(4).AlignItems(ui.Center).Label(a.t("Steps", "过程")).Focusable()
		if h.Hovered() {
			h.Background(t.SurfaceHover)
		}
		if h.Clicked() && !holds {
			st.expanded[key] = !st.expanded[key]
		}
		h.Children(func() {
			ui.Text(c, arrow).FontSize(10).TextColor(t.TextMuted)
			ui.Text(c, strings.Join(summary, " · ")).FontSize(12).TextColor(t.TextMuted).NoWrap()
			if len(names) > 0 {
				ui.Text(c, strings.Join(names, "  ")).Grow(1).MinWidth(0).Font(monoFont).FontSize(11).TextColor(t.TextMuted.Alpha(.75)).SingleLine().Ellipsis("…")
			} else {
				ui.Box(c).Grow(1)
			}
			if hits > 0 && !open {
				ui.Text(c, fmt.Sprintf(a.t("%d matches", "%d 处匹配"), hits)).FontSize(10).Padding(0, 6).Radius(7).Background(ui.Hex("#d9a441").Alpha(.25)).TextColor(ui.Hex("#f0c674")).NoWrap()
			}
		})
		if !open {
			return
		}
		ui.Column(c).Gap(8).Padding(4, 0, 4, 12).BorderWidth(0, 0, 0, 2).BorderColor(guideColor).Children(func() {
			for j := 0; j < len(b.parts); j++ {
				i, p := b.start+j, b.parts[j]
				if p.Kind != data.PartToolCall {
					a.transcriptStep(c, i, p)
					continue
				}
				// A call and the output that answers it share one card.
				var r *data.Part
				if j+1 < len(b.parts) && b.parts[j+1].Kind == data.PartToolResult {
					r = &b.parts[j+1]
					j++
				}
				a.transcriptTool(c, i, p, r)
			}
		})
	})
}

// stepLines is how many lines of a step show before it is opened.
const stepLines = 6

func (a *App) transcriptStep(c *ui.Context, i int, p data.Part) {
	t := c.Theme()
	st := &a.sessionsUI.transcript
	text := strings.TrimRight(p.Text, "\n")
	lines := strings.Split(text, "\n")
	full := st.expanded[i] || st.curPart() == i
	more := len(lines) > stepLines && !full
	if more {
		text = strings.Join(lines[:stepLines], "\n")
	}
	col := ui.Column(c.Key("p/" + strconv.Itoa(i))).Gap(3)
	a.reveal(col, i)
	col.Children(func() {
		switch p.Kind {
		case data.PartThinking:
			ui.Text(c, a.t("Thinking", "思考")).FontSize(11).TextColor(t.TextMuted)
			if _, hits := st.partMatches(i); hits > 0 {
				a.marked(c, text, i).FontSize(12).LineHeight(1.5).Italic().TextColor(t.TextMuted).Selectable()
			} else {
				markdown.View(c, text, a.replyMarkdown(c, true)).FillWidth()
			}
		default:
			ui.Box(c).Padding(6, 8).Radius(4).Background(ui.Hex("#0f1117")).Children(func() {
				a.marked(c, text, i).Font(monoFont).FontSize(11).LineHeight(1.45).TextColor(t.TextMuted).Selectable()
			})
		}
		if len(lines) > stepLines {
			label := fmt.Sprintf(a.t("Show all %d lines", "显示全部 %d 行"), len(lines))
			if full {
				label = a.t("Show less", "收起")
			}
			if textButton(c, label, false).Height(22).TextColor(t.Accent).Clicked() {
				st.expanded[i] = !full
			}
		}
	})
}

// clipLines returns where text[at:end] ends after n lines, and whether
// that leaves some out.
func clipLines(text string, at, end, n int) (int, bool) {
	for i := at; i < end; i++ {
		if text[i] == '\n' {
			if n--; n == 0 {
				return i, true
			}
		}
	}
	return end, false
}

// transcriptTool draws a tool call as a card: the tool's name, its
// arguments as name and value rows, and the output it got below a rule.
// Long values and output show their first lines until opened.
func (a *App) transcriptTool(c *ui.Context, i int, p data.Part, r *data.Part) {
	t := c.Theme()
	st := &a.sessionsUI.transcript
	cur := st.curPart()
	full := st.expanded[i] || cur == i || (r != nil && cur == i+1)
	clipped := false
	value := func(part int, text string, at, end int) ui.Element {
		if !full {
			var cut bool
			if end, cut = clipLines(text, at, end, stepLines); cut {
				clipped = true
			}
		}
		return a.markedRange(c, text, part, at, end).Grow(1).MinWidth(0).Font(monoFont).FontSize(11).LineHeight(1.5).Selectable()
	}
	card := ui.Column(c.Key("p/"+strconv.Itoa(i))).Gap(0).Radius(6).Border(1, t.Border).Background(ui.Hex("#12151c")).Clip()
	a.reveal(card, i)
	if r != nil {
		a.reveal(card, i+1)
	}
	card.Children(func() {
		ui.Row(c).Gap(8).Padding(5, 10).AlignItems(ui.Center).Background(t.Text.Alpha(.035)).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			ui.Box(c).Size(6, 6).Radius(3).Background(t.Accent.Alpha(.8))
			ui.Text(c, p.Name).Font(monoFont).FontSize(12).Bold().TextColor(t.Accent).NoWrap()
		})
		ui.Column(c).Gap(6).Padding(8, 10).Children(func() {
			if len(p.Args) == 0 {
				value(i, p.Text, 0, len(p.Text)).TextColor(t.Text.Alpha(.9))
				return
			}
			for _, arg := range p.Args {
				ui.Row(c).Gap(12).AlignItems(ui.Start).Children(func() {
					ui.Text(c, arg.Name).Width(84).Font(monoFont).FontSize(11).LineHeight(1.5).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
					value(i, p.Text, arg.At, arg.End).TextColor(t.Text.Alpha(.9))
				})
			}
		})
		if r != nil {
			out := strings.TrimRight(r.Text, "\n")
			ui.Column(c).Gap(4).Padding(8, 10).Background(ui.Hex("#0d0f14")).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
				ui.Text(c, a.t("Output", "输出")).FontSize(10).TextColor(t.TextMuted)
				if out == "" {
					ui.Text(c, a.t("(empty)", "（空）")).FontSize(11).TextColor(t.TextMuted.Alpha(.6))
				} else {
					value(i+1, out, 0, len(out)).TextColor(t.TextMuted)
				}
			})
		}
		if clipped || st.expanded[i] {
			label := a.t("Show all", "显示全部")
			if full {
				label = a.t("Show less", "收起")
			}
			ui.Row(c).Padding(0, 4).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
				if textButton(c, label, false).Height(24).TextColor(t.Accent).Clicked() {
					st.expanded[i] = !st.expanded[i]
				}
			})
		}
	})
}
