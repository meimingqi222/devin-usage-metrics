# Agent Note: Every switch write passes the target checks, and network runs outside the store lock

Status: implemented

## Problem

Two defects in the account switch and refresh paths:

- The Antigravity full-restart switch (`switchAfterQuit` in
  `internal/accounts/antigravity_process.go`) fell back to `Reapply` on
  any `Switch` error. `Reapply` checked nothing: not whether switching was
  on, not whether the record was switchable, not whether it was flagged as
  holding another account's sign-in. A copy `Switch` had just refused was
  written to the live store anyway, and Antigravity restarted on the wrong
  account.
- `ReconcileContext` and `Switch` asked Google to name Antigravity tokens
  while holding the store mutex: up to 5 s for userinfo plus 8 s for a
  refresh exchange per refresh, and 5 s per switch. The desktop reads
  `Store.Enabled()` while drawing every frame, so a slow issuer froze the
  window for that long.

## Decision

- `Switch` returns two sentinel errors for the failures a plain rewrite
  may finish: `errLiveChanged` (the compare-and-swap saw the live store
  move) and `errWriteFailed` (the write itself failed). `switchAfterQuit`
  falls back to `Reapply` only on those two. Every other `Switch` error is
  a refusal and stands.
- `validateTargetLocked` holds every check a target must pass before a
  write: switching on, record exists and is switchable, not flagged
  suspect, saved copy readable and an account document, stable identity
  matching the record. `Switch` and `Reapply` both call it.
- The Google lookups run before the lock. `readLiveAccounts` reads each
  live login once and names a claims-less Antigravity login through the
  issuer; `ReconcileContext` then takes the lock and uses those reads.
  `Switch` asks the issuer about the target copy through `targetIssuer`
  before locking and refuses a mismatch before writing, instead of writing
  and putting the previous login back.
- `Store.Enabled()` reads an atomic mirror of the manifest flag once
  loaded, so it never waits on the mutex.
- `syncclient.ReadAccountSyncConfig` caches the consent per file path;
  `SaveAccountSyncConfig` updates the cache. Only this app writes the
  file, and the desktop read it from disk every frame.

## Alternatives considered

**Keep the any-error fallback and add checks to `Reapply` only.** The
fallback would still retry refusals, now failing twice with a less
specific error; restricting the fallback states the intent.

**Keep the issuer check after the write and roll back.** It works while
the client runs, but in the full-restart path the client is down and the
rollback is one more write that can fail. Checking first removes the
write entirely.

**Release the lock around the lookup inside `ReconcileContext`.** The
loop mutates profiles between lookups; dropping and retaking the lock
mid-loop would let a switch interleave with half-applied state.

## Consequences

A flagged copy is refused by the shared check with "holds another
account's sign-in" before any capture runs; the live-token check still
names "the current account's sign-in" for an unflagged copy, so
`TestSwitchRejectsContaminatedCopy` accepts either message. The live
document a refresh reconciles is read before the lock, so a login that
changes during the issuer lookups is seen on the next refresh. A config
file edited by hand while the app runs is not noticed until restart.

## Verification

- `internal/accounts/switch_safety_test.go::TestRestartFallbackKeepsSwitchChecks`
- `internal/accounts/switch_safety_test.go::TestReapplyRefusesUnswitchableTarget`
- `internal/accounts/switch_safety_test.go::TestIdentityLookupsRunOutsideTheStoreLock`

Proved: the three tests were written against the pre-fix code (with only
`switchAfterQuit` extracted, behavior unchanged) and failed at their bound
assertions: the restart path switched to the refused copy, `Reapply`
wrote a flagged copy, and the store lock was held across the refresh
lookup; output saved to `docs/agent-notes-evidence/switch-safety-and-lock-red.txt`.
All three pass with the change.
