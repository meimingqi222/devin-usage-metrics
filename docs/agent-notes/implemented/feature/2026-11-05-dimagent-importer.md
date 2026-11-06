# Agent Note: DimAgent local usage importer

Status: implemented

## Problem

DimAgent stores usage in two product homes with different schemas. The alpha CLI (`dima`) writes `~/.dimcode-alpha/rust/dimcode.sqlite` timeline events; the desktop app writes `~/.dimcode/v2/dimcode.sqlite` (`usage_run_stats` / `usage_ledger`). Treating them as two agents would split one product; reading only the CLI home hides desktop sessions.

## Decision

One agent: `model.Dim` (`dim`, label DimAgent). Both homes are scanned when present. Session keys keep the source (`dimcode-alpha/…`, `dimcode-desktop/…`) so CLI and desktop rows never collide.

- **CLI.** `parseDimCLI` reads `model/call` + `model/usage` on branch `main`. One turn per `request_id`. Input is `promptTokens - cacheReadTokens`.
- **Desktop.** `parseDimDesktop` prefers `usage_run_stats` (one turn per `runId`). `plugin_` ledger rows for the same run are ignored; ledger is only a fallback when run stats are absent. `inputTokens` on run stats is the prompt total (includes cache), same split as CLI. Recorded cost is `cost.totalCostUsd` when present unless `quality` is `missing_price` (unknown, not a free request). Explicit `quality=exact` zeros stay recorded.

Empty sessions with no usage are omitted. Snapshot version 9 rebuilds caches that treated `missing_price` zeros as billed $0. A mixed CLI+desktop snapshot is not reused as a single remaining database; dropping the desktop file must not keep those sessions.

## Alternatives considered

Two sidebar agents (dima vs DimAgent). Same company and product name; users would think they installed two tools. Per-call desktop rows from `messages` are not a usage log.

## Consequences

Failed compaction without `model/usage` is still omitted. Desktop granularity is per run, not per inner model call.

## Verification

- `internal/data/data_test.go::TestDimModelUsageTurns`
- `internal/data/data_test.go::TestDimDesktopRunStatsIgnoresPluginLedger`
- `internal/data/data_test.go::TestDimDesktopMissingPriceIsNotRecordedZero`
- `internal/data/data_test.go::TestDimDesktopLedgerSkipsPluginWhenRunExists`
- `internal/data/data_test.go::TestDimLoadsCLIAndDesktopTogether`
- `internal/data/data_test.go::TestDimDroppingDesktopDoesNotKeepStaleSessions`
