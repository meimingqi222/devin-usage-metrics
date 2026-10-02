# Agent Note: Wrap long model usage labels within their column

Status: implemented

## Problem

The model distribution column wraps between model entries, but each entry has an unconstrained intrinsic text width. At the default window size, long adaptive model names plus token counts and costs exceed the column and are clipped, hiding amounts.

## Decision

The shared model entry limits its width to the column, allows the label's container to shrink to zero minimum width, and uses normal whitespace wrapping. Its color dot keeps a fixed size and aligns with the first text line. The parent column spaces entries with gaps instead of a trailing margin that consumes width outside the entry's constraint. Row heights follow the wrapped text.

## Alternatives considered

Ellipsis and tooltips still hide amounts at the default size. Increasing the default window width only postpones the issue. Giving every entry a full-width row would discard the existing compact arrangement at wider widths.

## Consequences

Long labels remain complete at narrow widths; short labels still share a line when space permits. The extracted component is used by both the desktop table and the native layout regression fixture. No prices or token totals change.

## Verification

- `tests/model_distribution_layout.rs::long_model_labels_wrap_inside_the_column`

Run on an interactive Windows desktop with cargo test --test model_distribution_layout -- --ignored --test-threads=1. The test is explicitly ignored during ordinary CI because it creates a native GPUI window and measures real shaped text.

Proved: Before the style fix, the native test failed because the SWE adaptive label reached x=238 in a 160px column without wrapping. After the fix, the same test passed at 160, 216, 320, and 700px widths, preserving every label character and growing narrow labels vertically.
