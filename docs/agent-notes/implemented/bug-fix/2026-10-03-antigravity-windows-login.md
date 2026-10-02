# Agent Note: Query current Antigravity login on Windows

Status: implemented

## Problem

The quota card failed with Antigravity OAuth client not found after the old .gemini/oauth_creds.json access token expired. Windows discovery used that file even though installed Antigravity held its current login in state.vscdb. OAuth client auto-discovery was implemented only on macOS.

## Decision

On Windows prefer the current Antigravity globalStorage state database when a valid OAuth token entry exists. Read the nested base64/protobuf token and its refresh token with bounds-checked decoding and open SQLite read-only. Use the database account email. Fall back to the existing file source if current state is unavailable. Extract the installed Windows language server's matching Google OAuth client pair through adjacent PE RIP-relative config references; avoid mixing the separate Cloud Auth client pair. Environment overrides retain priority. Existing 401 refresh logic uses that client and current refresh token. Never write refreshed tokens into Antigravity's database.

## Alternatives considered

Reusing the stale Gemini file repeatedly fails and may refer to a different login. Asking the user to set OAuth client environment variables shifts an application integration bug to the user. Blindly pairing strings from the executable mixes two distinct clients. Changing or overwriting the desktop app database is unnecessary.

## Consequences

Current Windows login can query quota even when the old file has expired. OAuth refresh is in memory for the current-state source. The installed default user-level Antigravity executable and its current Windows x64 config layout are required for automatic client discovery; unsupported layouts fail closed. macOS keychain consent behavior is unchanged.

## Verification

- `src/quota.rs::antigravity_state_token_reads_current_access_and_refresh`
- `src/quota.rs::antigravity_windows_client_pairs_config_references`
- `tests/antigravity_live_quota.rs::current_windows_login_can_fetch_quota`

Proved: Temporarily dropping the parsed refresh token caused the bound token test to fail with empty refresh versus fake-refresh, then the change was reverted. Synthetic parser tests cover truncated state, invalid PE data and absent client references. On the user's actual installation, the current access token first returned 401; refresh and quota queries returned HTTP 200. The Rust live integration test then passed and returned nonempty quota windows without printing credentials or writing the login database.
