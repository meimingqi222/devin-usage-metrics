# Agent Note: Repair audited objects from verified local data

Status: implemented

## Problem

The daily audit detected missing or damaged objects, but unchanged exports kept reusing their references and never restored them.

## Decision

Export audits before returning a no-op or publishing a new manifest. A failed integrity check restores the exact object from a verified cache or deterministic local encoding, then reads it back and verifies size and hash. The current manifest itself is also checked and restored from its original hashed bytes, so immutable cache reuse cannot hide a missing server manifest forever. Read-only imports report failures. Expired references are excluded and audit stamps include the source hash.

## Alternatives considered

An unconditional immutable PUT cannot replace corrupt existing bytes. Uploading every object on every sync would undo incremental sync benefits.

## Consequences

Audited objects can recover without changing record identities. Repair requires local source data or a valid object cache; sampling does not detect every missing object immediately.

## Verification

- `src/sync/v3.rs::audit_repairs_missing_and_corrupt_objects_from_local_source`
- `src/sync/v3.rs::immutable_manifest_cache_reuses_verified_bytes_and_refetches_corruption`

Proved: Temporarily disabling the repair source made this test fail on the missing block. Restoring it passes missing, corrupt and oversized object cases for both turns and sessions; read-only checks still fail.
