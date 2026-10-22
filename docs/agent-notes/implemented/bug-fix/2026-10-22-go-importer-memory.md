# Agent Note: Bound importer memory in the long-running tray process

Status: implemented

## Problem

With a large real history the Go desktop held about 2.3 GB resident after loading every agent for sync and peaked near 4 GB at startup; the Rust 0.2.11 tray held about 280 MB (peak about 390 MB) on the same data. A heap profile showed 96% of the retained heap was decoded JSONL lines kept for incremental reloads, mostly message bodies and tool output. Separately, the Devin and SQL-agent importers materialized whole result sets of full message rows (Devin peaked at 2.5 GB on its own).

## Decision

- `internal/data/database.go` streams Devin `message_nodes` and SQL-agent message rows through `sqlite.DB.EachBorrowedText` instead of collecting them.
- `internal/data/files.go` slims each decoded line before caching: strings over 1 KiB keep a copied prefix of at least 1 KiB and 101 non-space runes. Parsers read only identifiers, numbers, roles, short metadata and titles, and titles are `compact()`'s first 100 runes, so every parsed value is unchanged. The prefix is cloned; a substring would pin the whole body.
- The decoded-line cache has a 128 MiB estimated budget and evicts the least recently accessed file. Cache hits refresh recency: otherwise old but actively viewed history is repeatedly evicted after another agent loads. A single source larger than the entire budget is delivered to accounting without caching or flushing useful entries.
- Claude records use `internal/data/claude_json.go` to decode only the fields consumed by accounting. Entire lines are still validated, including ignored metadata; numeric overflow falls back to the original decoder. Object keys are case-sensitive, the last duplicate key wins, titles retain the first nonblank top-level text block, and retained values own their bytes. Parser changes must update this projection and its differential tests together.
- `internal/desktop/host.go` sets a 384 MiB soft memory limit unless `GOMEMLIMIT` is set.

## Alternatives considered

Dropping the decoded cache would re-decode every transcript whenever a snapshot expires. Keeping only keys the parsers read requires an allowlist synchronized with the parser. This now covers Claude, Codex, Pi, WorkBuddy, Grok envelopes, Devin message metadata and SQL-agent message JSON. Grok usage maps and other dynamic usage leaves remain complete, including arbitrary model keys. Schemas must track every field consumed by accounting; differential fuzzing compares against generic decoding. A hard limit could fail; the soft limit only makes the GC work harder.

## Consequences

On the measured history: retained heap 1.5 GB → 0.2 GB with byte-identical parse results for all ten agents (sessions including titles, turns, error counts); desktop footprint after loading all agents and sync collection 2.3 GB → 0.42 GB (peak 0.65 GB); Devin's load peak 2.5 GB → 0.1 GB. Histories exceeding the budget re-decode their least recently used transcripts. In the later Claude fixture, a 128 MiB transcript shrank from about 396 MiB decoded weight to 9 MiB, and all 17 Claude files occupy about 35 MiB; warm append reloads are about 104–119 ms. A parser that starts reading a long string beyond the kept prefix must revisit `slimThreshold`.

## Verification

- `internal/data/files_internal_test.go::TestSlimKeepsTitlesAndBlankness`
- `internal/data/files_internal_test.go::TestDecodedCacheEvictsLeastRecentlyUsedBeyondBudget`

Proved: disabling string slimming failed the first test ("long string was not shortened"); disabling the budget failed the second ("over budget kept the oldest file"); both restored and rerun green. Output saved to `docs/agent-notes-evidence/go-memory-red.txt`.

Additional regression bindings for selective decoding and cache policy:

- `internal/data/claude_json_test.go::TestClaudeProjectionDiscardsNestedTranscripts`
- `internal/data/files_internal_test.go::TestDecodedCacheKeepsRecentlyReadHistory`
- `internal/data/files_internal_test.go::TestOversizedDecodedFileDoesNotFlushCache`

Proved: restoring full JSON materialization failed the retained-memory assertion; restoring mtime eviction failed the hot-history assertion; disabling oversized-entry rejection failed the cache-preservation assertion. All guards were restored and their focused tests passed. Evidence: `docs/agent-notes-evidence/claude-projection-red.txt`, `docs/agent-notes-evidence/decoded-cache-policy-red.txt`.

Additional parity coverage: `internal/data/claude_json_test.go::TestClaudeProjectionMatchesFullAccounting` and `internal/data/claude_json_test.go::FuzzClaudeProjection`. The latter compares consumed fields, title selection, error presence and input-buffer ownership against generic decoding.

Additional coverage: `internal/data/project_json_test.go::FuzzAgentJSONProjection` checks all shared schemas, including dynamic usage leaves. `internal/data/proto_test.go::FuzzProtoLookup` compares Antigravity field lookup to the former materializing decoder; `internal/data/proto_test.go::TestProtoLookupDoesNotAllocate` pins allocation-free lookup. Title projections retain input prefixes, because applying compact twice can remove a trailing space at the 100-rune boundary.
