# Agent Note: Repair audited objects from verified local data

Status: implemented

## Problem

The daily audit detected missing or damaged objects, but unchanged exports kept reusing their references and never restored them.

## Decision

Export audits before returning a no-op or publishing a new manifest. The rules below describe the Rust reference; the Go client follows them as stated at the end. A failed integrity check restores the exact object from a verified cache or deterministic local encoding, then reads it back and verifies size and hash. The current manifest itself is also checked and restored from its original hashed bytes, so immutable cache reuse cannot hide a missing server manifest forever. Read-only imports report failures. Expired references are excluded and audit stamps include the source hash.

Go (`internal/syncclient/publish.go`): on the first publication of a process and then daily, a full listing removes references to missing objects from the parent manifest before reuse, so their groups are encoded and uploaded again. Objects the (audited) parent manifest still references are not probed. Other objects use one `If-None-Match: *` PUT; a 412 carrying the stored ETag proves the object intact without a download, and a mismatch reads it back and replaces it (`transport.go::putImmutable`). Every cycle a HEAD checks the stored ETag of this device's current manifest, since other devices read it; a missing or damaged copy is restored from verified cached bytes, and without them a fresh download reports the damage. The Go audit checks existence only; block contents rely on the server's name-hash check at upload.

## Alternatives considered

An unconditional immutable PUT cannot replace corrupt existing bytes. Uploading every object on every sync would undo incremental sync benefits.

## Consequences

Audited objects can recover without changing record identities. Repair requires local source data or a valid object cache; sampling does not detect every missing object immediately.

## Verification

- `src/sync/v3.rs::audit_repairs_missing_and_corrupt_objects_from_local_source`
- `src/sync/v3.rs::immutable_manifest_cache_reuses_verified_bytes_and_refetches_corruption`
- `internal/syncclient/efficiency_test.go::TestPublishSendsOnlyNewObjectsAndRepairsMissingOnes`
- `internal/syncclient/client_test.go::TestCycleV1MigrationReuseAndReadOnlyMerge`

Proved: Temporarily disabling the repair source made this test fail on the missing block. Restoring it passes missing, corrupt and oversized object cases for both turns and sessions; read-only checks still fail.

Go proof: disabling the audit, the manifest repair source, and the 412 ETag check each failed its bound assertion (missing referenced block, manifest hash mismatch, existing objects downloaded); all restored and rerun green. Output saved to `docs/agent-notes-evidence/go-sync-audit-red.txt`.

Server duplicate-upload refinement: after body/name hash validation, create-only immutable PUT checks the existing object under the tenant lock before writing a temporary file. A rejection returns the stored ETag (including damaged content), not the name hash. Missing objects and If-Match repairs retain the final locked CAS and quota checks after fsync. This avoids redundant disk writes without weakening repair semantics.

- `sync-server/performance_test.go::TestExistingImmutableUploadDoesNotTouchDirectory`

Proved: disabling the early check failed the directory-mtime assertion because rejected uploads created and deleted temporary files; the restored production path passed, including damage, repair and invalid-body cases. Evidence: `docs/agent-notes-evidence/server-duplicate-upload-red.txt`.
