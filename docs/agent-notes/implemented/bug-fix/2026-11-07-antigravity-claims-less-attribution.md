# Agent Note: Antigravity claims-less logins are named by the issuer, not recorded names

Status: implemented

## Problem

Antigravity's Windows state database returns bare tokens: no id_token, no
email, only the auth-status row names the account - and that row is UI
state that runs ahead of or behind the token row while a switch settles.
Naming these tokens from a recorded name stamped the wrong account onto
them, which rewrote a saved copy with another login's tokens (a "crossed
copy"), and a later switch restored that foreign login while the UI
insisted the selected account was active. Observed on a real machine: the
copy labelled agiaomei held meimingqi33's refresh token, every
subsequent naming pass re-attributed the live login to it, and the roster
rendered the same address twice.

Two more gaps surfaced: records of one Google account existed as
`email|sub` on one device and `email` on another, so repair could not
fold them and sync re-imported the second record every cycle; and a live
document matching a sync tombstone skipped all matching, so the live tab
went unbound and contaminated copies were never flagged.

## Decision

In `internal/accounts`:

- For a claims-less Antigravity document the issuer decides: the access
  token's userinfo answer, then a refresh-token exchange when the access
  token has expired (`googleRefreshedEmail`). Exchanged tokens are
  discarded; naming must not mint credentials. Recorded names (active
  profile, then any sharing copy) stay a fallback below the issuer, and
  the agent's auth-status label is never used.
- A claims-less document may still merge by token into any sharing
  profile (rotation capture); a document carrying claims must not
  overwrite a profile whose identity contradicts them.
- `matchLocked` attributes claims-less documents to whichever saved copy
  shares the tokens, not only the active one, and email-only stable
  identities match by email. An unprovable login keeps the previous
  attribution rather than clearing it.
- A tombstoned document still binds its record (the removal came from
  another device while the account is live here) and still flags copies
  holding the tokens under another name; it is never captured.
- Antigravity sync keys and duplicate folding key on the email, since
  `email` and `email|sub` are one Google account. Other providers can
  share an email across distinct logins and keep the full stable claim.
- `stableUpgrade` keeps the richer `email|sub` form instead of letting a
  stamped email overwrite it.

## Alternatives considered

- Trusting a sharing copy's own signed claims over recorded names: a
  franken-doc also claims its own tokens coherently, so this loses to the
  issuer. Tried first; `TestSwitchRejectsContaminatedCopy` rejected it.
- Clearing the previous active attribution on an unprovable login: a
  dying client writes a stale sign-in back over a finished switch, and
  clearing loses which account was switched to. The fallback stays.
- Recreating a tombstoned account because it is live: re-adds an account
  the user removed on another device and flaps across syncs.
- Hiding crossed copies from the roster: the record stays visible with
  its reason so the user can remove it or sign in again.

## Consequences

Every claims-less Antigravity refresh now performs up to two bounded
issuer calls under the store mutex (userinfo, then refresh exchange on
401/expiry). Crossed copies are flagged suspect once the real owner is
known and stay listed but unswitchable. Live logins whose account was
removed elsewhere bind their remaining record instead of appearing
nameless. Sync can still resurrect foreign `email`-key records until all
devices run this build; repair folds them locally on the next pass.

## Verification

- `internal/accounts/antigravity_switch_test.go::TestClaimsLessLiveNamedByAttestedCopy`
- `internal/accounts/antigravity_switch_test.go::TestAntigravityEmailStableRecordsFold`
- `internal/accounts/antigravity_switch_test.go::TestClaimsLessLiveAttributesBySavedTokens`
- `internal/accounts/antigravity_switch_test.go::TestEmailOnlyLiveAttributesByEmail`
- `internal/accounts/accounts_test.go::TestSwitchRejectsContaminatedCopy`

Proved: all four new tests fail on the parent commit
(`git worktree` at a6ed98e with the new test files): the previous account
stayed active under another login, the crossed copy stayed filed as the
live login, and the email-stable pair did not fold. Output is saved in
`docs/agent-notes-evidence/antigravity-claims-less-red.txt`; the same
tests pass with the fix. Also observed red on the real machine: two
`agiaomei@gmail.com` tabs, the agiaomei record holding
meimingqi33's refresh token (issuer-verified), and
`account_reconcile_skipped reason=sync_tombstone` leaving the live source
unmatched. The attested-first ordering was proven red by
`TestSwitchRejectsContaminatedCopy` during development before the issuer
was moved ahead of recorded names.
