# Agent Note: Attribute Antigravity usage to the model that generated it

Status: implemented

## Problem

`parseAG` previously assigned every turn the session's last model. A real conversation that used `gemini-4-carbon`, `gemini-4-neon` and `gemini-4-hydrogen` therefore reported all its usage as `gemini-4-hydrogen`. When the importer originally left turn models empty, the tray and compaction also missed pricing that analytics and the CLI could resolve through the session model.

## Decision

`internal/data/database.go::parseAG` reads each generation record's model name at protobuf path `1.19` and matches it to step usage using the message or response ID from generation usage at `1.4`. The model enum at `1.3` supplies a fallback only when it has one known label within the conversation. A nonzero enum with no unambiguous match is displayed as `unknown` when the conversation has enum metadata. Old records with sparse metadata still fall back to the session label. The snapshot version in `internal/data/data.go` forces a reimport after the change.

`internal/desktop/tray.go::trayEntriesOf` still falls back to `session.DisplayModel()` when a turn's model is empty, matching analytics/CLI. That covers old snapshots, remote sync payloads, and tests that construct Antigravity turns without a session model.

## Alternatives considered

**Leave turns empty and teach every Cost caller to pass the session model.** Analytics and CLI already did; the tray did not. A later Cost site (compaction, tray, CLI) will miss the fallback again.

**Use the enum alone.** Live histories reuse one enum for multiple rollout labels. Matching the generation by message or response ID preserves the logged label.

**Join rows by index.** A generation can be recorded on several steps. A response ID identifies the same generation across those rows.

## Consequences

Usage and pricing use the recorded model for each resolved turn, including retry attempts. Session summaries continue to use the last selected model.

## Verification

- `internal/data/data_test.go::TestAntigravityFullUsageSuppressesEcho`
- `internal/data/data_test.go::TestAntigravityAttributesUsageToEachModel`
- `internal/desktop/tray_test.go::TestTrayAntigravitySessionModelPricesEmptyTurns`

Proved: the old importer failed the mixed-model assertions by returning `gemini-4-hydrogen` for both Carbon labels and the unresolved records. See `docs/agent-notes-evidence/antigravity-model-attribution-red.txt`. The legacy fallback failures are recorded in `docs/agent-notes-evidence/antigravity-turn-model-red.txt` and `docs/agent-notes-evidence/antigravity-tray-session-model-red.txt`.
