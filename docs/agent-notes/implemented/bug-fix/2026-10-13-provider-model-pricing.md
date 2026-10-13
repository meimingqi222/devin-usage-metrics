# Agent Note: Preserve provider identity when resolving model prices

Status: implemented

## Problem

Flattening every models.dev provider into one model-name map lets later reseller quotes overwrite origin prices. The reported Claude daily usage had identical token counts to ccusage but cost 27.85178730 instead of 23.31790880: Sonnet cache reads were priced at 0.20 instead of 0.10 per million. The catalog contains 597 conflicting bare names and 98 conflicts between deployments within one provider. Generic prefix/reverse-prefix lookup, lost routing prefixes, omitted context tiers, missing cache rates treated as zero, and stale aliases compound the problem.

## Decision

Preserve provider, complete model ID, and provenance in the API catalog. Resolve exact channel IDs before normalization. Bare defaults require confirmed origin quotes or agreement between candidates; unknown all-zero placeholders do not become free defaults. Canonical identity helps locate an origin but cannot turn a reseller quote into an origin quote. Keep small family identity rules for incomplete metadata and global/regional APIs, never hardcoded rates. Preserve complete deployment IDs and reject conflicting defaults. Restrict inherited effort suffixes to known GPT/Claude families; unknown Fast, Pro, free and version variants have no inferred price.

API usage uses find_api, independently of Devin's Pro source and legacy enterprise inspection. Preserve explicit context tiers and select them per full request prompt, including cache reads/writes. Preserve unspecified cache-rate information and return unknown when actual cache usage cannot be priced. The compression replay uses the same API source and resolves rates for each simulated call. Replace complete catalogs rather than extending stale aliases; reject legacy flattened caches lacking provenance while retaining last good data after failed refreshes. Regenerate the offline snapshot through the same extractor.

Fix Claude empty/partial cache breakdowns, MimoCode's lost provider prefixes, and Pi's missing-cost-as-free behavior. Preserve positive OpenCode recorded costs in both database schemas, matching MimoCode's treatment of zero placeholders. Increment the usage cache schema to 13 so old parser results cannot remain the incremental baseline. Valid recorded costs, including explicit Pi zero, keep precedence; invalid recorded values cannot be summed.

## Alternatives considered

Ordering providers or choosing the cheapest reseller still conflates billing channels. Canonical IDs identify models, not sellers. Dropping all zero quotes breaks known free channel prices; interpreting missing rates as zero undercounts usage. Loose prefix matching prices distinct products as their base models. Merging refreshed maps retains defaults that became ambiguous. Reusing old flattened caches restores the original bug after upgrading.

## Consequences

More models correctly appear unpriced when their channel or cache rate cannot be established. Explicit channels retain their quotes, including valid zero prices. Public current prices remain API-equivalent estimates rather than account invoices or a historical price ledger. The complete offline snapshot is larger but preserves provider identity. API context tiers now apply to API aggregation instead of being limited to Devin's separately published rules. Synthetic Codex replay fixtures use ordinary input/cache reads rather than fictional Claude TTL writes, preserving their total prompts. See docs/pricing-audit-2026-10-13.md for catalog counts, policy and billing limits.

## Verification

- `src/pricing.rs::anthropic_prices_win_over_resellers_for_claude`
- `src/pricing.rs::catalog_keeps_provider_prices_and_rejects_ambiguous_defaults`
- `src/pricing.rs::unknown_model_variants_do_not_inherit_a_different_price`
- `src/pricing.rs::full_ids_distinguish_deployments_within_one_provider`
- `src/pricing.rs::exact_provider_ids_win_before_normalization_and_max_is_not_universal`
- `src/pricing.rs::context_tiers_use_full_prompt_and_keep_unspecified_rates`
- `src/pricing.rs::malformed_or_unspecified_rates_are_not_assumed_free`
- `src/pricing.rs::api_lookup_preserves_channels_and_never_uses_devin_or_unknown_variants`
- `src/pricing.rs::canonical_identity_finds_an_origin_without_using_a_reseller_quote`
- `src/pricing.rs::cached_file_loads_both_flat_and_nested_shapes`
- `src/agg.rs::api_and_devin_apply_their_own_long_context_tiers`
- `src/compaction.rs::replay_uses_each_simulated_calls_context_tier`
- `src/local_sources.rs::claude_empty_cache_breakdown_preserves_total_writes`
- `src/local_sources.rs::provider_names_keep_the_route_even_with_author_qualified_models`
- `src/local_sources.rs::pi_missing_cost_is_unknown_and_explicit_zero_is_preserved`
- `src/local_sources.rs::opencode_database_keeps_recorded_cost_in_both_schemas`
- `tests/model_pricing.rs::refresh_removes_stale_defaults_and_preserves_scoped_zero_prices`

Proved: Before implementation, the origin/reseller test failed with Sonnet rates (2, 10, 0.2, 2.5) instead of (2, 10, 0.1, 2.5); the cross-provider fixture chose input 9 instead of 2; and the unknown-variant test incorrectly priced claude-opus. Before the parser fixes, independent tests failed with 0 instead of 100 cache writes, xiaomi/mimo-v2.5 instead of openrouter/xiaomi/mimo-v2.5, and Some(0) instead of None for missing Pi costs. The real SQLite fixture also failed with None instead of Some(0.125) for an OpenCode recorded cost. After the fixes, these tests pass. The complete 227-provider catalog audit checked all 8,114 quotes and explicit tiers with zero mismatches. The rebuilt executable read the user's actual Claude logs and produced 23.31790880 for 2026-10-11, matching ccusage exactly to eight decimal places and preserving every token counter.
