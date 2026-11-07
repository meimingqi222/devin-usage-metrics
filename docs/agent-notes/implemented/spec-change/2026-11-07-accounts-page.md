# Agent Note: Accounts, switching and account sync live on one Accounts page

Status: implemented

## Problem

Account management was spread over five entry points: a toolbar button
on the quota page, each quota card's add button, the card's account menu,
the tray's account menu, and Settings › Sync & Devices. The account
manager was a modal holding switching, import/export, every agent's
accounts and the footer actions behind a 380px scroll, so "Remove other
saved accounts" was off screen at any window height. Account sync was
split three ways: its switch in Settings, its status in the manager, its
password in a third dialog. The tray's "Manage accounts…" set a flag
drawn only in the main window, which may be hidden.

## Decision

- A sidebar entry "Accounts" under "Live quotas" opens the Accounts page
  (`screen == "accounts"`, `accountsView` in `internal/desktop/accounts.go`).
  It holds switching, account sync (`accountSyncSettings`), import/export,
  the per-agent lists and the footer, in the page's own scroll.
- `openAccounts` navigates to the page and brings up the main window
  through `trayUI.openDashboard`, so the tray menu lands on it too.
- The quota page's "Accounts" toolbar button is gone; quota cards keep
  preview, switch and "+ Add account" (which leads to the page while
  switching is off).
- Settings › Sync & Devices keeps the GitHub sign-in the sync depends on
  and links to the page for account sync. Before sign-in the page says
  so and offers "Open sync settings".
- The account-sync confirmations (turn off, drop unreadable or foreign
  snapshots) are drawn by the main window on every screen
  (`accountSyncConfirms`): a pending "turn off" holds automatic syncs and
  must never wait off screen.
- Destructive confirmations use `dangerConfirm`: a red action button and
  the focus on Cancel.

## Alternatives considered

- Tabs inside the existing modal (Accounts / Sync / Advanced): smaller
  change, but sync would still live in a dialog opened from several
  places, and the entry points would stay as many.
- Keeping the password entry inline on the page: the existing unlock
  dialog already handles setup, unlock and rotation; it opens from the
  page and stays one flow.

## Consequences

Tests that pinned the old placement were rewritten in the same change:
`TestAccountsDialogShowsSyncStatus` became
`internal/desktop/account_sync_test.go::TestAccountsPageOwnsAccountSync`;
`TestAccountSyncSettingsAreReadable` and
`TestAccountSyncDisableRequiresConfirmation` render the page and
`accountSyncConfirms` instead of the settings dialog;
`TestAccountSyncSettingsLockAndEndpointChange` asserts the settings link.
The tray entry is pinned by
`internal/desktop/account_ux_test.go::TestTrayManageAccountsOpensThePage`.
The account sync status no longer flags an unsaved endpoint edit, since
the page does not edit the endpoint; Settings still re-locks account sync
when a new endpoint is saved.
