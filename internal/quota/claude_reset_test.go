package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const fixtureOrg = "11111111-2222-3333-4444-555555555555"

func cedar(t *testing.T, s string) object {
	t.Helper()
	var v object
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestClaudeResetGrantsParse(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC).Unix()
	for _, c := range []struct {
		name, block    string
		ok             bool
		left, total    int
		grant, blocker string
		ends           string
		clearsFiveHour bool
	}{
		{"usable at limit, recommended grant wins", `{"eligible":true,"at_limit":true,"next_grant_id":"b","grants":[{"id":"a","resets_total":1,"resets_left":1,"usable_now":true,"ends_at":"2026-11-20T00:00:00Z"},{"id":"b","resets_total":2,"resets_left":1,"usable_now":true,"clears":["five_hour","bogus"],"ends_at":"2026-11-15T00:00:00Z"}]}`, true, 2, 3, "b", "", "2026-11-15T00:00:00Z", true},
		{"not at limit", `{"eligible":true,"at_limit":false,"grants":[{"id":"a","resets_total":2,"resets_left":2,"usable_now":true}]}`, true, 2, 2, "a", "not_limited", "", false},
		{"limit not required", `{"eligible":true,"grants":[{"id":"a","resets_total":1,"resets_left":1,"usable_now":true,"use_requires_limit":false}]}`, true, 1, 1, "a", "", "", false},
		{"cooldown", `{"eligible":true,"at_limit":true,"cooldown_until":"2026-10-22T13:00:00Z","grants":[{"id":"a","resets_total":1,"resets_left":1,"usable_now":true}]}`, true, 1, 1, "a", "cooldown", "", false},
		{"missing usable flag refuses", `{"eligible":true,"at_limit":true,"grants":[{"id":"a","resets_total":1,"resets_left":1}]}`, true, 1, 1, "a", "not_usable", "", false},
		{"exhausted", `{"eligible":true,"at_limit":true,"grants":[{"id":"a","resets_total":2,"resets_left":0,"usable_now":true}]}`, true, 0, 2, "", "exhausted", "", false},
		{"ineligible without grants", `{"eligible":false,"ineligible_reason":"tier"}`, false, 0, 0, "", "", "", false},
		{"malformed count rejects block", `{"eligible":true,"grants":[{"id":"a","resets_total":1,"resets_left":3}]}`, false, 0, 0, "", "", "", false},
		{"bad id rejects block", `{"eligible":true,"grants":[{"id":"A B","resets_total":1,"resets_left":1}]}`, false, 0, 0, "", "", "", false},
		{"duplicate id rejects block", `{"eligible":true,"grants":[{"id":"a","resets_total":1,"resets_left":1},{"id":"a","resets_total":1,"resets_left":1}]}`, false, 0, 0, "", "", "", false},
		{"absent", `{}`, false, 0, 0, "", "", "", false},
	} {
		r, ok := parseClaudeReset(cedar(t, c.block), now)
		if ok != c.ok || r.Left != c.left || r.Total != c.total || r.GrantID != c.grant || r.Blocker != c.blocker {
			t.Errorf("%s: got %+v ok=%v", c.name, r, ok)
		}
		if c.ends != "" && r.EndsAt != when(c.ends) {
			t.Errorf("%s: ends %d", c.name, r.EndsAt)
		}
		if c.clearsFiveHour && (len(r.Clears) != 1 || r.Clears[0] != "five_hour") {
			t.Errorf("%s: clears %v", c.name, r.Clears)
		}
	}
}

