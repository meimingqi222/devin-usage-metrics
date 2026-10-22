package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// The overview leads with cost, explains each figure, and the chart switches
// between cost, all tokens and tokens without cache reads.
func TestUsageOverviewAndChartModes(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	// 197K in total, of which 140K are cache reads: 140K of 188K prompt tokens.
	for _, want := range []string{"Cost", "Total Tokens", "197.0K", "57.0K without cache reads", "74% hit rate", "2 turns per session", "Daily Cost"} {
		if !tt.HasText(want) {
			t.Fatalf("overview missing %q: %q", want, tt.Texts())
		}
	}
	for _, text := range tt.Texts() {
		if text == "·" {
			t.Fatal("inactive days still draw a placeholder")
		}
	}
	click(t, tt, "Tokens")
	// Today: local 112.0K plus the laptop's 29.0K.
	if !tt.HasText("Token Usage Trend") || !tt.HasText("141.0K") {
		t.Fatalf("token chart: %q", tt.Texts())
	}
	click(t, tt, "Without cache reads")
	// Without cache reads: today 141K - 100K, four days ago 56K - 40K. (The
	// details table below still lists 141.0K as today's total.)
	if !tt.HasText("41.0K") || !tt.HasText("16.0K") {
		t.Fatalf("chart without cache reads: %q", tt.Texts())
	}
	capture(t, tt, "go-usage-new-tokens")
}
