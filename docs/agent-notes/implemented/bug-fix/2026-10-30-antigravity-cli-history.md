# Agent Note: Include Antigravity CLI conversations in history

Status: implemented

## Problem

Google Antigravity keeps desktop-app conversations in `~/.gemini/antigravity/conversations/*.db` and CLI conversations in a separate `~/.gemini/antigravity-cli/conversations/*.db` store. Both use the same SQLite/protobuf schema. The importer only scanned the desktop path, so every CLI session, turn and token was missing from usage totals. On a real machine the CLI store held 8 sessions and 148 turns while the app store held 66 sessions.

## Decision

- `internal/data/database.go` scans both Antigravity roots with the existing `files`/`parseAG` path. No schema change was needed.
- `parseAG` records the writer as `Source: "app"` or `Source: "cli"`. Desktop sessions keep their existing `antigravity/<uuid>` keys; CLI sessions use `antigravity-cli/<uuid>` so a conversation UUID shared by both stores can never merge or double-count them.
- Totals stay merged under the single Antigravity agent because both clients use the same models, subscription and quota. The session list adds a small `CLI` tag, and session details show the source; there is no separate sidebar agent.
- `internal/desktop/layout.go` treats either directory as an installed Antigravity source.
- `snapshotVersion` is bumped to 4 so snapshots produced before the second source existed are rebuilt instead of served for five minutes.

## Alternatives considered

**A separate Antigravity CLI agent.** It would fragment one subscription's usage across two sidebar entries, require a new label/color/sync agent name, and make the usage cards disagree with the quota card. Source-level distinction gives the same traceability without splitting totals.

**Reusing `antigravity/<uuid>` for CLI sessions.** The UUIDs are globally unique today, but sharing a key would silently merge two sessions if the clients ever expose the same conversation. Namespacing costs nothing because CLI keys are new.

**Inferring the source from the filename.** Conversation files are bare UUIDs in both stores; the directory is the only reliable signal.

## Consequences

Antigravity usage and session lists now include CLI history. Existing desktop session keys and their sync identities are unchanged, so previously published data is not duplicated. Desktop sessions change `Source` from `local` to `app`, which changes the content hash but not protocol identity. Old parsed snapshots rebuild once. The Rust reference still reads only the app store, so Go/Rust differential scripts intentionally do not include CLI data.

## Verification

- `internal/data/data_test.go::TestAntigravityIncludesCLIStore`
- `internal/desktop/sessions_visual_test.go::TestAntigravityCLISourceTag`

Proved: removed the CLI root → `internal/data/data_test.go::TestAntigravityIncludesCLIStore` failed with 1 of 2 turns; removed the source tag → `internal/desktop/sessions_visual_test.go::TestAntigravityCLISourceTag` failed; both guards restored and re-run green. Output saved to `docs/agent-notes-evidence/antigravity-cli-source-red.txt` and `docs/agent-notes-evidence/antigravity-cli-tag-red.txt`.
