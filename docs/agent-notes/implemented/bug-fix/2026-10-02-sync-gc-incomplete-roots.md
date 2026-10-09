# Agent Note: Abort GC when the manifest graph cannot be verified

Status: implemented

## Problem

GC ignored unreadable or malformed roots and continued sweeping, deleting referenced blocks when a head or manifest was damaged.

## Decision

GC completes root marking before deletion and aborts on unreadable heads, missing manifests, hash mismatches, malformed JSON or invalid references. Current and previous manifests and recent upload manifests remain roots. Retention and legacy migration rules remain in effect.

## Alternatives considered

Skipping only the bad root is unsafe because its block references are unknown. Disabling all GC permanently would prevent recovery of quota space.

## Consequences

Corrupt roots prevent cleanup for that tenant until repaired. Other tenants continue independently.

## Verification

- `sync-server/main_test.go::TestGCStopsOnCorruptRoots`

Proved: With the original GC, corrupt heads deleted two objects and corrupt or missing manifests deleted one object without an error. After the fix, all corrupt-root cases return an error and delete zero objects. Tests also cover valid JSON with a wrong hash and correctly hashed invalid manifest structures.
