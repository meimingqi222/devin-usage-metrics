// Package credentialid names the account a provider credential document
// belongs to. It reads only identity claims, never a token.
package credentialid

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Identity is what a credential document says about its account. Stable is
// the provider's own account key, empty when the document carries none.
type Identity struct {
	Stable string
	Email  string
	Plan   string
}

// Of reads provider's identity claims from its credential document.
func Of(provider string, doc map[string]any) Identity {
	switch provider {
	case "claude":
		oauth := obj(doc["claudeAiOauth"])
		email := str(oauth["emailAddress"])
		if email == "" {
			email = str(doc["email"])
		}
		org := str(doc["organizationUuid"])
		account := str(oauth["accountUuid"])
		return Identity{Stable: join(email, org, account), Email: email, Plan: str(oauth["subscriptionType"])}
	case "codex":
		tokens := obj(doc["tokens"])
		claims := jwtClaims(str(tokens["id_token"]))
		auth := obj(claims["https://api.openai.com/auth"])
		email := str(claims["email"])
		if email == "" {
			email = str(obj(claims["https://api.openai.com/profile"])["email"])
		}
		if email == "" {
			email = str(doc["email"])
		}
		account := str(tokens["account_id"])
		if account == "" {
			account = str(auth["chatgpt_account_id"])
		}
		return Identity{Stable: join(email, account), Email: email, Plan: str(auth["chatgpt_plan_type"])}
	case "antigravity":
		token := doc
		if nested := obj(doc["token"]); nested != nil {
			token = nested
		}
		idToken := str(token["id_token"])
		if idToken == "" {
			// The macOS keychain value keeps id_token beside token{}.
			idToken = str(doc["id_token"])
		}
		claims := jwtClaims(idToken)
		email := str(claims["email"])
		if email == "" {
			// Windows Antigravity state and some keychain payloads keep the
			// account email beside the opaque tokens instead of an id_token.
			email = str(doc["id_token_email"])
		}
		if email == "" {
			email = str(doc["email"])
		}
		return Identity{Stable: join(email, str(claims["sub"])), Email: email}
	case "grok":
		keys := make([]string, 0, len(doc))
		for k := range doc {
			keys = append(keys, k)
		}
		for _, k := range keys {
			entry := obj(doc[k])
			if _, ok := entry["refresh_token"]; !ok {
				continue
			}
			email := str(entry["email"])
			if email == "" {
				email = str(entry["email_address"])
			}
			return Identity{Stable: join(email, str(entry["user_id"])), Email: email, Plan: str(entry["plan"])}
		}
	}
	return Identity{}
}

// Claims is the identity the agent or issuer actually wrote, without the
// fields this app stamps for naming. Of merges those stamped values back in
// (doc["email"], Antigravity's id_token_email row) because they are the only
// name some documents carry; comparing two documents' provenance needs the
// unmerged form - a stamped name cannot testify which account signed the
// tokens.
func Claims(provider string, doc map[string]any) Identity {
	switch provider {
	case "claude":
		oauth := obj(doc["claudeAiOauth"])
		email := str(oauth["emailAddress"])
		org := str(doc["organizationUuid"])
		account := str(oauth["accountUuid"])
		return Identity{Stable: join(email, org, account), Email: email, Plan: str(oauth["subscriptionType"])}
	case "codex":
		tokens := obj(doc["tokens"])
		claims := jwtClaims(str(tokens["id_token"]))
		auth := obj(claims["https://api.openai.com/auth"])
		email := str(claims["email"])
		if email == "" {
			email = str(obj(claims["https://api.openai.com/profile"])["email"])
		}
		account := str(tokens["account_id"])
		if account == "" {
			account = str(auth["chatgpt_account_id"])
		}
		return Identity{Stable: join(email, account), Email: email, Plan: str(auth["chatgpt_plan_type"])}
	case "antigravity":
		token := doc
		if nested := obj(doc["token"]); nested != nil {
			token = nested
		}
		idToken := str(token["id_token"])
		if idToken == "" {
			idToken = str(doc["id_token"])
		}
		claims := jwtClaims(idToken)
		return Identity{Stable: join(str(claims["email"]), str(claims["sub"])), Email: str(claims["email"])}
	case "grok":
		return Of(provider, doc)
	}
	return Identity{}
}

// Label is the account name a person reads: email first, then the stable key.
func Label(provider string, id Identity) string {
	if id.Email != "" {
		return id.Email
	}
	if id.Stable != "" {
		return id.Stable
	}
	switch provider {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	case "grok":
		return "Grok"
	}
	return provider
}

func join(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "|")
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
