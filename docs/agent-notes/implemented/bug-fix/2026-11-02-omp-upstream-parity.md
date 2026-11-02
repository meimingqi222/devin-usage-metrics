# Agent Note: Keep OMP import aligned with upstream semantics for directories, zero cost, orchestration and timestamps

Status: implemented

## Problem

Four upstream-parity gaps undercounted or misplaced OMP usage:

1. Only `~/.omp/agent/sessions` was scanned, so installs using `PI_CONFIG_DIR`, `PI_CODING_AGENT_DIR`, named profiles or the XDG layout were invisible.
2. `usage.cost.total = 0` was stored and frozen as a free request. OMP zeroes usage/cost at stream start and only fills cost on success, so failed/aborted requests carried real tokens with a phantom $0; upstream treats a stored 0 as unknown and reprices.
3. `usage.orchestration` legs were dropped, although `totalTokens` and upstream billing include them.
4. Turns were bucketed by the JSONL envelope `timestamp` (write time) instead of `message.timestamp` (request time), which upstream prefers.

## Decision

- `internal/data/files.go::ompRoots` resolves all OMP session roots: `$PI_CODING_AGENT_DIR/sessions`, `<configRoot>/agent/sessions` (`configRoot` = `PI_CONFIG_DIR` or `~/.omp`), every `<configRoot>/profiles/<name>/agent/sessions`, `$XDG_DATA_HOME/omp/sessions`, plus the default `~/.omp/agent/sessions` alongside any override so pre-migration history still loads. `files()` already dedups by path.
- `internal/data/files.go::ompUsage` folds `usage.orchestration.input/output/cacheRead/cacheWrite` into the matching turn buckets for both messages and `model_usage`, and skips `cost.total <= 0` so `RecordedCost` stays nil and pricing falls through to the catalog.
- `internal/data/files.go::line` uses `message.timestamp` for OMP turns when present, falling back to the entry `timestamp`; `project_json.go` keeps `message.timestamp`.
- `internal/pricing/pricing.go::resolveLocked` routes OMP `devin/*` names to the Devin Pro table (previously only `devin/adaptive`), falling back to the API catalog when the Pro table lacks the model.

## Alternatives considered

**Zero-cost handling inside `pricing.Cost`/`Calculator.Cost`.** Rejected: other agents legitimately record explicit zero costs (confirmed free calls), and the recorded-cost contract is agent-agnostic. The parser is where "0 means unknown" is known to be true for OMP.

**Keeping upstreamModel only for display, pricing by requested model.** The existing design intentionally attributes each turn to its own upstream model; left unchanged pending a product decision (audit finding 2.6).

## Consequences

Custom OMP installs and profiles now load; a user pointing `PI_CONFIG_DIR` elsewhere still gets old `~/.omp` history. Failed/aborted OMP calls reprice from the catalog instead of $0 (when the model is priced; `devin/*` non-adaptive names now resolve through the Pro table). Turns may move into a different day/week/month bucket than before, matching upstream stats.

## Verification

- `internal/data/data_test.go::TestOmpCustomDirectories`
- `internal/data/data_test.go::TestOmpOrchestrationAndZeroCost`
- `internal/data/data_test.go::TestOmpUsesMessageTimestamp`
- `internal/pricing/pricing_test.go::TestOmpDevinModelsUseProTableWithApiFallback`

Proved: tests written against the fix fail on pre-fix code paths by construction (custom dirs load no sessions; zero recorded cost repriced; entry timestamp used). `go test -race ./internal/data ./internal/pricing -count=1` green; real `~/.omp` history still loads through the CLI.
