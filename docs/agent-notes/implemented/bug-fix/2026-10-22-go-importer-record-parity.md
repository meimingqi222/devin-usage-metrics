# Agent Note: Preserve Rust importer records beyond CLI totals

Status: implemented

## Problem

The real-history migration comparison matched daily tokens and costs while hiding record differences: Grok counted `turn_completed` events without usage as empty turns and treated missing optional update logs as errors; WorkBuddy reused the preceding record's session ID for metadata without an ID instead of resolving the filename fallback. Summary equality alone therefore failed to protect session lists and sync completeness.

## Decision

`internal/data/files.go` keeps summary-only Grok sessions without an error and only creates turns for events with a usage field. WorkBuddy resolves the filename fallback independently for every record, matching `src/local_sources.rs`. `internal/data/data.go` versions parsed snapshots with `snapshotVersion`; it rejects version 1 caches even while their TTL and source signatures still match. Future changes to parsed record semantics must bump this version.

## Alternatives considered

Comparing only CLI totals misses zero-token turns and metadata-only sessions. Reusing a fresh old snapshot, or an expired snapshot whose file signatures still match, hides parser fixes indefinitely without a manual refresh. Removing metadata-only sessions would change Rust's session-list behavior.

## Consequences

The first load after this parser change reparses local history. Device identity, source databases and sync formats remain unchanged. The real-history comparison now checks session, turn and source-error counts in addition to CSV rows; it does not claim field-by-field equality of all real-history metadata.

## Verification

- `internal/data/data_test.go::TestGrokOptionalUpdatesAndUsageMatchRust`
- `internal/data/data_test.go::TestWorkBuddyMissingSessionIDUsesFileNamePerRecord`
- `internal/data/data_test.go::TestLegacySnapshotCannotHideImporterFixes`

Proved: Before the fixes, the focused tests failed at the record-count and legacy-cache assertions. Output saved to `docs/agent-notes-evidence/go-importer-parity-red.txt`; all three tests pass after restoring the Rust behavior and rejecting old snapshots.
