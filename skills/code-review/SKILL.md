---
name: code-review
version: "1"
description: Review a diff, PR, or code change — check correctness, error handling, concurrency safety, style, testing, and design. Use when the user says "review this", "code review", "check this change", "look at this PR", "audit this code", or asks for a structured review of any Go or general code. Also triggers on "is this safe?", "anything wrong here?", "review and tell me what to fix."
---

# Code Review

You are reviewing code written by another developer (possibly your own prior
self). The goal is to catch problems the author missed — not to nitpick style
that a linter would catch. Every issue you raise must be actionable.

## Workflow

### 1. Determine the scope

Ask the user if it is not clear. Options:

- **Uncommitted changes**: `git_diff` on the workspace.
- **A specific commit or range**: `git log` then `git_diff`.
- **A file or directory**: `read_file` the files.
- **A PR**: user provides the branch or range.

If the user says "just review everything", default to uncommitted changes with
`git_diff`. If there are none, show recent commits and ask which to review.

### 2. Understand the change

Before you review, read enough context to understand what the code does and why.
Do not review a diff line-by-line in isolation — a line that looks wrong may be
correct in the call chain it belongs to.

- Use `project_graph find_references` on changed symbols to see who calls them.
- Use `project_graph find_symbol` to find the definition of unfamiliar types.
- Use `read_file` on callers and callees of the changed functions.
- Use `grep` to find related patterns (same error type used elsewhere, same
  concurrency pattern in the codebase).

### 3. Review along these dimensions

For each dimension, focus on the diff but apply the project's existing patterns
as the baseline. A change that looks unconventional may be fine if it matches
how the rest of the codebase does it.

#### Correctness (always check first)

- Logic errors: off-by-one, inverted conditions, missing cases in switches.
- Nil dereference risks: can a pointer/map/slice be nil at this point?
- Boundary conditions: empty input, zero value, max value, EOF, context
  cancellation.
- Type conversion safety: truncation, overflow, loss of precision.
- Does the change alter behavior for existing callers in an unintended way?

#### Error Handling

- Are returned errors checked? Checked with `_` = intentional or bug?
- Are errors wrapped with context (`fmt.Errorf("...: %w", err)`) or bare?
- **Single-handling rule**: an error must be either returned OR logged, never
  both. A `log.Error(err); return err` pair is a duplicate and a bug.
- Are `errors.Is` / `errors.As` used for sentinel matching, not `==`?
- Panic: any new `panic` call — is it truly unrecoverable, or should it be an
  error return?

#### Concurrency Safety

- New goroutines: do they have a clear lifetime? How do they exit?
- Shared state: is it protected by a mutex or channel? Any unprotected access?
- Context propagation: does the goroutine respect context cancellation?
- Channel operations: any unbuffered send without a corresponding receive that
  could deadlock?
- `sync.WaitGroup`, `errgroup`, or equivalent for goroutine lifecycle?

#### Style & Clarity

- Naming: does the new name match the pattern in the file/package? Does it say
  what it is, not what it does?
- Control flow: deeply nested? Happy path at the left margin?
- Function length and parameter count: >4 params without an options struct?
- Comments: do they explain *why*, or just restate *what*? Missing comments on
  exported symbols?

#### Testing

- Does the change touch a path that has no test? Flag it.
- Does the test cover the edge case the change introduces?
- If a bug fix, is there a regression test?

#### Design

- New abstraction: does it earn its keep, or is it premature?
- Does the change duplicate an existing helper or pattern?
- Interface changes: does this break implementers or force downstream changes?

### 4. Produce the report

Use this structure:

```
## Review: [scope — file, commit, or "uncommitted changes"]

### Summary
[1-3 sentences: what the change does, overall assessment]

### Issues

#### 🔴 [Title] — [file:line]
**Problem**: [what's wrong and why it matters]
**Fix**: [concrete suggestion]

#### 🟡 [Title] — [file:line]
**Problem**: [...]
**Fix**: [...]

#### 🔵 [Title] — [file:line]
**Problem**: [...]
**Fix**: [...]

### ✅ Good
- [Something the author did well — be specific]
```

Severity levels:
- 🔴 **Critical**: bug, data loss, security issue, crash — must fix before merge.
- 🟡 **Important**: likely bug, brittle code, missing error handling — should fix.
- 🔵 **Nit / Suggestion**: style, naming, clarity — optional, author's call.

### 5. Offer to fix

After the report, ask: "Want me to fix any of these?"

## Rules

- **Never review code you have not read.** If the diff is large, read the key
  files before forming opinions.
- **Silence is not approval.** If you cannot determine whether something is
  correct (e.g. unfamiliar domain logic), say so instead of skipping it.
- **Respect the codebase's conventions.** If the project uses a pattern
  consistently, do not flag it as wrong even if you personally dislike it.
- **One issue per finding.** Do not bundle unrelated problems into one item.
- **If there are no issues, say so clearly.** "No issues found" is a valid
  review. Do not invent problems to fill the report.

## Parallel reviews

For large changes (>5 files or >200 lines), use sub-agents to review
independent files or dimensions in parallel. Each sub-agent gets a focused
prompt: "Review file X for correctness and error handling" with the diff
content. Combine their findings into the final report.

## References

- `references/dimensions.md` — detailed checklist per review dimension. Load
  for deep reviews or when the user asks for an exhaustive audit.
