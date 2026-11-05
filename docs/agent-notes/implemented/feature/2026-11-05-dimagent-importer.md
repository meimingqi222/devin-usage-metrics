# Agent Note: DimAgent local usage importer

Status: implemented

## Problem

DimAgent stores turns in `~/.dimcode-alpha/rust/dimcode.sqlite` (stable: `~/.dimcode/rust/dimcode.sqlite`) as timeline events, not JSONL. Without an importer those sessions are invisible in Agent Usage Metrics.

## Decision

`model.Dim` (`dim`, label DimAgent) is the twelfth local agent. `parseDim` reads `agent_sessions` plus `model/call` and `model/usage` rows on branch `main`. Each `request_id` becomes one turn: `promptTokens - cacheReadTokens` as input, `completionTokens` as output, `cacheReadTokens` as cache reads. The model id comes from the matching `model/call` (`providerId/modelId`). Titles and cwd come from `session_metadata` / `session_catalog`. Both product-home databases are scanned when present. No Rust counterpart.

## Alternatives considered

Treating Dim as Devin sessions.db would miss the timeline event schema. Counting `context/budget` estimates would invent tokens that were never billed.

## Consequences

Missing Dim databases surface as the usual source-not-found error. Compacted or failed model calls still count when a `model/usage` event exists.

## Verification

- `internal/data/data_test.go::TestDimModelUsageTurns`
