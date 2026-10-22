package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// The overview's four cards explain their figures, and the chart switches
// between all tokens, tokens without cache reads, and cost.
func TestUsageOverviewAndChartModes(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	// 197K in total, of which 140K are cache reads: 140K of 188K prompt
	// tokens. Model I/O is 45K new input plus 9K output.
	for _, want := range []string{"Period: 10-08 ~ 10-22 (last 15 days)", "API list pricing", "Total cost", "197.0", "K Tokens", "2 sessions · 3 turns", "74.5%", "140.0K", "54.0", "45.0K", "9.0K", "Token Usage Trend", "Daily avg 13.1K Tokens"} {
		if !tt.HasText(want) {
			t.Fatalf("overview missing %q: %q", want, tt.Texts())
		}
	}
	// Cache reads are billed below new input, so the badge reports a saving.
	if !hasPrefix(tt, "Saved ~") {
		t.Fatalf("no cache savings badge: %q", tt.Texts())
	}
	for _, text := range tt.Texts() {
		if text == "·" {
			t.Fatal("inactive days still draw a placeholder")
		}
	}
	// Today: local 112.0K plus the laptop's 29.0K.
	if !tt.HasText("141.0K") || !tt.HasText("56.0K") {
		t.Fatalf("token chart: %q", tt.Texts())
	}
	capture(t, tt, "go-usage-tokens")
	click(t, tt, "Without cache reads")
	// Without cache reads: today 141K - 100K, four days ago 56K - 40K. (The
	// details table below still lists 141.0K as today's total.)
	if !tt.HasText("41.0K") || !tt.HasText("16.0K") {
		t.Fatalf("chart without cache reads: %q", tt.Texts())
	}
	click(t, tt, "Cost")
	if !tt.HasText("Cost Trend") || !tt.HasText("$0.26") {
		t.Fatalf("cost chart: %q", tt.Texts())
	}
}

func hasPrefix(tt *ui.Tester, prefix string) bool {
	for _, text := range tt.Texts() {
		if len(text) >= len(prefix) && text[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func TestNiceAxisSteps(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, 7: 10, 41: 50, 75: 100, 140: 200, 2.3e7: 2.5e7, 8.1e7: 1e8} {
		if got := niceStep(in); got != want {
			t.Errorf("niceStep(%v) = %v, want %v", in, got, want)
		}
	}
	if got := axisLabel(5e7, false); got != "50M" {
		t.Errorf("axis label %q", got)
	}
	if got := grouped(3599); got != "3,599" {
		t.Errorf("grouped %q", got)
	}
}
