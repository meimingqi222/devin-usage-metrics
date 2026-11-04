# Agent Note: Bound Antigravity OAuth completion and cancellation

Status: implemented

## Problem

The browser displayed "Sign-in complete" immediately after handing an authorization code to the app, before Google's token exchange or saving the account. The exchange shared the five-minute browser wait deadline. More critically, repeated callbacks filled a channel after its sole consumer had moved on; the next callback blocked forever. Unbounded HTTP server shutdown then prevented the login worker from returning even after cancellation. Speculative browser connections with incomplete headers could also hold shutdown open. Existing diagnostics covered sync and loading but recorded no login stages.

## Decision

`internal/accounts/login.go` accepts one validated callback using a single result channel and `sync.Once`. Later callbacks return without enqueueing another result; a mismatched state is rejected without terminating the legitimate login. The callback page acknowledges authorization only. `internal/desktop/accounts.go` reports credential exchange on the main-thread event queue.

The token exchange has its own 30-second deadline, including reading the response body. Callback server shutdown has a one-second grace period followed by `Close`, even when login cancellation has already occurred. Keep this separate bounded shutdown context: an already-cancelled login context cannot allow the current callback response to drain, while an unbounded background context recreates the stuck cancellation.

Login, callback, exchange HTTP status, account save and completion stages use the existing bounded `syncdiag` log with one run ID. Records contain fixed events, elapsed time and sanitized error codes, never OAuth URLs, codes, state, tokens, response bodies or account identity. Token rejection codes use an allowlist (for example `invalid_grant` and `invalid_client`); arbitrary errors and descriptions are omitted. Cancellation during callback shutdown is checked before saving a returned credential.

## Alternatives considered

Larger callback buffers only postpone the deadlock. Checking request cancellation during enqueue still leaves a browser waiting indefinitely while its connection remains open. Immediate server close can interrupt the browser's first response; bounded graceful shutdown lets that response finish and forcibly removes speculative connections. The browser's receipt of a code cannot establish that the app has usable credentials, so retaining the old completion message hides downstream failures.

## Consequences

Authorization and credential exchange have distinct status text. Repeated callbacks cannot delay cancellation or successful account import. A stalled exchange fails within 30 seconds and a lingering browser connection adds at most one second of shutdown wait. Account identity, storage and the live Antigravity sign-in are unchanged. Historical logs cannot retroactively identify the user's specific triggering connection; the reproduced failure modes explain the observed stuck cancellation, and new login records make subsequent attempts diagnosable.

## Verification

- `internal/accounts/login_test.go::TestAntigravityOAuthRepeatedCallbackCanCancel`
- `internal/accounts/login_test.go::TestAntigravityOAuthCancelWithBrowserPreconnect`
- `internal/accounts/login_test.go::TestAntigravityOAuthFlow`
- `internal/accounts/login_test.go::TestGoogleTokenExchangeDeadlineAndDiagnostics`
- `internal/accounts/login_test.go::TestGoogleTokenExchangeCancelsWhileReadingBody`
- `internal/accounts/login_test.go::TestGoogleTokenRejectionDiagnosticsAreSanitized`

Proved: Before the fix, the third callback exceeded its request deadline, cancellation with repeated callbacks did not return, and cancellation with an incomplete browser connection did not return. The failing output is saved to `docs/agent-notes-evidence/antigravity-oauth-cancel-red.txt`. Both bound cancellation regressions passed after the fix.
