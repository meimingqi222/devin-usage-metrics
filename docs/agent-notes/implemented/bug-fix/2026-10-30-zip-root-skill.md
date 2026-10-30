# Agent Note: Install a skill whose SKILL.md sits at the zip root

Status: implemented

## Problem

After the "no first-SKILL.md fallback" fix, `findSkillRel` required a parent directory named SkillID. GitHub `codeload` zips wrap the repo in `<repo>-<ref>/`, so a single-skill repo with `SKILL.md` at its root (`my-skill-main/SKILL.md`) could not install. Preview markdown also took the first `SKILL.md` under the prefix, so a nested `s/sub/SKILL.md` listed before `s/SKILL.md` showed the nested file.

## Decision

- After the SkillID-parent match, accept a zip-root `SKILL.md` (`<wrapper>/SKILL.md`) when its frontmatter `name` equals SkillID.
- Preview markdown is only the `SKILL.md` directly in the matched directory, not a nested one.
- A zip-root `SKILL.md` whose frontmatter name does not match SkillID is still refused, so a random root file cannot install as `pdf`.

## Alternatives considered

**Accept the only SKILL.md in the archive even when its name does not match.** That reopens the first-SKILL.md fallback: `repo-main/SKILL.md` would install under whatever SkillID the user picked.

**Match any SKILL.md whose frontmatter name equals SkillID, including nested docs copies.** A docs example could win over the real skill directory.

## Consequences

Single-skill GitHub repos with `name:` in root frontmatter install. Nested extra `SKILL.md` files do not replace the preview. `TestExtractSkillRequiresMatchingDir` still rejects a root file named for a different skill.

## Verification

- `internal/ext/leftover_test.go::TestFindSkillRelZipRoot`
- `internal/ext/leftover_test.go::TestExtractSkillRequiresMatchingDir`

Proved: `findSkillRel` returned after the first SKILL.md whose parent was not SkillID → `TestFindSkillRelZipRoot` failed with `SKILL.md not found in archive`; guard restored and the test re-run green. Output saved to `docs/agent-notes-evidence/zip-root-skill-red.txt`.
