# Agent Note: Price Devin usage with the official Pro table

Status: implemented

## Problem

Enterprise rates do not represent Self-serve/Pro billing. Calendar overrides also became stale when SWE-2's free promotion was extended: a zero quote was replaced by nonzero list rates. The former GPT-5.6 name checks, 272K threshold and fixed long-context rates likewise overrode current quotes and omitted other published tiered models.

## Decision

Use the official models.md `modelCostData`, filtered strictly to `TEAMS_TIER_PRO`. Preserve explicit zero prices. No promotion dates, free-model whitelist or fallback list rates override the selected quote. The user explicitly chose current-price estimates for all dates; valid recorded costs still take precedence in aggregation.

Extract the same document's `longContextFamilies` and `longContextModelFamilies` object literals using JSON5 without executing JavaScript. Their explicit model mapping, threshold and rates populate `Pricing.tiers`, shared with the API pricing engine. Exact Fast variants only receive tiers if mapped by the source. An omitted tier cache-write rate inherits the base rate. Malformed or incomplete published tables reject the refresh rather than partially replacing prices.

The embedded Pro snapshot is `internal/pricing/devin-pro-pricing.json`, produced by the extractor that once lived in the Rust examples. Successful refreshes replace the memory table and atomically cache the raw source for 24 hours. Failures preserve the last good quote and use a one-hour retry backoff. A fresh but invalid cache cannot suppress refreshing. The GUI checks expiry hourly in one background worker; CLI checks before aggregating. New prices affect the next aggregation. Missing Pro models never fall back to enterprise rates.

## Alternatives considered

Moving the hardcoded deadline whenever a promotion is extended repeats the original failure. Interpreting zero as missing invents fees. Copying API rates into the Pro table confuses billing channels. Using family-name substring tests cannot reliably distinguish paid Fast/Lightning products. Evaluating document JavaScript is unnecessary for static data. Applying the current quote to historical usage is an estimate, not a reconstructed historical invoice; the current source supplies no historical effective dates.

## Consequences

Devin estimates follow the current Pro source, including future free/paid transitions and tier changes. Historical estimates may change after a refresh, as explicitly selected by the user. Account-specific pricing, historical invoices and special voice-call billing cannot be inferred from the public catalog. Offline usage retains the last valid cache or bundled snapshot. Source syntax changes that cannot be parsed retain valid cached data. API estimates stay on their independent provider catalog.

## Verification

- `internal/pricing/pricing_test.go::TestAdaptiveBillingIgnoresDisplayModelAndRecordedEstimate`
- `internal/pricing/pricing_test.go::TestOmpDevinModelsUseProTableWithApiFallback`
- `internal/pricing/calculator_test.go::TestCalculatorMatchesRequestPricing`
- `internal/pricing/pricing_test.go::TestRequestBillingRegressions`

The Go pricing contract lives in `internal/pricing`; the Rust generator and its unit tests were removed with the old tree. The embedded Pro snapshot is now `internal/pricing/devin-pro-pricing.json` consumed by `internal/pricing/pricing.go`.

Proved: Before removing calendar overrides, the current-zero-quote test returned Some(18.3) instead of Some(0.0); see `docs/agent-notes-evidence/2026-10-20-online-pro-quote-red.txt`. Before extracting the online context maps, the future-model test retained (1, 2, 0.1, 3) instead of the published (7, 9, 0.7, 3); see `docs/agent-notes-evidence/2026-10-20-online-pro-tiers-red.txt`. Temporarily removing fresh-cache validation made the HTTP integration test fail at refresh_devin_pro(false) after writing corrupt cache data; restoring the guard passes, recorded in `docs/agent-notes-evidence/2026-10-20-pro-invalid-cache-red.txt`. The integration test also changes an SWE quote from zero to paid and back, moves an existing GPT tier to 400K with input rate 13, keeps Lightning paid, and rejects malformed tier data without overwriting good quotes.

Live audit on 2026-10-20 independently compared all 296 Pro base quotes and 86 context mappings against the generated offline snapshot with zero mismatches. Details and reproduction are in docs/reports/pricing-audit-2026-10-20.md.
