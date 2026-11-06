package desktop

import (
	"errors"
	"os"

	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

var errSavedSignInUnavailable = errors.New("saved sign-in unavailable; sign in again")

// quotaRoster uses the same profile IDs and names as the manager. Only the
// reconciled active ID joins a live source; email and token similarity must
// not hide another saved profile or transfer the active badge to it.
func quotaRoster(store *accounts.Store, profiles []accounts.Profile, live []quota.Account) []accountResult {
	var out []accountResult
	bound := map[string]bool{}
	for _, acc := range live {
		ar := accountResult{account: acc, isLive: true}
		for _, p := range profiles {
			if p.Provider == acc.Provider && p.Active && !p.Hidden && !bound[p.ID] {
				ar.profileID = p.ID
				ar.account.Label = profileName(profiles, p.Provider, p)
				bound[p.ID] = true
				break
			}
		}
		out = append(out, ar)
	}
	for _, p := range profiles {
		if p.Hidden || bound[p.ID] {
			continue
		}
		path := store.CredentialPath(p.Provider, p.ID)
		ar := accountResult{
			account:   quota.ProfileAccount("profile-"+p.ID, p.Provider, profileName(profiles, p.Provider, p), path),
			profileID: p.ID,
		}
		if _, err := os.Stat(path); err != nil {
			ar.err = errSavedSignInUnavailable
		} else if !p.Writable && p.Reason != "" {
			ar.err = errors.New(p.Reason)
		}
		out = append(out, ar)
	}
	return out
}

func cachedQuotaRoster(targets []accountResult, existing map[string]accountResult) []accountResult {
	out := append([]accountResult(nil), targets...)
	for i, ar := range out {
		// A live source key survives switches. Its old quota belongs to the
		// previous profile, so only reuse results with the same profile ID.
		if prev, ok := existing[ar.account.Key]; ok && prev.profileID == ar.profileID && ar.err == nil {
			out[i].result, out[i].err = prev.result, prev.err
		}
	}
	return out
}

// updateQuotaRoster runs on the UI thread, before any network refresh. A
// pending worker must not put its older profile list back after an edit.
func (a *App) updateQuotaRoster() {
	a.quotaUI.rosterRevision++
	var live []quota.Account
	existing := make(map[string]accountResult, len(a.accounts))
	for _, ar := range a.accounts {
		existing[ar.account.Key] = ar
		if ar.isLive {
			live = append(live, ar.account)
		}
	}
	a.accounts = cachedQuotaRoster(quotaRoster(a.accountStore, a.profiles, live), existing)
	ctx := accountDiagnosticContext(a.ctx, "account_roster_update")
	syncdiag.Log(ctx, "account_roster_updated", "revision", a.quotaUI.rosterRevision)
	logAccountRoster(ctx, a.profiles, a.accounts)
}

func (a *App) canSwitchProfile(provider, id string) bool {
	if !a.accountStore.Enabled() {
		return false
	}
	for _, p := range a.profiles {
		if p.Provider == provider && p.ID == id {
			return p.Writable && !p.Hidden
		}
	}
	return false
}
