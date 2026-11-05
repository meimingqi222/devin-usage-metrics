# Agent Note: Windows release asset names avoid separator-normalized characters

Status: implemented

## Problem

Releases 0.3.14–0.3.16 published the Windows package as `Agent.Usage.Metrics-windows-x64.zip` even though `scripts/package-release.py`, the Release workflow and `tests/test_package_release.py` all produced and asserted `Agent Usage Metrics-windows-x64.zip`. The Windows build itself succeeded; publishing replaces separators in asset filenames, so the two spaces in the branded name became dots in the published asset. `internal/updater/updater.go` matched asset names exactly, so every Windows client looking at those releases reported `no matching asset for this platform` and could not update, while macOS — whose asset name has no spaces — was unaffected.

## Decision

The Windows release publishes two byte-identical archives: `Agent-Usage-Metrics-windows-x64.zip` as the branded primary and `devin-usage-metrics-windows-x64.zip` as a legacy-named alias, each with its own `.sha256` sidecar. Hyphens keep the branded name free of any character publishing rewrites. `internal/updater/updater.go` compares asset names through `assetKey`, which lowercases and folds spaces, dots and underscores to a single separator, so the dotted, spaced and hyphenated forms all resolve. Branded discovery precedes the legacy fallback, and `assetName` remains a platform gate for `installTarget` only.

The alias exists because release publishing rewrites separators in asset filenames, so a spaced branded name can never be published, and clients older than the rename match asset names exactly. `scripts/package-release.py` emits both archives and both sidecars; `.github/workflows/release.yml` uploads them with a `*.zip` / `*.sha256` glob so no per-target path list has to name the alias.

## Alternatives considered

**Keep the spaced name and only teach the updater about dots.** Rejected: it leaves the published name at the mercy of a normalizer outside this repository, and every future asset whose name gains a space silently breaks discovery again.

**Rename the packaged executable too.** Rejected: the user-facing executable name and installed path stay branded; only the archive needed a name that survives publishing.

**Match assets by substring or suffix.** Rejected: looser matching can select another platform's asset (the macOS archive shares the `-x64` suffix), and the regression test pins that rejection.

**Publish only the legacy name.** Rejected: it would let every old client update but drop the branding the user asked for, when a byte-identical alias costs one extra archive.

## Consequences

Clients at 0.3.14–0.3.16 still need this build (or a manual download) to update, because the name they expect does not exist in those releases; the separator-insensitive match is what makes those releases installable at all, and the legacy alias is what lets pre-rename clients update to this one. Each Windows release carries one extra ~9MB asset and sidecar. Anything outside the repository that hardcodes an archive name must accept `Agent-Usage-Metrics-windows-x64.zip` or the dotted form actually published. Packaging is covered by `tests/test_package_release.py`, which asserts both archives, both sidecars and their byte equality; asset discovery across separator variants and the rejection of other platforms' assets are covered by `internal/updater/updater_test.go::TestWindowsAssetDiscoverySurvivesSeparatorNormalization`, with branded-versus-legacy preference in `internal/updater/updater_test.go::TestWindowsReleaseNamesPreferBrandingAndAcceptLegacy`. Reverting the match to an exact string comparison fails those tests with `no matching asset for this platform`, which is the reported symptom.
