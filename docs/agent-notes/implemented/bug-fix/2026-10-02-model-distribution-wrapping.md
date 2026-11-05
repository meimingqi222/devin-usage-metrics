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

- `internal/desktop/visual_test.go::TestAdaptiveSessionModelDisplay`
- `internal/desktop/sessions_visual_test.go::TestSessionsOriginalControlsAndEmptyStates`
- `internal/desktop/visual_test.go::TestOriginalDenseVisualStates`

The Go desktop no longer has a dedicated model-distribution integration test; the rendered-layout fixtures in `internal/desktop` cover long model labels and summary/detail rows. The original Windows interactive layout test was removed with the Rust tree.

Proved: Restoring the preceding committed component made the strengthened test fail: a plain entry occupied only 109.333336px of a 160px column. Restoring the fix passed all 16 plain-entry probes and 20 shaped-label/full-row probes. Native application previews at default and narrower widths were separately captured and inspected; actual clicks collapsed and re-expanded the September 23 details, including the free compactor fee.
