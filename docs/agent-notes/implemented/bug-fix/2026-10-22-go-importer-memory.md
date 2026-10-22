# Agent Note: Bound importer memory in the long-running tray process

Status: implemented

## Problem

With a large real history the Go desktop held about 2.3 GB resident after loading every agent for sync and peaked near 4 GB at startup; the Rust 0.2.11 tray held about 280 MB (peak about 390 MB) on the same data. A heap profile showed 96% of the retained heap was decoded JSONL lines kept for incremental reloads, mostly message bodies and tool output. Separately, the Devin and SQL-agent importers materialized whole result sets of full message rows (Devin peaked at 2.5 GB on its own).

## Decision

- `internal/data/database.go` streams Devin `message_nodes` and SQL-agent message rows through `sqlite.DB.Each` instead of collecting them.
- `internal/data/files.go` slims each decoded line before caching: strings over 1 KiB keep a copied prefix of at least 1 KiB and 101 non-space runes. Parsers read only identifiers, numbers, roles, short metadata and titles, and titles are `compact()`'s first 100 runes, so every parsed value is unchanged. The prefix is cloned; a substring would pin the whole body.
- The decoded-line cache has a 128 MiB estimated budget; beyond it the files modified longest ago are dropped and decoded again on the next reload.
- `internal/desktop/host.go` sets a 384 MiB soft memory limit unless `GOMEMLIMIT` is set.

## Alternatives considered

Dropping the decoded cache would re-decode every transcript whenever a snapshot expires. Keeping only keys the parsers read would shrink maps further, but needs an allowlist that silently breaks when a parser reads a new key (the Grok parser also iterates a map's keys). A hard limit could fail; the soft limit only makes the GC work harder.

## Consequences

On the measured history: retained heap 1.5 GB → 0.2 GB with byte-identical parse results for all ten agents (sessions including titles, turns, error counts); desktop footprint after loading all agents and sync collection 2.3 GB → 0.42 GB (peak 0.65 GB); Devin's load peak 2.5 GB → 0.1 GB. Very large histories re-decode their oldest transcripts on reload, costing a few seconds of background CPU. A parser that starts reading a long string beyond the kept prefix must revisit `slimThreshold`.

## Verification

- `internal/data/files_internal_test.go::TestSlimKeepsTitlesAndBlankness`
- `internal/data/files_internal_test.go::TestDecodedCacheEvictsLeastRecentlyModifiedBeyondBudget`

Proved: disabling string slimming failed the first test ("long string was not shortened"); disabling the budget failed the second ("over budget kept the oldest file"); both restored and rerun green. Output saved to `docs/agent-notes-evidence/go-memory-red.txt`.
