# Agent Note: Keep portable account backups independent of active sign-ins

Status: implemented
Partly-superseded-by: 2026-11-05-account-sync-stable-identity.md

## Problem

Browser authorization is not always available during device migration. Users need portable account backups and optional cross-device backup sync, but ordinary usage sync must never silently start publishing credentials. Reusing the account switch/upsert path for imports could overwrite the active profile when an opaque credential has no identity claims. Independent remote objects would also be deleted by the usage manifest garbage collector.

## Decision

`internal/accounts/backup.go` defines a versioned password-encrypted archive using AES-256-GCM with PBKDF2-SHA256, 600,000 rounds, random salt and nonce, a domain-specific authenticated tag, bounded sizes and fixed KDF parameters. The user-selected minimum password length is 10 Unicode characters for export, import and sync; the UI uses the same limit. The plaintext schema carries supported provider credential documents, names and credential modification times; no current-login or switching preference is portable. File import decrypts to a preview and requires a final explicit action.

Imports add only missing saved profiles under a store lock and roll back newly created files if the manifest cannot be saved. They never call the switch/upsert active-account fallback. Imported profiles are not inferred to be active until reconciled with a real live sign-in or explicitly switched. Explicit file imports preserve existing local credentials and may restore opaque credentials keyed by document hash. Automatic sync is a separate LWW-element-set over StableID (see the 2026-11-05 identity note): it reconciles additions, names, saved-credential updates and deletions, never treats absence as a delete, and protects the account currently in use. Antigravity shapes are adapted for the target platform, preserving identity metadata. Removing a saved profile records a timestamped deletion shared across devices; manual import or explicit account capture can restore it. Background quota discovery must not resurrect a removed profile. Active account identity comes from actual agent discovery, never from the most recently used saved profile.

`internal/syncclient/accounts.go` stores independent consent scoped to the exact API and GitHub tenant, with no password on disk. Each device uploads only its own encrypted snapshot through the existing authenticated object API. Snapshots that cannot be decrypted are skipped and reported rather than aborting the cycle; a cycle still fails without publishing when no snapshot at all opens (see the 2026-11-05 rotation note). The client requires the server's authenticated account-backup capability; `sync-server/main.go` retains valid account snapshots outside the usage manifest graph. The same tenant may DELETE only `accounts-v1-*` objects (turning sync off removes this device's snapshot; unread or abandoned device snapshots can be dropped explicitly). Usage CAS objects stay undeletable. Existing usage formats, identities, devices, configuration and authentication remain compatible.

The desktop offers one account-sync switch and remembers the password in the system credential store when selected. It restores automatic sync after restarting, preserves the password during network failures, and only requires new input on a password error. Turning off sync or changing identity cancels in-flight work and deletes the scoped remembered password. System credential operations happen on workers; UI results only enter through the main-thread event queue. File dialog window ownership stays in `internal/desktop/host.go`.

## Alternatives considered

Uploading ordinary account JSON or deriving its encryption key from the existing GitHub session/device ID would expose credentials or make cross-device recovery depend on server-known material. One mutable shared snapshot would require a separate CAS merge protocol and could lose concurrent additions. Blindly replacing local tokens from a newer-looking remote file can revoke a rotating local refresh token. Per-device snapshots, independent name/credential modification times and timestamped deletion records resolve concurrent changes without a new protocol, a separate migration or shared-object overwrite races.

## Consequences

The feature is opt-in and uses one shared password across devices. Remembering it is optional and uses native system credential storage; plaintext configuration contains consent and scope only. Automatic sync now handles rename, saved-token updates and deletion, and retains deletion records so stale offline snapshots cannot resurrect accounts. Active sign-ins and their saved credential remain protected until switching away. Manual file import is still an explicit restore. Ordinary network failures permit retry without retyping. The default UI shows synchronization state and account count, with exceptional protection details behind Details.

Verification: `internal/accounts/sync_test.go` covers credential/name updates, deletion convergence, live-sign-in protection, concurrent independent changes and rollback. `internal/syncclient/accounts_test.go` exercises two-device account lifecycles over HTTP. `internal/desktop/account_sync_test.go` covers remembered restart, retry, turning off sync and UI states, plus an opt-in native Windows credential-store round trip. Existing backup and server retention tests remain applicable.

## Superseded

Sync identity, "absence is not deletion", re-add versus tombstone, and credTime-on-byte-change are owned by `2026-11-05-account-sync-stable-identity.md`. This note remains authority for the encrypted envelope, file import, consent/scope, live-store isolation and server GC retention.
