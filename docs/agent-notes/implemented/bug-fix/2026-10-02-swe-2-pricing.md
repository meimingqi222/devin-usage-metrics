# Agent Note: Price Devin usage with the official Pro table

Status: implemented

## Problem

The main workspace omitted recently added SWE-2 prices, but restoring enterprise rates did not meet the selected Self-serve/Pro billing plan. Official Pro data explicitly lists SWE-2 as free through October 15, 2026. Zero prices must be retained rather than treated as missing rates or replaced by enterprise prices.

## Decision

Use a separate Pro table for Devin turns, populated from the official models.md modelCostData JSON array and filtered strictly to TEAMS_TIER_PRO. The official Pro snapshot is the first-launch offline fallback. Successful network refreshes replace the table and are atomically cached for 24 hours. Bad responses preserve the last good table and use a one-hour retry backoff. GUI checks in the background; CLI checks before aggregating. Missing Pro models do not fall back to enterprise rates. The GUI labels Devin costs USD / Pro and distinguishes known free prices from unknown model prices in model details.

Use each turn's UTC date for the published SWE-2 promotion: zero through October 15 inclusive, followed by the documented list prices if the cached snapshot still contains zero. This keeps earlier promotional turns free after refresh. Existing GPT-5.6 long-context handling stays per turn, with Sol rates corrected to the current official document.

## Alternatives considered

Using enterprise rates produces incorrect Self-serve costs. Dropping zero-price rows loses the distinction between free and unknown. Reading only an embedded snapshot requires application releases for price updates. Using credit multipliers confuses the legacy enterprise credits plan with current Pro token billing.

## Consequences

Only Devin aggregation switches to the Pro source; other agent pricing is unchanged. Values remain token-based USD estimates rather than monthly subscription charges. Public current prices are not a historical ledger; the explicitly documented SWE promotion is date-aware. Account-specific pricing and special voice-call billing are not inferred from local token records. Official document format changes fail closed to cached data. New prices affect the next aggregation, not a view that has already been calculated.

## Verification

- `src/agg.rs::swe_2_adaptive_models_use_pro_free_promotion`
- `src/pricing.rs::pro_prices_keep_zero_and_do_not_use_enterprise_fallback`
- `src/pricing.rs::swe_pro_promotion_uses_turn_date_and_expiry`
- `src/pricing.rs::official_document_parser_filters_tiers_and_validates_rates`
- `tests/devin_pro_pricing.rs::pro_refresh_preserves_free_prices_and_keeps_cache_on_bad_response`

Proved: Temporarily routing Pro lookup to the enterprise lookup made the Pro source test fail (0.75 versus expected 0). Restoring Pro lookup passed. The HTTP integration test retains free rows, verifies cached document content and fresh-cache network avoidance, and proves an invalid refresh cannot overwrite good disk or memory data. The actual official document was downloaded on October 2, 2026 and contained 269 Pro entries.
