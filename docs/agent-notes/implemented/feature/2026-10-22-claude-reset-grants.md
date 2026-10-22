# Agent Note: Show and spend Claude reset grants only with confirmation

Status: implemented

## Problem

Some Claude subscriptions carry a few reset grants (Anthropic's `cedar_ember` program) that clear a usage window early. The quota page did not show them, and spending one is irreversible and scarce.

## Decision

- Quota windows come from the plain `GET /api/oauth/usage`. Grants come from a separate `?cedar_ember=1&skip_spend=1` read, which `claudeResetGrants` in `internal/quota/claude_reset.go` sends at most once an hour per login and only after a successful usage read. A failed grants read keeps the previous status and still counts as the hour's check; a cancelled one does not. The claim's own fresh read refreshes the cache, and a sent claim clears it so the next refresh re-reads. The contract follows Claude Code, openusage (`robinebers/openusage`) and CLIProxyAPI's management center (`copilot-refs/cpa`).
- Because the status may be an hour old, the desktop enables the button despite a stale `not_limited` when a window the grant clears is full in the latest usage read; the claim re-checks eligibility live.
- Parsing refuses rather than guesses: a bad grant id, a count out of range or a duplicate id rejects the whole block, and missing `usable_now` / `use_requires_limit` flags take the refusing side.
- `ClaimClaudeReset` re-reads the profile (organization UUID) and the grant status right before spending, returns the blocker without spending when one applies, sends one `POST /api/organizations/{org}/reset_rate_limits` with a fresh `request_id`, and never retries. A lost or unreadable answer is `ErrClaudeResetUnknown`; the app then re-reads quotas instead of offering a retry.
- `internal/desktop/quota.go` shows the remaining count and the earliest expiry, disables the button with the reason while blocked, and spends only after an `AlertDialog` confirmation.

## Alternatives considered

Retrying an ambiguous claim like CLIProxyAPI's ten-minute journal would need durable bookkeeping of request IDs across restarts; without it a retry could spend twice, so the app re-reads instead.

Adding `cedar_ember=1` to every usage read (the first implementation) is rejected: Anthropic rate-limits that variant hard when polled, and every five-minute refresh drew an hour-long 429 on the usage endpoint itself, hiding all Claude windows (openusage issue #1355 saw the same).

## Consequences

A login costs at most one grants read an hour beyond its usage reads, so a grant newly offered or spent elsewhere can take up to an hour to appear. A grant the API does not describe completely is not offered. Tests use a local server only; nothing in the suite can reach Anthropic.

## Verification

- `internal/quota/claude_reset_test.go::TestClaudeResetGrantsParse`
- `internal/quota/claude_reset_test.go::TestClaimClaudeReset`
- `internal/desktop/quota_test.go::TestClaudeResetGrantNeedsConfirmation`
- `internal/quota/claude_reset_test.go::TestClaudeGrantsReadAtMostHourly`
- `internal/desktop/quota_test.go::TestStaleNotLimitedYieldsToFullWindow`

Proved: disabled the hourly gate in `claudeResetGrants` → `TestClaudeGrantsReadAtMostHourly` failed with "grants re-read within the hour", then restored and passed; output saved to `docs/agent-notes-evidence/claude-grants-hourly-red.txt`.
