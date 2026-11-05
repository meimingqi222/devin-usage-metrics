# Agent Note: Windows release asset names avoid separator-normalized characters

Status: implemented

## Problem

Releases 0.3.14–0.3.16 published the Windows package as `Agent.Usage.Metrics-windows-x64.zip` even though `scripts/package-release.py`, the Release workflow and `tests/test_package_release.py` all produced and asserted `Agent Usage Metrics-windows-x64.zip`. The Windows build itself succeeded; publishing replaces separators in asset filenames, so the two spaces in the branded name became dots in the published asset. `internal/updater/updater.go` matched asset names exactly, so every Windows client looking at those releases reported `no matching asset for this platform` and could not update, while macOS — whose asset name has no spaces — was unaffected.

## Decision

The Windows release archive is `Agent-Usage-Metrics-windows-x64.zip`: hyphenated so it carries the branding without any character that publishing rewrites. `scripts/package-release.py`, the Release workflow matrix and `tests/test_package_release.py` produce and assert that name; the archive still contains exactly `Agent Usage Metrics.exe`, so the installed executable name is unchanged. `internal/updater/updater.go` additionally compares asset names through `assetKey`, which lowercases and folds spaces, dots and underscores to a single separator, so a release published as `Agent.Usage.Metrics-windows-x64.zip`, `Agent Usage Metrics-windows-x64.zip` or the hyphenated form all resolve to the same asset. Branded discovery still precedes the legacy `devin-usage-metrics-windows-x64.zip` fallback, and `assetName` remains a platform gate for `installTarget` only.

## Alternatives considered

**Keep the spaced name and only teach the updater about dots.** Rejected: it leaves the published name at the mercy of a normalizer outside this repository, and every future asset whose name gains a space silently breaks discovery again.

**Rename the packaged executable too.** Rejected: the user-facing executable name and installed path stay branded; only the archive needed a name that survives publishing.

**Match assets by substring or suffix.** Rejected: looser matching can select another platform's asset (the macOS archive shares the `-x64` suffix), and the regression test pins that rejection.

## Consequences

Clients at 0.3.14–0.3.16 still need this build (or a manual download) to update, because the name they expect does not exist in those releases; the separator-insensitive match is what makes those releases installable at all. The archive name changed for a fourth time, so anything outside the repository that hardcodes `Agent Usage Metrics-windows-x64.zip` — release notes, download mirrors, third-party scripts — must use the hyphenated name or the dotted form actually published. Packaging is covered by `tests/test_package_release.py`; asset discovery across separator variants and the rejection of other platforms' assets are covered by `internal/updater/updater_test.go::TestWindowsAssetDiscoverySurvivesSeparatorNormalization`, with the branded-versus-legacy preference in `internal/updater/updater_test.go::TestWindowsReleaseNamesPreferBrandingAndAcceptLegacy`. Reverting the match to an exact string comparison fails those tests with `no matching asset for this platform`, which is the reported symptom.
