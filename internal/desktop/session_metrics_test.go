package desktop

import (
	"fmt"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// The session detail shows the cache hit rate over the session's prompt,
// the speed of the timed requests (the burst is left out), the request
// table, and the conversation dialog.
func TestSessionDetailMetricsAndConversation(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	a.agent = model.Devin
	now := time.Now()
	s := model.Session{Agent: model.Devin, Key: "cli/s1", ID: "sudden-cheshire", Source: "cli", AgentMode: "smart", Title: "Devin CLI 历史版本缓存清理分析", WorkingDirectory: "/projects/quick-cleaner", SelectedModel: "swe-2", DeviceID: a.localID, CreatedAt: now.Add(-time.Hour).Unix(), LastActivityAt: now.Unix()}
	rows := [][6]float64{{3200, 0, 15200, 410, 1900, 7600}, {900, 16800, 0, 520, 780, 8180}, {1100, 17900, 0, 1310, 840, 19740}, {2400, 18800, 0, 260, 2600, 2630}, {600, 21400, 0, 880, 760, 13360}, {450, 22100, 0, 2050, 910, 30310}}
	var turns []model.Turn
	for i, r := range rows {
		turns = append(turns, model.Turn{Agent: model.Devin, SessionKey: s.Key, DeviceID: a.localID, Model: "swe-2", CreatedAt: now.Add(time.Duration(i-6) * time.Minute).Unix(), InputTokens: r[0], CacheReadTokens: r[1], CacheCreationTokens: r[2], OutputTokens: r[3], TTFTMs: r[4], TotalTimeMs: r[5]})
		s.InputTokens += r[0]
		s.CachedTokens += r[1]
		s.CacheCreationTokens += r[2]
		s.OutputTokens += r[3]
		s.AgentMessages++
	}
	a.storeLoaded(model.Devin, model.Data{Sessions: []model.Session{s}, Turns: turns})
	tt := ui.NewTester(a.View, 1240, 860)
	tt.SetScale(2)
	click(t, tt, "会话")
	click(t, tt, s.Title)
	click(t, tt, "展开")
	tt.Frame()
	for _, want := range []string{"80.3%", "70 tok/s", "· 5/6 条有可用计时 · 不含等待首 token", "98.0%", "72 tok/s", "显示对话"} {
		if !tt.HasText(want) {
			t.Fatalf("detail missing %q: %v", want, tt.Texts())
		}
	}
	capture(t, tt, "session-metrics")
	reply := "共 3 个版本，约 **3.8 GB**：\n\n| 版本 | 大小 |\n|---|---|\n| `3000.11.234` | 1.2G |\n\n`current` 指向 3000.14.30821，另外两个可以删除。"
	cmd := "du -sh ~/.local/share/devin/cli/_versions/*\nprintf '检查完成\\n'"
	args := "command:\n" + cmd + "\nworkdir: /projects/quick-cleaner"
	parts := []data.Part{
		{Kind: data.PartUser, Text: "清理一下 Devin CLI 的历史版本缓存，先看看占了多少空间"},
		{Kind: data.PartThinking, Text: "需要先列出 _versions 目录。"},
		{Kind: data.PartToolCall, Name: "exec", Text: args, Args: []data.Arg{{Name: "command", At: 9, End: 9 + len(cmd)}, {Name: "workdir", At: len(args) - len("/projects/quick-cleaner"), End: len(args)}}},
		{Kind: data.PartToolResult, Text: "1.2G  3000.11.234\n1.3G  3000.13.4\n1.3G  3000.14.30821"},
		{Kind: data.PartAssistant, Text: reply},
		{Kind: data.PartUser, Text: "删掉旧的两个"},
		{Kind: data.PartAssistant, Text: "已删除，剩余 1.3 GB。"}}
	for k := 3; k <= 12; k++ {
		parts = append(parts,
			data.Part{Kind: data.PartUser, Text: fmt.Sprintf("问题 %d：看看 Cache 目录还有多大", k)},
			data.Part{Kind: data.PartAssistant, Text: fmt.Sprintf("cache 目录第 %d 次检查：\n剩余 %d MB。\n没有别的变化。\n可以继续。", k, 100+k)})
	}
	a.sessionsUI.transcript = transcriptState{open: true, session: s, expanded: map[int]bool{}, round: -1, cut: true, parts: parts}
	tt.Frame()
	tt.Frame()
	for _, want := range []string{"第 1 轮", "第 2 轮", "思考 · 1 次工具调用", "轮次"} {
		if !tt.HasText(want) {
			t.Fatalf("conversation missing %q: %v", want, tt.Texts())
		}
	}
	if tt.HasText("1.3G  3000.13.4") {
		t.Fatal("tool output shown before its steps were opened")
	}
	// Replies read as Markdown: no syntax shows, and the table's cells do.
	if tt.HasText("**") || tt.HasText("|---|") || !tt.HasText("3.8 GB") || !tt.HasText("3000.11.234") {
		t.Fatalf("reply not rendered as Markdown: %v", tt.Texts())
	}
	visible := func(s string) bool {
		r, ok := tt.Find(s)
		return ok && r.Y > 0 && r.Y+r.H < 860
	}
	if visible("第 10 轮") {
		t.Fatal("round 10 already in view; the fixture does not scroll")
	}
	// The rail jumps to a round.
	click(t, tt, "问题 10：看看 Cache 目录还有多大")
	tt.Advance(time.Second)
	tt.Frame()
	if !visible("第 10 轮") {
		t.Fatal("choosing round 10 in the rail did not scroll to it")
	}
	// The last round cannot reach the top of the view, yet the rail keeps
	// it marked; scrolling by hand then marks the round at the top.
	st := &a.sessionsUI.transcript
	click(t, tt, "问题 12：看看 Cache 目录还有多大")
	tt.Advance(time.Second)
	tt.Frame()
	tt.Frame()
	if st.round != 11 {
		t.Fatalf("rail marks round %d after choosing 12", st.round+1)
	}
	tt.Scroll(700, 500, 0, -5000)
	tt.Frame()
	tt.Frame()
	if st.round != 0 {
		t.Fatalf("rail marks round %d at the top", st.round+1)
	}
	tt.Scroll(700, 500, 0, 700)
	tt.Frame()
	tt.Frame()
	r, _ := tt.Find(fmt.Sprintf("第 %d 轮", st.round+1))
	next, _ := tt.Find(fmt.Sprintf("第 %d 轮", st.round+2))
	if st.round == 0 || r.Y > 300 || next.Y < 300 {
		t.Fatalf("rail marks round %d; its divider at %v, the next at %v", st.round+1, r.Y, next.Y)
	}
	capture(t, tt, "session-scrollspy")
	// Search counts matches case-insensitively, steps through them with
	// Enter and Shift+Enter, and opens the folded steps holding one.
	tt.Key(ui.Cmd, ui.KeyF)
	tt.Type("cache")
	tt.Advance(time.Second)
	tt.Frame()
	if !tt.HasText("1 / 20") || !visible("第 3 轮") {
		t.Fatalf("search for cache: %v", tt.Texts())
	}
	tt.Key(0, ui.KeyEnter)
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	if !tt.HasText("2 / 20") {
		t.Fatalf("Enter twice and Shift+Enter should land on match 2: %v", tt.Texts())
	}
	capture(t, tt, "session-search")
	// Scrolling away while the view glides to a far match stops the glide;
	// the match's round, never shown, must not keep the rail's mark.
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Scroll(700, 500, 0, -1e5)
	tt.Frame()
	tt.Advance(2 * time.Second)
	tt.Scroll(700, 500, 0, 5)
	tt.Frame()
	tt.Frame()
	if !tt.HasText("20 / 20") || st.round != 0 {
		t.Fatalf("rail marks round %d after scrolling to the top: %v", st.round+1, tt.Texts())
	}
	for range 5 {
		tt.Key(0, ui.KeyBackspace)
	}
	// A reply the search hits shows its text, so the hit is marked in it.
	tt.Type("GB")
	tt.Frame()
	if !tt.HasText(reply) {
		t.Fatalf("searched reply should show its text: %v", tt.Texts())
	}
	tt.Key(0, ui.KeyBackspace)
	tt.Key(0, ui.KeyBackspace)
	tt.Type("检查完成")
	tt.Advance(time.Second)
	tt.Frame()
	if !tt.HasText("1 / 1") || !tt.HasText(cmd) || !tt.HasText("workdir") || !tt.HasText("输出") {
		t.Fatalf("search should open the formatted tool arguments: %v", tt.Texts())
	}
	capture(t, tt, "session-tool-args-search")
	for range 4 {
		tt.Key(0, ui.KeyBackspace)
	}
	tt.Type("3000.13.4")
	tt.Frame()
	if !tt.HasText("1 / 1") || !tt.HasText("3000.13.4\n1.3G") {
		t.Fatalf("a match in folded steps should open them: %v", tt.Texts())
	}
	for range 9 {
		tt.Key(0, ui.KeyBackspace)
	}
	tt.Frame()
	if tt.HasText("1.3G  3000.13.4") {
		t.Fatal("steps stayed open after the search was cleared")
	}
	click(t, tt, "思考 · 1 次工具调用")
	tt.Frame()
	if !tt.HasText("1.2G  3000.11.234\n1.3G  3000.13.4\n1.3G  3000.14.30821") {
		t.Fatalf("steps did not open: %v", tt.Texts())
	}
	capture(t, tt, "session-tool-args")
	click(t, tt, "关闭")
	tt.Frame()
	if a.sessionsUI.transcript.open {
		t.Fatal("dialog stayed open")
	}
}
