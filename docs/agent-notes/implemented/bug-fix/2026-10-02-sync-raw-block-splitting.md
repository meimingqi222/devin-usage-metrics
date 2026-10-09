# Agent Note: Split compressed records using both size limits

Status: implemented

## Problem

Highly repetitive records compress below the target block size but can exceed the 8 MiB decoded limit. Export rejected a divisible group instead of splitting it.

## Decision

Turn and session groups are accepted only when both decoded and compressed sizes fit. Otherwise they split recursively; an indivisible oversized record still returns an error.

## Alternatives considered

Raising the decoded limit would weaken memory bounds and merely postpone the same failure.

## Consequences

Highly compressible groups create more blocks while retaining every record and the existing wire format.

## Verification

- `src/sync/v3.rs::highly_compressible_records_split_at_raw_limit`

Proved: Before the fix, twelve repetitive records failed with the decoded 8 MiB limit error. After the fix, turns and sessions split, preserve all twelve records and decode within the limit.
