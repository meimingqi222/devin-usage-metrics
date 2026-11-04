package credentialid

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "x." + base64.RawURLEncoding.EncodeToString(b) + ".y"
}

func TestClaudeIdentityIncludesOrganization(t *testing.T) {
	doc := map[string]any{
		"claudeAiOauth":    map[string]any{"emailAddress": "a@example.com", "subscriptionType": "max"},
		"organizationUuid": "org-1",
	}
	got := Of("claude", doc)
	if got.Stable != "a@example.com|org-1" || got.Email != "a@example.com" || got.Plan != "max" {
		t.Fatalf("claude identity: %+v", got)
	}
}

func TestCodexIdentityUsesWorkspace(t *testing.T) {
	doc := map[string]any{"tokens": map[string]any{
		"id_token": jwt(map[string]any{
			"email":                       "a@example.com",
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "pro"},
		}),
	}}
	got := Of("codex", doc)
	if got.Stable != "a@example.com|acct-1" || got.Plan != "pro" {
		t.Fatalf("codex identity: %+v", got)
	}
}

func TestAntigravityNestedToken(t *testing.T) {
	doc := map[string]any{"token": map[string]any{
		"id_token": jwt(map[string]any{"email": "g@example.com", "sub": "sub-1"}),
	}}
	got := Of("antigravity", doc)
	if got.Stable != "g@example.com|sub-1" || got.Email != "g@example.com" {
		t.Fatalf("antigravity identity: %+v", got)
	}
}

func TestAntigravityKeychainWrapperUsesTopLevelIDToken(t *testing.T) {
	doc := map[string]any{
		"token":       map[string]any{"access_token": "a", "refresh_token": "r"},
		"id_token":    jwt(map[string]any{"email": "g@example.com", "sub": "sub-1"}),
		"auth_method": "consumer",
	}
	got := Of("antigravity", doc)
	if got.Stable != "g@example.com|sub-1" || got.Email != "g@example.com" {
		t.Fatalf("antigravity keychain identity: %+v", got)
	}
}
