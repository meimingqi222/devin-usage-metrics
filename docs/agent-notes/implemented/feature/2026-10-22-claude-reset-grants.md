# Agent Note: Show and spend Claude reset grants only with confirmation

Status: implemented

## Problem

Some Claude subscriptions carry a few reset grants (Anthropic's `cedar_ember` program) that clear a usage window early. The quota page did not show them, and spending one is irreversible and scarce.

## Decision

- `internal/quota/claude_reset.go` reads grants from the existing usage request with `?cedar_ember=1&skip_spend=1`; `skip_spend` keeps the read free of side effects. The contract follows Claude Code and CLIProxyAPI's management center (`copilot-refs/cpa`).
- Parsing refuses rather than guesses: a bad grant id, a count out of range or a duplicate id rejects the whole block, and missing `usable_now` / `use_requires_limit` flags take the refusing side.
- `ClaimClaudeReset` re-reads the profile (organization UUID) and the grant status right before spending, returns the blocker without spending when one applies, sends one `POST /api/organizations/{org}/reset_rate_limits` with a fresh `request_id`, and never retries. A lost or unreadable answer is `ErrClaudeResetUnknown`; the app then re-reads quotas instead of offering a retry.
- `internal/desktop/quota.go` shows the remaining count and the earliest expiry, disables the button with the reason while blocked, and spends only after an `AlertDialog` confirmation.

## Alternatives considered

Retrying an ambiguous claim like CLIProxyAPI's ten-minute journal would need durable bookkeeping of request IDs across restarts; without it a retry could spend twice, so the app re-reads instead. A separate status request per card would double Claude's usage calls and their rate limit.

## Consequences

The usage request gains two query parameters. A grant the API does not describe completely is not offered. Tests use a local server only; nothing in the suite can reach Anthropic.

## Verification

- `internal/quota/claude_reset_test.go::TestClaudeResetGrantsParse`
- `internal/quota/claude_reset_test.go::TestClaimClaudeReset`
- `internal/desktop/quota_test.go::TestClaudeResetGrantNeedsConfirmation`
