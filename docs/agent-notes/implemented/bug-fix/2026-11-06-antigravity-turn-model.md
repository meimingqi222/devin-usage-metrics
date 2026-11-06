# Agent Note: Stamp Antigravity session model onto turns

Status: implemented

## Problem

`parseAG` never set `Turn.Model`. Usage protobuf field 1 is a numeric enum, not a model string, so every Antigravity turn left the importer with an empty model. Analytics, CLI and session costs already fell back to the session's `SelectedModel` (`gemini-3-flash-a`, `gemini-4-argon-trial`, …). The tray priced `pricing.Cost(turn, turn.Model)` and compaction skipped empty-model turns, so local Antigravity history (thousands of turns, billions of cache-read tokens) showed as free in the menu while the usage page billed it.

## Decision

On emit, `internal/data/database.go::parseAG` stamps `t.Model = s.SelectedModel` from `gen_metadata`. That is the same session-level label analytics already used; per-turn model IDs are not in the protobuf as strings. `internal/data/data.go` `snapshotVersion` is 10 so cached turns without a model rebuild.

`internal/desktop/tray.go::trayEntriesOf` still falls back to `session.DisplayModel()` when a turn's model is empty, matching analytics/CLI. That covers old snapshots, remote sync payloads, and tests that construct Antigravity turns without a session model.

## Alternatives considered

**Leave turns empty and teach every Cost caller to pass the session model.** Analytics and CLI already did; the tray did not. A later Cost site (compaction, tray, CLI) will miss the fallback again.

**Decode usage field 1 as the model.** Fixture and live blobs store a varint enum there, not `gemini-…`. Treating it as a string leaves the model empty.

**Map each `gen_metadata` row onto steps by index.** Session `SelectedModel` is already the last selected model and matches current analytics/CLI billing. Per-step mapping would change totals without a stored per-turn string.

## Consequences

Antigravity tray totals and compaction become priced after the snapshot rebuilds. Mid-session model switches still bill every turn at the session's last selected model, same as before. Turns whose session has no `SelectedModel` stay unpriced.

## Verification

- `internal/data/data_test.go::TestAntigravityFullUsageSuppressesEcho`
- `internal/desktop/tray_test.go::TestTrayAntigravitySessionModelPricesEmptyTurns`

Proved: removed `t.Model = s.SelectedModel` → `TestAntigravityFullUsageSuppressesEcho` failed at the session-model assertion; restored `trayEntriesOf` to `pricing.Cost(turn, turn.Model)` → `TestTrayAntigravitySessionModelPricesEmptyTurns` failed at the priced-entry assertion; both guards restored and re-run green. Output saved to `docs/agent-notes-evidence/antigravity-turn-model-red.txt` and `docs/agent-notes-evidence/antigravity-tray-session-model-red.txt`.
