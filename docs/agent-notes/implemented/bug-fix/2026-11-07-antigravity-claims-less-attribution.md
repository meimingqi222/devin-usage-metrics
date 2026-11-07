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

Even then the crossed copy still rendered as a working account: the
live-token contamination check only catches a copy of the account
currently signed in, so a copy holding another saved login's tokens
looked clean, stayed listed under the wrong name, and answered quota for
the copied account - the roster showed two tabs reporting identical
quota for what the UI claimed were different people.

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
- A tombstoned document binds its record (the removal came from another
  device while the account is live here) and still flags copies holding the
  tokens under another name. It used never to be captured; see
  `spec-change/2026-11-07-live-sign-in-is-the-account.md`, which reversed
  that: the live login is the account, so it is captured and the tombstone
  is cleared.
- Antigravity sync keys and duplicate folding key on the email, since
  `email` and `email|sub` are one Google account. Other providers can
  share an email across distinct logins and keep the full stable claim.
- `stableUpgrade` keeps the richer `email|sub` form instead of letting a
  stamped email overwrite it.
- `flagCrossedCopiesLocked` compares saved-record pairs: two records
  sharing a token pair hold one login, so when their recorded names
  conflict, the side with weaker identity evidence is the copy and is
  flagged suspect. Claims the issuer wrote outrank an `email|sub`
  record, which outranks a bare stamped name (`crossedLevel`); the
  active record is exempt because it just read the live login back, and
  equal evidence on both sides flags nothing. `credentialid.Claims`
  reads only what the agent or issuer wrote - the `email` field and
  `id_token_email` row are this app's own stamps and cannot testify.

## Alternatives considered

- Trusting a sharing copy's own signed claims over recorded names: a
  franken-doc also claims its own tokens coherently, so this loses to the
  issuer. Tried first; `TestSwitchRejectsContaminatedCopy` rejected it.
- Clearing the previous active attribution on an unprovable login: a
  dying client writes a stale sign-in back over a finished switch, and
  clearing loses which account was switched to. The fallback stays.
- Recreating a tombstoned account because it is live: rejected here, then
  required by the user in
  `spec-change/2026-11-07-live-sign-in-is-the-account.md` - the live login
  is the account, and the tombstone is cleared so sync cannot retire it.
- Hiding crossed copies from the roster: the record stays visible with
  its reason so the user can remove it or sign in again.

## Consequences

Every claims-less Antigravity refresh now performs up to two bounded
issuer calls (userinfo, then refresh exchange on 401/expiry). They run in
readLiveAccounts before the store mutex is taken; see
2026-11-07-switch-target-checks-and-lock.md. Crossed copies are flagged suspect once the real owner is
known and stay listed but unswitchable. Live logins whose account was
removed elsewhere bind their remaining record instead of appearing
nameless, and are captured when no record survived. Sync can still
resurrect foreign `email`-key records until all devices run this build;
repair folds them locally on the next pass.

## Verification

- `internal/accounts/antigravity_switch_test.go::TestClaimsLessLiveNamedByAttestedCopy`
- `internal/accounts/antigravity_switch_test.go::TestAntigravityEmailStableRecordsFold`
- `internal/accounts/antigravity_switch_test.go::TestClaimsLessLiveAttributesBySavedTokens`
- `internal/accounts/antigravity_switch_test.go::TestEmailOnlyLiveAttributesByEmail`
- `internal/accounts/antigravity_switch_test.go::TestCrossedCopyOfSavedLoginIsFlagged`
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
was moved ahead of recorded names. `TestCrossedCopyOfSavedLoginIsFlagged`
fails on d988f51 (crossed copy stayed switchable under both the
claims-bearing and claims-less shapes; output in
`docs/agent-notes-evidence/crossed-copy-pair-red.txt`) and passes with
`flagCrossedCopiesLocked`; that pair is also exactly the real machine's
agiaomei/mmq records, which reported identical quota until this check.
