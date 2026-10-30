# Agent Note: Keep MCP/skill rows after a write when one agent file is bad

Status: implemented

## Problem

`InventoryMCP` / `InventorySkills` return rows plus `errors.Join` when any one agent file is unreadable. `refreshExt` stored those rows. `runExtNotice` and `installPreview` only stored rows when `err == nil`, so a successful write against a machine with one broken config left the matrix frozen while the banner still said Updated.

## Decision

`storeExtInventory` always assigns the returned slices, matching `refreshExt`. After a successful write, any inventory error is appended to the success message.

## Alternatives considered

**Keep rows only when inventory has no error.** That is the bug: one bad `~/.claude.json` then hides a Codex write.

**Drop the inventory error from the message.** The pills would update but the user would not see why Claude is missing.

## Consequences

A write still refreshes pills when another agent's file is broken. The success line is followed by that agent's parse error.

## Verification

- `internal/desktop/ext_test.go::TestExtWriteKeepsRowsWhenAgentFileBad`

Proved: `runExtNotice` stored rows only when `err1 == nil` → `TestExtWriteKeepsRowsWhenAgentFileBad` failed with `stale after write` (`github` still shown, `new` missing); guard restored and the test re-run green. Output saved to `docs/agent-notes-evidence/ext-inventory-after-write-red.txt`.
