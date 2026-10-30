# Agent Note: Do not treat quotes inside a one-line TOML string as multiline

Status: implemented

## Problem

`tomlMultilineState` searched for `"""` / `'''` anywhere on a line after stripping comments. A value like `note = "use ''' here"` opened multiline state 2. Every later header was treated as part of that string, so the current table ran to EOF. `deleteTOMLServer` then deleted `[profile]` and every following table. Upsert already refused a block that contained those markers; delete did not.

## Decision

- At state 0 the scanner walks the line: skip `#` comments, skip `"…"` / `'…'` one-line strings (with escapes in basic strings), and only then treat `"""` / `'''` as a multiline opener.
- `deleteTOMLServer` refuses with `errTOMLMultiline` when multiline state is still open at the end of a server block.

## Alternatives considered

**Refuse delete whenever the block contains `'''` or `"""` as a substring.** That would also refuse `note = "use ''' here"`, which is a valid one-line string, so users could not remove that server.

**Leave delete as a raw byte cut of `scanTOMLTables` ranges.** After the scanner is fixed, a well-closed multiline string is safe to delete; the extra open-state check is for an unclosed `"""` that would still swallow the rest of the file.

## Consequences

Deleting `[mcp_servers.a]` keeps later tables when `'''` / `"""` sit inside a one-line string. An actually unclosed multiline string still cannot be deleted through this path.

## Verification

- `internal/ext/toml_test.go::TestDeleteTOMLKeepsProfileWhenTripleQuoteInString`
- `internal/ext/toml_test.go::TestDeleteTOMLRefusesUnclosedMultiline`

Proved: `skipTOMLQuoted` returned after the opening quote → `TestDeleteTOMLKeepsProfileWhenTripleQuoteInString` failed with `multiline TOML requires a preserving TOML parser`; guard restored and the test re-run green. Output saved to `docs/agent-notes-evidence/toml-quoted-multiline-red.txt`.
