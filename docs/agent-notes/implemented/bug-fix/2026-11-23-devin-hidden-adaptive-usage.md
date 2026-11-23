# Agent Note: Preserve adaptive billing for hidden Devin usage

Status: implemented

## Problem

Hidden Devin sessions contribute turns without producing a session summary. The importer previously skipped their adaptive billing route. Republishing the corrected turns could leave both versions on the device's sync head because billing route is part of the turn identity.

## Decision

Read the selection before filtering hidden summaries in `internal/data/database.go`. Mark the imported turns and use that route in analytics. In `internal/syncclient/publish.go`, suppress an old unrouted record only when the current hidden turn matches its complete normalized identity with the route cleared.

## Alternatives considered

Replacing every published turn for a hidden session would discard older history that no longer exists in the local database. Publishing hidden summaries to trigger the existing replacement rule would expose hidden sessions in the UI.

## Consequences

Corrected turns replace their old counterparts on the next sync. Unmatched history remains available.

## Verification

- `internal/data/data_test.go::TestDevinAdaptiveBilling`
- `internal/syncclient/efficiency_test.go::TestPublishReplacesChangedTurnsOfLocalSessions`

Proved: before the fixes, the hidden turn had an empty billing route, and a later publish retained both versions. Both tests pass after the fix. See `docs/agent-notes-evidence/2026-11-23-usage-import-red.txt`.
