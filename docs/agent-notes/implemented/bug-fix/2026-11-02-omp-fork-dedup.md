# Agent Note: omp dedups globally by (entry id, timestamp) so forked sessions cannot double count

Status: implemented

## Problem

OMP `createBranchedSession` / fork deep-copies the parent transcript into a new session file with a new session id but identical entry ids and `responseId`s. The importer deduplicated turns per session (`SessionKey + responseId`), so every copied turn was counted once in the parent session and again in the fork. A fork inflated tokens, cost and turn count; the inflation grows with each branch.

## Decision

In `internal/data/files.go::account`, omp keeps its per-session dedup on
`SessionKey + responseId` (a retry that rewrites a response under a new
entry id still dedups within the session), then adds a second, global check
on `(DedupKey, CreatedAt)` — the `(entry_id, timestamp)` pair OMP's own
stats database uses for its fork guard, first-write-wins. A copied entry
has the same id and entry timestamp in both files, so the second file's
copy is skipped; the fork's own later turns carry new ids/timestamps and
count normally.

Attribution is decided by basename order, not file mtime: transcript names
start with the creation timestamp, and OMP forks are always created after
their parent, so the parent claims the copied entry. Pi/WorkBuddy/Codex
keep per-session dedup; Claude's `latest` merge is unchanged.

## Alternatives considered

**Global dedup on `responseId` alone (my first pass).** File processing
order is newest-mtime-first, so a newer fork file claimed the copied turn
and the parent's copy was skipped — attribution and totals depended on
which file happened to be touched last; the test passed or failed on mtime
timing. The `(id, timestamp)` pair matches upstream semantics regardless
of file order.

**Keep-last within a file.** `TestOmpTranscript` pins that a repeated
`responseId` can carry a smaller second usage record (interim values), so
the first record must survive; keep-last also collapsed all messages with
an empty `DedupKey` into one.

## Consequences

Session summaries and buckets charge a copied turn once, attributed to the
parent session. A fork's own follow-up turns still count. A turn sharing
both `responseId` and entry timestamp for a genuinely different billed
call would undercount; OMP ids/timestamps are unique per response, so this
is theoretical.

## Verification

- `internal/data/data_test.go::TestOmpForkCopiesDeduped`
- `internal/data/data_test.go::TestOmpResumedSessionDeduped`
- `internal/data/data_test.go::TestOmpTranscript`

Proved: pre-fix code counted both the parent and the fork copy (`want 2
turns` → both sessions each held the copied turn); output saved to
`docs/agent-notes-evidence/omp-fork-dedup-red.txt`. Guard restored,
`go test -race ./internal/data` green.

Proved: pre-fix code counted both the parent and the fork copy (`want 2 turns` → both sessions each held the copied turn); output saved to `docs/agent-notes-evidence/omp-fork-dedup-red.txt`. Guard restored, `go test -race ./internal/data` green.
