# Agent Note: Antigravity on Windows signs in from the Credential Manager

Status: implemented

## Problem

Switching Antigravity accounts on Windows did nothing. Antigravity 2.x
(observed on 2.29) reads its sign-in from the Windows Credential Manager
item `gemini:antigravity` (go-keyring's "<service>:<user>" generic
credential, UTF-8 JSON: `token`, `auth_method`, `id_token_email`,
`id_token`). This app read and wrote only the state database's
`antigravityUnifiedStateSync.oauthToken` plus the `~/.gemini` files, its
"keychain" support being macOS-only. A switch wrote the target into the
state database, read it back there, and reported success, while the
Credential Manager kept the previous account and Antigravity used it.
Observed on a real machine: after a "successful" switch the state database
held the target's tokens and the Credential Manager the previous
account's.

## Decision

- On Windows the keychain calls for the `gemini` service go to the
  Credential Manager (`internal/quota/wincred_windows.go`, plain
  `advapi32` syscalls, no cgo): `keyExists`, `keyReadFullItem` and
  `keyWriteDoc` dispatch through `winKeyring`. Discovery takes the item as
  Antigravity's live sign-in (kind `keychain`), without the macOS consent
  gate: the Credential Manager raises no prompt and the state database was
  already read without one. The state database stays the fallback when the
  item is absent, and a switch still writes it and the `~/.gemini` files as
  mirrors, as Antigravity Tools does.
- The keychain shape written for a switch carries `id_token_email`, as the
  client and Antigravity Tools write it. `adaptCredential` now runs before
  `switchDocument` so the email this app stamped can fill it.
- A test binary never addresses the real item: `antigravityWinCredTarget`
  returns `aum-test:<hash of APPDATA>:gemini:antigravity` under
  `testing.Testing()`, and the quota, accounts and desktop fixtures delete
  it through `quota.ForgetTestCredential`. During this fix a test run
  before that guard existed overwrote the developer's real item with
  fixture tokens; temporary HOME/APPDATA do not isolate a global store.

## Alternatives considered

**Keep the state database as the live store and add the Credential
Manager as one more mirror.** Discovery would keep naming the account in
the state database, not the one Antigravity uses, so the app would go on
showing the wrong account as active whenever the two disagree.

**Gate the Credential Manager behind the keychain consent setting.**
Windows users would have to find a macOS-motivated setting before
switching works at all.

**Isolate tests with an environment variable set in each fixture.** A test
that forgets the fixture would touch the real item; `testing.Testing()`
makes the scratch name the default for every test binary.

## Consequences

Windows discovery reports the account Antigravity actually uses, and a
switch replaces it where Antigravity reads it. The client's own
`antigravityAuthStatus` cache in the state database is left alone, as
Antigravity Tools leaves it; the client refreshes it on start. Importing
`testing` in `internal/quota` only uses `testing.Testing()`; production
binaries resolve the real name.

## Verification

- `internal/quota/wincred_test.go::TestAntigravityCredentialManagerIsTheLiveSignIn`
- `internal/quota/wincred_test.go::TestTestBinaryNeverUsesRealCredential`

Proved: with `winKeyring` forced to false, the Credential Manager test
failed at its first write through the keychain path ("keychain update
failed"); output saved to
`docs/agent-notes-evidence/antigravity-credential-manager-red.txt`. The
guard was restored and the test passed. The isolation test was added with
the guard and has not been observed red; the incident it prevents is
described above.
