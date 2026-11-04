# Agent Note: Use Agent Usage Metrics for Windows release filenames

Status: implemented

## Problem

The native UI and installer use Agent Usage Metrics, but the release ZIP and its executable still use devin-usage-metrics to accommodate the old Rust updater. The user explicitly requested the Windows package use Agent Usage Metrics and stop publishing the old filename.

## Decision

Windows releases are `Agent Usage Metrics-windows-x64.zip` with exactly `Agent Usage Metrics.exe` inside; the checksum sidecar and release workflow use the same archive name. `scripts/package-release.py` and `.github/workflows/release.yml` follow that contract. macOS artifact names, repository/module identifiers, config directories and device/sync identities are unchanged.

`internal/updater/updater.go` prefers the branded Windows release asset independently of GitHub asset ordering. It accepts the legacy asset when a historical release has no branded asset. Extraction and apply continue accepting historical executable names; newly produced ZIPs include no legacy alias. Updating an existing installation retains its currently running filename, while fresh packages use the branded executable.

## Alternatives considered

Publishing both names would preserve old Rust auto-updates but contradict the user's request. Changing only the EXE in the ZIP would leave release assets under the old product name. Dropping the new client's legacy-reader support would unnecessarily prevent it from reading older releases.

## Consequences

Already-released Rust clients and older Go clients that only discover the old asset name need a one-time manual upgrade. This source change cannot alter those installed clients. Installer naming already follows the branded name. Package naming and checksum behavior are covered by `tests/test_package_release.py`; discovery preference, legacy fallback and executable extraction are covered by `internal/updater/updater_test.go::TestWindowsReleaseNamesPreferBrandingAndAcceptLegacy` and `internal/updater/updater_test.go::TestReleaseDownload`. The Python packaging regression runs in CI and Release before the real package is created. Native installed update/restart acceptance remains separate from archive and HTTP-fixture checks.
