# Agent Note: Agent-local routes price as origin; Grok ticks do not override the catalog

Status: implemented

## Problem

DimAgent stores models as `grok-build/grok-4.6` and `custom-provider/grok-4.6`. Those prefixes are product/plugin ids, not models.dev billing channels. Lookup required an exact catalog key, so CLI usage (no recorded cost) stayed unpriced while a handful of desktop runs with recorded catalog estimates still marked the day as priced. The usage page then showed ~85M tokens at $0.54.

Grok CLI writes a different identity (`grok-4.6-build`) and a `costUsdTicks` estimate. Treating ticks as an invoice undercounted API-equivalent cost (observed ratios ~0.085–0.5× list). The 2026-10-13 contract also treated any unknown `provider/model` prefix as unpriced, which hid origin quotes for local routes.

## Decision

Lookup still prefers an exact channel key. After normalize, date and GPT/Claude effort suffixes fail, a slash prefix that is **not** a catalog provider (no catalog key is that prefix or starts with `prefix/`) falls back to the remainder as an origin alias. Nested local prefixes recurse. A real catalog provider that does not list the model stays unpriced (`openai/claude-sonnet-5-5` does not inherit Anthropic).

`grok-*-build` is the same product as `grok-*` (identity, not a rate table), matching the existing `gk-` rewrite.

Grok `costUsdTicks` are an internal estimate. Catalog quotes win when `costWithPrice` can price the turn, not merely when lookup finds a row. A catalog model that cannot price this usage (for example `missing_cache_write` with cache writes) still keeps ticks. Ticks remain only when catalog pricing fails. Other agents keep recorded-cost precedence, including Dim desktop `quality=exact` totals and explicit Pi zeros.

## Alternatives considered

**Strip every unknown prefix, including known catalog providers missing this model.** That would bill a reseller channel at origin rates, which 2026-10-13 rejected.

**Keep `unknown-provider/claude-sonnet-5-5` unpriced as a sentinel.** The prefix is not a billing channel; leaving it unpriced also left DimAgent's `grok-build/` unpriced.

**Always prefer recorded cost, including Grok ticks.** Ticks are not an API invoice. Dim desktop recorded costs that match origin stay first because they are catalog estimates, not ticks.

**Hardcode `grok-build` / `custom-provider` allowlists.** Agent-local ids change; "not a catalog provider" is the channel test.

## Consequences

Unknown routing prefixes with a confirmed origin alias now show API-equivalent estimates. Distinct SKUs without an origin quote (`claude-opus`, `gpt-5.1-fast`) stay unpriced. Grok totals move from internal ticks toward list rates, except turns the catalog cannot price, which keep ticks. Dim CLI `grok-build/grok-4.6` prices as xAI `grok-4.6`, including the 200k context tier.

## Verification

- `internal/pricing/pricing_test.go::TestRequestBillingRegressions`
- `internal/pricing/pricing_test.go::TestGrokRecordedTicksYieldToCatalog`
