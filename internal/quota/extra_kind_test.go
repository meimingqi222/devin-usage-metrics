package quota

import (
	"encoding/json"
	"testing"
)

// TestExtraCarriesItsMeaning pins that each provider's extra figure says
// what it is. The desktop showed all three as "Extra: …": Claude's extra
// usage spent against its monthly limit, Devin's remaining overage balance,
// and the CLI's overage figure whose meaning is not confirmed.
func TestExtraCarriesItsMeaning(t *testing.T) {
	decode := func(s string) object {
		var v object
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	claude := ParseClaudeUsage(decode(`{"five_hour":{"utilization":1},"extra_usage":{"is_enabled":true,"used_credits":12.3,"monthly_limit":50}}`), "Max")
	if claude.Extra != "$12.30 / $50.00" || claude.ExtraKind != ExtraSpend {
		t.Fatalf("claude extra = %q %q", claude.Extra, claude.ExtraKind)
	}
	devin := ParseDevinQuota(decode(`{"daily_percentage":42,"overage_balance":3.2}`), "pro")
	if devin.Extra != "3.20 ACU" || devin.ExtraKind != ExtraBalance {
		t.Fatalf("devin extra = %q %q", devin.Extra, devin.ExtraKind)
	}
}
