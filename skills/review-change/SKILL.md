---
name: review-change
version: "1"
description: How to investigate before editing — read the related code, fix root causes not symptoms, check side effects and reuse. Load before a non-trivial change, a refactor, or a bug fix where the cause is not yet understood.
---

# Review before (and after) changing

The dominant failure mode here is moving too fast and reading too little. A few
extra reads up front beat a wrong fix.

Before editing:

- **Honor user read/edit boundaries first.** If the user says not to inspect a
  dependency, path, or file class, do not read it via `read_file`, `run_command`,
  grep, or any workaround. Investigate with the allowed project code and call out
  uncertainty instead of crossing the boundary.
- **Read the related implementation first**, not just the failing line.
  Understand the call chain and why the current code is shaped the way it is.
- **Check whether something similar already exists** — a helper, a pattern, an
  interface — and reuse it instead of adding a parallel path.

When changing:

- **Fix the root cause, not the symptom.** Making an error message disappear is
  not the same as fixing the bug.
- **Check side effects.** Who else calls this? Does the change alter behavior
  elsewhere? Is there test coverage for the new path?

## Gotchas

- Don't fix a symptom you can see while ignoring a root cause you'd have to read
  for.
- A change that "looks right" but you didn't trace the callers of is a guess.
  Trace first.
