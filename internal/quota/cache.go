package quota

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Quota caches are credential-scoped. Only hashes, quota values and timestamps
// reach disk, never OAuth tokens or upstream error bodies. quotaKey binds a
// cache to the account's stable identity, not its path: switching accounts in
// one live file must not inherit the previous one's cached values or cooldown.
// Providers without identity claims fall back to their tokens in
// credentialIdentity.
func quotaKey(a Account) string {
	identity := credentialIdentity(a)
	fingerprint := sha256.Sum256([]byte(a.source + "\x00" + identity))
	return fmt.Sprintf("%s:%x", a.Key, fingerprint)
}

func quotaCachePath(key string) string {
	return configPath(fmt.Sprintf("quota-cache/%x.json", sha256.Sum256([]byte(key))))
}

type quotaDiskCache struct {
	Version        int
	Result         Result
	At, Until      time.Time
	Failures       int
	GrantsAt       time.Time
	GrantsIdentity string
	Grants         *ClaudeReset
}

func restoreQuotaCache(a Account, key string, s *entry) {
	b, err := os.ReadFile(quotaCachePath(key))
	var c quotaDiskCache
	if err != nil || json.Unmarshal(b, &c) != nil || c.Version != 1 {
		return
	}
	s.result, s.at, s.until, s.failures = c.Result, c.At, c.Until, max(0, min(c.Failures, 5))
	claudeGrants.Lock()
	claudeGrants.m[a.source] = claudeGrantsCheck{at: c.GrantsAt, reset: c.Grants, identity: c.GrantsIdentity}
	claudeGrants.Unlock()
}

func saveQuotaCache(a Account, key string, s *entry) {
	if quotaKey(a) != key {
		return
	}
	claudeGrants.Lock()
	g := claudeGrants.m[a.source]
	claudeGrants.Unlock()
	c := quotaDiskCache{Version: 1, Result: s.result, At: s.at, Until: s.until, Failures: s.failures,
		GrantsAt: g.at, GrantsIdentity: g.identity, Grants: g.reset}
	// Cache writes are best effort: an unwritable config directory must not
	// turn a successful quota query into a failure.
	_ = atomicWrite(quotaCachePath(key), c)
}