// Grants come from their own cedar_ember read, at most hourly per login:
// polling it drew hour-long 429s on Claude's usage endpoint. A failed read
// keeps the last status and still counts as the hour's check.
func TestClaudeGrantsReadAtMostHourly(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"tok"}}`)
	a := Account{Provider: "claude", source: homePath(".claude/.credentials.json")}
	usage, grants := 0, 0
	grantStatus := 200
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/oauth/usage" && r.URL.Query().Get("cedar_ember") == "":
			usage++
			fmt.Fprint(w, `{"five_hour":{"utilization":100}}`)
		case r.URL.Path == "/api/oauth/usage":
			grants++
			if grantStatus != 200 {
				w.WriteHeader(grantStatus)
				return
			}
			fmt.Fprint(w, `{"cedar_ember":{"eligible":true,"at_limit":true,"grants":[{"id":"g","resets_total":3,"resets_left":2,"usable_now":true}]}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	get := func() *ClaudeReset {
		t.Helper()
		r, err := fetch(context.Background(), a)
		if err != nil && status(err) != 429 {
			t.Fatal(err)
		}
		return r.Reset
	}
	if r := get(); r == nil || r.Left != 2 || usage != 1 || grants != 1 {
		t.Fatalf("first refresh: %+v usage=%d grants=%d", r, usage, grants)
	}
	if r := get(); r == nil || r.Left != 2 || usage != 2 || grants != 1 {
		t.Fatalf("grants re-read within the hour: usage=%d grants=%d", usage, grants)
	}
	// An hour on, a 429 keeps the last status and resets the hour.
	claudeGrants.Lock()
	check := claudeGrants.m[a.source]
	check.at = check.at.Add(-claudeGrantsInterval)
	claudeGrants.m[a.source] = check
	claudeGrants.Unlock()
	grantStatus = 429
	if r := get(); r == nil || r.Left != 2 || grants != 2 {
		t.Fatalf("failed read lost the status: %+v grants=%d", r, grants)
	}
	if get(); grants != 2 {
		t.Fatal("a failed read was retried on the next refresh")
	}
	// A claim makes the next refresh read again.
	forgetClaudeResetGrants(a.source)
	grantStatus = 200
	if get(); grants != 3 {
		t.Fatal("grants not re-read after a claim")
	}
}

func claudeResetServer(t *testing.T, status string, claim func(w http.ResponseWriter, body map[string]string)) *int {
	t.Helper()
	claims := 0
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("missing token")
		}
		switch {
		case r.URL.Path == "/api/oauth/profile":
			fmt.Fprintf(w, `{"organization":{"uuid":%q}}`, fixtureOrg)
		case r.URL.Path == "/api/oauth/usage":
			if r.URL.Query().Get("skip_spend") != "1" || r.URL.Query().Get("cedar_ember") != "1" {
				t.Error("status read may spend")
			}
			fmt.Fprintf(w, `{"cedar_ember":%s}`, status)
		case r.Method == "POST" && r.URL.Path == "/api/organizations/"+fixtureOrg+"/reset_rate_limits":
			claims++
			b, _ := io.ReadAll(r.Body)
			var body map[string]string
			_ = json.Unmarshal(b, &body)
			claim(w, body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
	return &claims
}

func TestClaimClaudeReset(t *testing.T) {
	usable := `{"eligible":true,"at_limit":true,"grants":[{"id":"g1","resets_total":2,"resets_left":1,"usable_now":true}]}`
	account := func(t *testing.T) Account {
		fixtureHome(t)
		fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"tok"}}`)
		return Account{Provider: "claude", source: homePath(".claude/.credentials.json")}
	}
	t.Run("spends the selected grant once", func(t *testing.T) {
		a := account(t)
		claims := claudeResetServer(t, usable, func(w http.ResponseWriter, body map[string]string) {
			if body["program"] != "cedar_ember" || body["grant_id"] != "g1" || len(body["request_id"]) != 32 {
				t.Errorf("claim body %v", body)
			}
			fmt.Fprint(w, `{"result":"reset"}`)
		})
		if got, err := ClaimClaudeReset(context.Background(), a); err != nil || got != "reset" || *claims != 1 {
			t.Fatalf("%q %v claims=%d", got, err, *claims)
		}
	})
	t.Run("refuses without spending when blocked", func(t *testing.T) {
		a := account(t)
		claims := claudeResetServer(t, strings.Replace(usable, `"at_limit":true`, `"at_limit":false`, 1), func(w http.ResponseWriter, _ map[string]string) {
			fmt.Fprint(w, `{"result":"reset"}`)
		})
		if got, err := ClaimClaudeReset(context.Background(), a); err != nil || got != "not_limited" || *claims != 0 {
			t.Fatalf("%q %v claims=%d", got, err, *claims)
		}
	})
	t.Run("an unreadable answer is unknown, not retried", func(t *testing.T) {
		a := account(t)
		claims := claudeResetServer(t, usable, func(w http.ResponseWriter, _ map[string]string) {
			w.WriteHeader(502)
		})
		if _, err := ClaimClaudeReset(context.Background(), a); !errors.Is(err, ErrClaudeResetUnknown) || *claims != 1 {
			t.Fatalf("%v claims=%d", err, *claims)
		}
	})
}
