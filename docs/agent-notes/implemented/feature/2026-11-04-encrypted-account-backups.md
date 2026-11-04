# Agent Note: Keep portable account backups independent of active sign-ins

Status: implemented

## Problem

Browser authorization is not always available during device migration. Users need portable account backups and optional cross-device backup sync, but ordinary usage sync must never silently start publishing credentials. Reusing the account switch/upsert path for imports could overwrite the active profile when an opaque credential has no identity claims. Independent remote objects would also be deleted by the usage manifest garbage collector.

## Decision

`internal/accounts/backup.go` defines a versioned password-encrypted archive using AES-256-GCM with PBKDF2-SHA256, 600,000 rounds, random salt and nonce, a domain-specific authenticated tag, bounded sizes and fixed KDF parameters. The user-selected minimum password length is 10 Unicode characters for export, import and sync; the UI uses the same limit. The plaintext schema carries supported provider credential documents, names and credential modification times; no current-login or switching preference is portable. File import decrypts to a preview and requires a final explicit action.

Imports add only missing saved profiles under a store lock and roll back newly created files if the manifest cannot be saved. They never call the switch/upsert active-account fallback. Imported profiles are not inferred to be active until reconciled with a real live sign-in or explicitly switched. Existing credentials always win locally. Antigravity shapes are adapted for the target platform, preserving identity metadata. Removing a local profile records an account identity suppression marker for automatic sync; manual file import can deliberately restore it.

`internal/syncclient/accounts.go` stores independent consent scoped to the exact API and GitHub tenant, with no password on disk. Each device uploads only its own encrypted snapshot through the existing authenticated object API. All remote snapshots must decrypt before any upload/import. The client requires the server's authenticated account-backup capability; `sync-server/main.go` retains valid account snapshots outside the usage manifest graph. Existing usage formats, identities, devices, configuration and authentication remain compatible.

The desktop keeps the unlock password only for this process, supports lock/disable, clears it on an unsuccessful sync, cancels in-flight work when locking or changing identity, and applies result state only through its main-thread event queue. File dialog window ownership stays in `internal/desktop/host.go`.

## Alternatives considered

Uploading ordinary account JSON or deriving its encryption key from the existing GitHub session/device ID would expose credentials or make cross-device recovery depend on server-known material. One mutable shared snapshot would require a separate CAS merge protocol and could lose concurrent additions. Blindly replacing local tokens from a newer-looking remote file can revoke a rotating local refresh token. Per-device snapshots plus additive local imports keep these contracts separate.

## Consequences

The feature is opt-in and requires one shared password across devices. Application restart requires unlocking again. Old servers continue usage sync but must be upgraded for account backups. Existing-account token updates and remote deletions are intentionally not applied locally. Local suppression markers prevent automatic resurrection; cloud snapshots remain until administratively removed. Credential modification times survive import and unchanged reconciliation does not manufacture a newer timestamp. This is account backup distribution, not simultaneous live-login replication; upstream expiry/revocation still requires provider reauthorization.

Verification is covered by `internal/accounts/backup_test.go`, `internal/syncclient/accounts_test.go`, `internal/desktop/account_backup_test.go`, and `sync-server/account_backups_test.go`, including tampering/wrong-password rejection, no plaintext upload, two-device convergence, preservation of live/local credentials, rollback, opaque identities, suppression, preview confirmation, lock/scope isolation and GC retention.
