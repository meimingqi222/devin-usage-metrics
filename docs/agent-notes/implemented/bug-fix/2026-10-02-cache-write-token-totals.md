# Agent Note: Include cache writes in desktop token totals

Status: implemented

## Problem

The parsers keep ordinary input, cache reads, and cache writes separate, but desktop period, model, and session totals omit cache writes. The CLI already includes them, causing inconsistent totals for the same usage.

## Decision

Period and model aggregation retain a separate cache creation counter. Total token counts add ordinary input, output, cache reads, and cache creation. Session displays include cache writes and fall back to turn usage when session metadata is empty. The overview includes the separate cache write counter. Cards, stacked bars, period rows, and bilingual session breakdowns display cache writes separately.

## Alternatives considered

Adding writes to ordinary input or cache reads would obscure the distinct billing categories. Adding the 5-minute and 1-hour breakdowns on top of cache creation would count the same writes twice.

## Consequences

Desktop totals increase by the previously omitted cache writes and agree with the CLI formula. Pricing and parsed usage remain unchanged. Cache reads and writes stay distinct in the user interface.

## Verification

- `src/agg.rs::totals_include_cache_writes_once_across_periods_and_models`

Proved: With the new regression test and the old aggregation, cargo test --lib totals_include_cache_writes_once_across_periods_and_models failed with 840 instead of 1840. After adding cache creation to aggregation and totals, the test passed. The test covers day, week, month, repeated model accumulation, and avoids counting the 5-minute and 1-hour subdivisions twice.
