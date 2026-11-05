---
name: codeagent-conventions
version: "2"
description: Non-obvious rules and real gotchas for THIS repo (code-agent). Load before editing this project's Go — especially internal/agent (the loop), internal/tools, internal/observation, internal/reflection, or the system prompt.
---

# Working in code-agent

This is a thin-loop coding agent. A few rules are easy to get wrong because they
are not Go common sense — they are this project's spine.

- **The agent loop is tool-agnostic.** `internal/agent/loop.go` must never
  hard-code a tool name (`if toolName == "run_command"`) or import a specific
  tool's package. Cross-cutting behavior is added through interfaces the loop
  checks — `SideEffectingInput`, `SkillAnnouncer`, the `Observer` / `Reflector`
  hooks. If you're about to special-case a tool in the loop, add an interface
  instead.
- **The model owns control flow; the runtime owns boundaries.** Observation
  (P4.1) and Reflection (P4.3) are *data layers*: they classify and surface, the
  model decides what to do. Never make them retry, sequence, or auto-fix. No
  decision state machines in the loop.
- **run_command starts in the workspace root.** Simple commands execute directly;
  supported chains, redirects, assignments and substitutions execute through
  `sh -c` after command-policy checks. This does not bypass approval or workspace
  restrictions. Prefer path arguments over `cd`, and a command's filter flags
  over piping already-captured output. Use `background: true` for long commands,
  `job_wait` to wait, `job_logs` for output, and `job_cancel` to stop them.
  For external listing/reading/searching, use `list_files`/`read_file`/`grep`
  with an absolute path to request host approval. These tools do not expand `~`;
  do not shell-escape spaces in JSON paths. If approval is unavailable or denied,
  do not retry through Python or another command to bypass the refusal.
- **Comments explain why, not what**, and match the surrounding density and
  voice. Don't add narration the rest of the file wouldn't.

## Gotchas

These are real pitfalls hit while working here — append new ones as they happen.

- A literal BOM character in a Go string literal is a **compile error**
  ("illegal byte order mark"). Use the `"\ufeff"` escape, never the raw byte.
- **Two packages with the same name can't both be imported.** `internal/skills`
  (the registry) and the load_skill tool would collide, so the tool package is
  `internal/tools/skill` (singular). Name new tool packages distinctly.
- Adding a tool only requires registering it in `buildRegistry` — you do **not**
  touch the loop. If a change makes you edit the loop to add a tool, reconsider.
- New event kinds go through the `Emitter`; the loop emits, it never writes to
  stdout. Rendering belongs in the console emitter, not the loop.
- `read_file` prefixes every line with `N\t` (a 1-based line number + tab) for
  reference. Those prefixes are NOT part of the file — strip them when building
  an `edit_file` / `apply_patch` `old` string, or request `line_numbers: false` when copying source text.
- When `insert_file` is available and a change only ADDS lines, prefer `insert_file` (path + 1-based `line` +
  `content`) over `edit_file`. It needs no `old` text to match, so long CJK
  paragraphs, smart quotes and backticks cannot break it — the exact failure mode
  `edit_file`/`apply_patch` hit. `edit_file` remains the tool for replace/delete.
