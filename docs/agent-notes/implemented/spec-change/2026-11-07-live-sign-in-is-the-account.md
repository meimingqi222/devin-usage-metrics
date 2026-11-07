# Agent Note: the sign-in an agent holds is the account

Status: implemented

## Problem

The roster only listed accounts that had been captured, and a refresh
captured a live sign-in only when the account was not tombstoned and the
live store was writable. Two gaps made the manual "Capture current login"
button the only way to get an account the user already had:

- A deletion writes a tombstone (the manifest's deleted_accounts map) so
  sync does not file a removed copy back. ReconcileContext treated that
  tombstone as a ban on the account itself, so an account removed on
  another device - or removed here while signed in elsewhere - stayed
  missing from the roster even though this device was signed in to it.
  Add cleared the tombstone, so the button worked and the automatic path
  did not.
- The capture also required Account.Writable(), which says the live store
  can be replaced during a switch. Saving a copy needs no such write, so a
  read-only live store (Codex keyring, a store this app never writes) was
  the one login the refresh refused to list.

Observed on a real machine: account_reconcile_decision provider=claude
capture=tombstoned match_reason=unmatched, and the account only appeared
after the user pressed capture.

## Decision

- The sign-in an agent holds is the account. A tombstone stops sync from
  re-importing a deleted copy; it never hides the live login. Reconcile
  binds a surviving record as before and otherwise captures the account
  like any other live login.
- Capturing clears the tombstone through clearTombstoneLocked, which
  stamps the profile newer than the deletion record (and the credential
  file mtime with it) so the next sync cannot retire the account the user
  still signs in to. Add shares that helper.
- Capturing no longer requires a writable live store. Whether an account
  can be switched to is decided at switch time, where the read-only reason
  is already reported.
- Diagnostics carry tombstoned and bound_record instead of folding the
  tombstone into match_reason.
- The manual capture entry is gone from the add-account panel: it did
  exactly what the refresh does, on the same quota.Discover set. The panel
  only action is now the browser sign-in, for adding an account that is not
  signed in yet, and it states that existing sign-ins are added
  automatically.
- "+ Add account..." in the account menu is limited to switchable agents;
  the menu is reachable from every quota card, Devin included, and used to
  open a panel whose only action could not work there.
- An account in use is occupied: it cannot be removed. The single-account
  Remove hides itself for the active record and refuses a record whose name
  matches the live label (a sign-in switched outside the app before the
  refresh bound it). RemoveAll, relabelled "Remove other saved accounts",
  keeps every record the agents are signed in to and deletes the rest with
  their tombstones, so the removals still sync. Removing a live copy would
  only be undone by the next refresh.

## Alternatives considered

- Keeping the tombstone absolute and only hiding the button: the account
  stays missing until the user finds a button that no longer exists.
- Making RemoveAll delete every copy including the ones in use: the next
  refresh captures them again, so the action reads as broken. The occupied
  records are kept instead, which is also what the single-account Remove
  already enforced.
- Re-adding only when the live document changed since the deletion: token
  rotation makes an unchanged document rare, so a deletion would be undone
  within a refresh cycle anyway and the rule would be arbitrary.

## Consequences

An account deleted on another device reappears here while this device is
signed in to it, and sync no longer retires it. A read-only live store now
lists its account, which cannot be switched to and says so when tried.
"Remove other saved accounts" leaves the accounts in use listed and only
removes the rest; forgetting a live account entirely still means signing
out of the agent.

## Verification

- internal/accounts/sync_test.go::TestQuotaDiscoveryRestoresDeletedLiveAccountAndSyncKeepsIt
  (rewritten from TestAccountSyncDeletionSurvivesQuotaDiscoveryAndExplicitRestore,
  which pinned the opposite rule)
- internal/desktop/accounts_test.go::TestAccountsManagerDialog
- internal/desktop/accounts_test.go::TestDedicatedAddBackupModalAndCleanManager

Proved on d988f51 (git worktree at HEAD with the new test files):
TestQuotaDiscoveryRestoresDeletedLiveAccountAndSyncKeepsIt fails with
"quota discovery did not restore the live account: []" (output in
docs/agent-notes-evidence/live-login-tombstone-red.txt) and
TestRemoveAllKeepsTheAccountInUse fails with "remove all did not keep the
account in use: []" (output in
docs/agent-notes-evidence/remove-all-keeps-live-account-red.txt); both pass
with the change. The desktop tests assert the capture entry is gone and the
relabelled removal button.
gofmt -l main.go internal, go vet ./..., go test -race ./... and the
sync-server module checks are clean.
