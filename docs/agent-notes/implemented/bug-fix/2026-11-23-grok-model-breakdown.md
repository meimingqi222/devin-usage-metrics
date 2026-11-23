# Agent Note: Allocate Grok usage by recorded model

Status: implemented

## Problem

Some Grok completion records contain a `modelUsage` map. The importer sorted the keys and charged the full completion to the first model.

## Decision

`internal/data/files.go::recordGrokUsage` emits one normalized turn per metered model. It retains the aggregate's unmatched tokens as unknown. A single model can inherit missing fields from the completion, including older records with an empty model object. Grok's prompt count includes cache writes and reads, and its output count includes reasoning. Aggregate-only ticks do not create a second charge when the model rows cover all tokens.

## Alternatives considered

Using the session model or the first key for the aggregate assigns other models' usage and rates to an unrelated model. Keeping per-model rows alone can drop usage from a partial breakdown.

## Consequences

The standard usage, pricing and sync paths see the model breakdown. The session retains its source-reported message count. The parser orders keys only for records with multiple models so single-model records avoid a sort. Existing usage snapshots rebuild once.

## Verification

- `internal/data/data_test.go::TestGrokModelUsagePreservesAttributionAndTotals`
- `internal/data/data_test.go::TestGrokAggregateTicksDoNotDoubleChargeModelBreakdown`

Proved: the pre-fix run returned three aggregate turns instead of five normalized rows. The focused test passes after the fix. See `docs/agent-notes-evidence/2026-11-23-usage-import-red.txt`.

Proved: the review run counted output as 8 and 11 instead of 6 and 9, and aggregate-only ticks created a third row beside two fully attributed rows. See `docs/agent-notes-evidence/2026-11-23-usage-import-red.txt`.
