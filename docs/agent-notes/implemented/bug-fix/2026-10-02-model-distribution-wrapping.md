# Agent Note: Keep model distributions complete and show costs in expandable rows

Status: implemented

## Problem

The previous flex-wrap component rendered ordinary String labels as single letters in the actual desktop table even though the isolated StyledText test passed. Parenthesized model fees also consumed scarce distribution-column width.

## Decision

Summary rows use compact model summaries; multiple models show a proportional bar, top model and extra-model count. Clicking a multi-model summary expands a separate detail panel with model names, token share, turns, input, output, cache-write, cache-read, total and cost. Long detail names wrap. Each detail numeric column has a fixed shared width; rows grow with their text. Known free models show $0.00; missing prices show an em dash. A single-model row already contains its model totals and needs no expansion. Rebuilding buckets resets expansion; the table can hide inactive buckets.

## Alternatives considered

Intrinsic-width flex-wrap entries reproduced clipping with ordinary strings. Ellipsis and wider windows hide or postpone the problem. Parenthesized fees made narrow labels harder to read; independent detail columns make each model fee explicit.

## Consequences

Model fees no longer compete for space inside summary labels. Numeric detail columns stay aligned at default and wider window sizes. Detail panels use the existing scrolling usage view. Token aggregation is unchanged; the separate Pro pricing note owns billing-source changes.

## Verification

- `tests/model_distribution_layout.rs::long_model_labels_wrap_inside_the_column`
- `tests/model_distribution_layout.rs::summary_layout_renders_correctly_without_abnormal_gaps`

The interactive Windows test checks ordinary String entry widths, shaped glyph positions, complete wrapped labels, and full summary/detail table rows. Run cargo test --test model_distribution_layout -- --ignored --test-threads=1.

Proved: Restoring the preceding committed component made the strengthened test fail: a plain entry occupied only 109.333336px of a 160px column. Restoring the fix passed all 16 plain-entry probes and 20 shaped-label/full-row probes. Native application previews at default and narrower widths were separately captured and inspected; actual clicks collapsed and re-expanded the September 23 details, including the free compactor fee.
