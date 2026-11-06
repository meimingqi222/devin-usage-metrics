package desktop

import (
	"context"
	"errors"
	"strings"

	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

func accountDiagnosticContext(ctx context.Context, operation string) context.Context {
	ctx = syncdiag.WithFields(syncdiag.WithRun(ctx), "operation", operation, "account_diagnostics_version", 1)
	return syncdiag.WithFields(ctx, syncdiag.BuildFields()...)
}

// Empty identifiers remain empty; names use a normalized fingerprint so the
// log can explain duplicate labels without recording emails or user renames.
func accountRef(value string) string {
	if value == "" {
		return ""
	}
	return syncdiag.ID(value)
}

func accountNameRef(value string) string {
	return accountRef(strings.ToLower(strings.TrimSpace(value)))
}

func quotaDiagnosticError(err error) string {
	if errors.Is(err, errSavedSignInUnavailable) {
		return "saved_sign_in_unavailable"
	}
	return quota.DiagnosticErrorCode(err)
}

// Record decisions at roster construction, never once per rendered frame.
// Details are capped; totals and anomaly counts still cover the entire roster.
func logAccountRoster(ctx context.Context, profiles []accounts.Profile, roster []accountResult) {
	const maxDetails = 64
	visible, active, hidden := 0, 0, 0
	for i, p := range profiles {
		if p.Hidden {
			hidden++
		} else {
			visible++
		}
		if p.Active {
			active++
		}
		if i < maxDetails {
			syncdiag.Log(ctx, "account_profile", "provider", p.Provider, "profile", accountRef(p.ID),
				"email_ref", accountNameRef(p.Email), "label_ref", accountNameRef(p.DisplayName()), "stable_ref", accountRef(p.StableID),
				"active", p.Active, "hidden", p.Hidden, "writable", p.Writable, "has_restriction", p.Reason != "")
		}
	}
	labels, bound := map[string]int{}, map[string]bool{}
	live, unbound, unavailable, duplicateLabels := 0, 0, 0, 0
	for i, ar := range roster {
		binding := "saved_profile"
		if ar.isLive {
			live++
			binding = "active_profile"
			if ar.profileID == "" {
				unbound++
				binding = "unmatched_live"
			}
		}
		if ar.profileID != "" {
			bound[ar.account.Provider+"/"+ar.profileID] = true
		}
		if ar.err != nil {
			unavailable++
		}
		labelRef := accountNameRef(ar.account.Label)
		key := ar.account.Provider + "/" + labelRef
		if labelRef != "" {
			labels[key]++
			if labels[key] == 2 {
				duplicateLabels++
			}
		}
		if i < maxDetails {
			syncdiag.Log(ctx, "quota_target", "index", i, "provider", ar.account.Provider,
				"source", accountRef(ar.account.Key), "source_kind", ar.account.CredentialKind(),
				"profile", accountRef(ar.profileID), "label_ref", labelRef, "binding", binding,
				"is_live", ar.isLive, "error", quotaDiagnosticError(ar.err))
		}
	}
	missing := 0
	for _, p := range profiles {
		if !p.Hidden && !bound[p.Provider+"/"+p.ID] {
			missing++
		}
	}
	syncdiag.Log(ctx, "quota_roster", "saved", len(profiles), "visible_saved", visible, "hidden", hidden,
		"active_saved", active, "targets", len(roster), "live", live, "unmatched_live", unbound,
		"unavailable", unavailable, "duplicate_labels", duplicateLabels, "missing_profiles", missing,
		"profiles_truncated", max(0, len(profiles)-maxDetails), "targets_truncated", max(0, len(roster)-maxDetails))
	if duplicateLabels > 0 || missing > 0 {
		syncdiag.Log(ctx, "quota_roster_anomaly", "duplicate_labels", duplicateLabels, "missing_profiles", missing, "unmatched_live", unbound)
	}
}

func (a *App) recordAccountDiagnostics() {
	ctx := accountDiagnosticContext(a.ctx, "account_snapshot")
	syncdiag.Log(ctx, "account_snapshot", "revision", a.quotaUI.rosterRevision, "quota_busy", a.busy["quota"],
		"sync_busy", a.busy["account-sync"], "refresh_pending", a.quotaUI.refreshPending,
		"switching_providers", len(a.accountUI.switching))
	logAccountRoster(ctx, a.profiles, a.accounts)
}
