package accounts

import (
	"errors"
	"strings"

	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

// DiagnosticErrorCode classifies switch failures without logging the email
// included in some user-facing errors, or underlying paths and URLs.
func DiagnosticErrorCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, ErrDisabled) {
		return "switching_disabled"
	}
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	switch err.Error() {
	case "account not found":
		return "profile_not_found"
	case "this account has no saved sign-in; sign in again", "saved sign-in is unreadable":
		return "saved_sign_in_unavailable"
	case "the saved sign-in belongs to a different account; sign in again":
		return "saved_identity_mismatch"
	case "this saved copy holds the current account's sign-in; sign in again to refresh it":
		return "saved_copy_conflict"
	case "the live sign-in changed while switching; try again":
		return "live_changed_during_switch"
	case "the sign-in did not read back as the selected account":
		return "switch_readback_mismatch"
	case "Antigravity went back to the previous sign-in; quit Antigravity fully and switch again":
		return "previous_sign_in_restored"
	case "Antigravity did not exit":
		return "client_did_not_exit"
	case "the agent's sign-in store is read-only here":
		return "live_store_readonly"
	}
	if strings.HasPrefix(err.Error(), "this saved copy holds ") && strings.HasSuffix(err.Error(), "'s sign-in; sign in again to refresh it") {
		return "saved_copy_conflict"
	}
	return syncdiag.ErrorCode(err)
}
