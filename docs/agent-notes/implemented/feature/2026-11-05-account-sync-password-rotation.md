# Agent Note: Account-sync password rotation skips unreadable snapshots

Status: implemented

## Problem

Changing the sync password was impossible once two devices had ever synced. Every device seals its own snapshot, and a cycle had to decrypt all remote snapshots before publishing anything. The first device to use a new password could not open the others' old-sealed snapshots and aborted before publishing; other devices aborted on the new-sealed snapshot in turn. Account snapshots are retention roots on the server, so the stale objects never disappear — disabling and re-enabling sync on every device could not recover either.

## Decision

`internal/syncclient/accounts.go` takes a `prevPassword` alongside the password. During a rotation it still opens snapshots sealed with the previous password, so the rotating device publishes a complete merge instead of a local-only one. Snapshots that neither password opens are counted in `AccountSyncResult.Unreadable` and skipped rather than aborting the cycle. The cycle fails with `accounts.ErrBackupPassword` only when no remote snapshot at all could be opened — that still means the entered password is wrong, and nothing is published or imported in that case. This deliberately replaces the earlier all-or-nothing decrypt rule from the 2026-11-04 note; the wrong-password guarantee is preserved by the zero-readable guard, not by aborting on any single failure.

The desktop treats the password dialog as three states: first setup, unlock (password lost), and change (password known). Change asks for the new password twice and hands the previous one to the next cycle as `prevPassword`; a successful sync updates the remembered password in the system credential store. Devices still on the old password keep syncing among themselves, report the rotated device as unreadable ("N 台设备的快照无法用当前密码解开…" in settings, "· N 台待更新" in the accounts dialog), and finish the change by entering the new password. A mistyped new password is not data loss: other devices simply report the device as unreadable until the password is corrected there.

## Alternatives considered

Recording a password-generation key ID in the envelope would label snapshots precisely, but requires an envelope format version and adds little over the readable/unreadable split while there is only one shared password. A server-side key-wrapping envelope would break the storage-only end-to-end encryption model. Keeping all-or-nothing decryption would keep rotation impossible.

## Consequences

Offline devices sealed with a retired password leave a permanent one-line "unreadable" note until they are updated or abandoned; their snapshots stay unreadable under the new password forever, which is inherent to end-to-end encryption. A device that never knew the old password still cannot read old-sealed data — rotation is not recovery.

Verification: `internal/syncclient/accounts_test.go` `TestSyncAccountsPasswordRotation` (rotate, stale-device continue, catch-up, convergence) and the wrong-password tail of `TestEncryptedAccountSyncTwoDevicesAndWrongPassword` (no publish on a wrong password). `internal/desktop/account_sync_test.go` covers change-mode copy, the unreadable warning, and an end-to-end rotation that updates the remembered password.
