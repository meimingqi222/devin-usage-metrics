# Agent Note: Merge OpenCode history across usage schemas

Status: implemented

## Problem

An OpenCode database can have both table pairs while migration is incomplete. Importing only the new pair drops legacy-only usage. Reading both pairs without checking message IDs counts migrated copies twice.

## Decision

`internal/data/database.go::parseSQLAgent` streams the modern message rows and reads only legacy IDs absent from the modern tables. The two reads share a SQLite snapshot. The anti-joins use the new tables' primary keys and let SQLite skip migrated message bodies before the importer decodes JSON.

OpenCode and MimoCode report reasoning separately from visible output. Their shared SQL adapter adds both counters when building the output bucket and preserves valid recorded costs.

## Alternatives considered

Testing whether the modern tables contain any rows misses partial migrations. Keeping every modern message ID in Go would grow memory use with the entire OpenCode history. SQLite already indexes those IDs.

## Consequences

Legacy-only sessions and turns remain available during a partial migration. Migrated IDs use their modern records, and reasoning contributes to reported output and catalog estimates. Existing usage snapshots rebuild once.

## Verification

- `internal/data/data_test.go::TestOpenCodeReadsBothSchemasWithoutDuplicatingMigratedMessages`
- `internal/data/data_test.go::TestSQLiteImportersReadOnlyAndParentRollup`

Proved: the pre-fix run returned zero turns for the empty modern pair and omitted reasoning in both importers. The focused tests pass after the fix. See `docs/agent-notes-evidence/2026-11-23-usage-import-red.txt`.
