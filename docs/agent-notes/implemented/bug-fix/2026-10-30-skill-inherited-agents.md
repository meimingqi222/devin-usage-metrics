# Agent Note: Count auto-loaded ~/.agents/skills as enabled

Status: implemented

## Problem

Skill inventory only put copies under each agent's own `skills/` directory into `ByAgent`. Skills that live only in `~/.agents/skills` showed a 共享 tag and WorkBuddy (when that tree had a symlink) as on, while Codex, OpenCode and Pi — which load that shared folder automatically — stayed off. The matrix then offered Enable copies that those agents did not need, and the 已启用 count was short.

## Decision

- `inheritedSkillDirs` lists extra scan roots the agent loads without a dedicated copy: Codex and Pi use `~/.agents/skills`; OpenCode uses that folder and `~/.claude/skills`.
- Dedicated `skillDirs` / `extraSkillDirs` still win. An inherited hit fills `ByAgent` only when that agent has no dedicated place, and it is not added to `Places`, so per-agent `RemoveSkill` cannot delete the shared folder.
- Claude, Antigravity and WorkBuddy stay dedicated-only.
- Clicking off an inherited pill does not delete files. A shared inherit explains that `~/.agents/skills` is auto-loaded and points at **取消共享**. An OpenCode inherit from Claude's directory tells the user to remove Claude's copy.
- **取消共享** (card header, only when a 共享 place exists) confirms `UnshareSkill`: delete `~/.agents/skills/<name>` only. Dedicated copies stay. Codex, OpenCode and Pi then lose the auto-loaded copy together.
- `EnableSkill` still treats an inherited agent as already on.

## Alternatives considered

**Mark every matrix agent on whenever `~/.agents/skills` has the skill.** Claude Code and Antigravity do not load that folder; WorkBuddy only sees it through its own tree. That would show false enables.

**Copy the shared skill into each inheriting agent's dedicated directory on inventory.** Inventory would write on every refresh.

**Drive the pills from `Places` plus a 共享 tag, without `ByAgent`.** The per-agent on/off control and 已启用 count both read `ByAgent`.

## Consequences

A skill that exists only in `~/.agents/skills` shows Codex, OpenCode and Pi on. Removing a dedicated copy falls back to inherited if the shared (or Claude) copy remains. Clicking an inherited pill never deletes `~/.agents/skills`. Unshare (explicit) deletes only that shared folder and leaves dedicated copies. Install and Enable still write dedicated directories, never the shared folder.

## Verification

- `internal/ext/skills_test.go::TestInventorySkillsMarksInheritedShared`
- `internal/ext/skills_test.go::TestInventorySkillsOpenCodeInheritsClaude`
- `internal/ext/skills_test.go::TestEnableSkillRefusesInheritedShared`
- `internal/ext/skills_test.go::TestRemoveSkillSkipsSharedForInheritedAgent`
- `internal/desktop/ext_test.go::TestExtSkillsInheritedFromShared` (inherited pill click keeps `~/.agents/skills` and shows 取消共享)

Proved: `inheritedSkillDirs` returned nil → `TestInventorySkillsMarksInheritedShared` failed with Codex missing from `ByAgent` and `TestInventorySkillsOpenCodeInheritsClaude` failed with OpenCode off; guard restored and both tests re-run green. Output saved to `docs/agent-notes-evidence/skill-inherited-agents-red.txt`.
