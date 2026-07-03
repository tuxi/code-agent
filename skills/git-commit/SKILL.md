---
name: git-commit
version: "1"
description: Write and execute git commits following Conventional Commits v1.0.0 — analyze the diff, determine type/scope, write a structured message, and commit safely. Use when the user says "commit", "git commit", "提交", "commit this", or asks to save/stage changes. Includes commit splitting for mixed changes, breaking change detection, and a safety protocol for destructive operations.
---

# Git Commit — Conventional Commits

Write commits that a stranger can understand six months from now. The diff tells
you *what* changed; the message tells you *why*.

## Commit Format

```
type(scope): short summary

body — what and why, not implementation diary

footer(s) — BREAKING CHANGE, issue refs, Co-Authored-By
```

### Types

| Type       | When to use                                            |
|------------|--------------------------------------------------------|
| `feat`     | New feature (new tool, new endpoint, new capability)   |
| `fix`      | Bug fix                                                 |
| `docs`     | Documentation only (README, design docs, comments)      |
| `style`    | Formatting, whitespace, linter — no logic change        |
| `refactor` | Code restructure, no feature/fix                        |
| `perf`     | Performance improvement                                 |
| `test`     | Adding or fixing tests                                  |
| `build`    | Build system, dependencies (go.mod, CI config)          |
| `ci`       | CI pipeline, deployment config                          |
| `chore`    | Maintenance, cleanup, tooling — no production code      |
| `revert`   | Reverting a previous commit                             |

### Scope

Scope is the area affected, lowercase, in parentheses. Look at the changed file
paths for the right scope. In this project, common scopes are:

- `tools` — internal/tools/*
- `agent` — internal/agent/*
- `server` — internal/server/*
- `runtime` — runtime, lifecycle, session management
- `skills` — skill system
- `jobs` — background jobs
- `assets` — asset references, asset API
- `mcp` — MCP integration
- `git` — git operations (git_clone, git_pull, git_commit tool)
- `project_graph` — project graph / code analysis
- `workspace` — workspace management
- `docs` — documentation (use type=docs)

If unsure, omit the scope.

### Subject Line Rules

- Imperative mood, present tense: "add" not "added", "fix" not "fixes"
- Lowercase after the colon
- No period at the end
- ≤72 characters (if it's longer, your commit is probably doing too much)

### Body (optional but encouraged)

Separated from subject by a blank line. Write what changed and *why* — not a
step-by-step diary of what you typed. A reader should understand the motivation
without reading the diff. Use bullet points for multiple changes.

### Footers

- `BREAKING CHANGE: description` — for breaking API changes. Use `!` after
  type/scope as shorthand: `feat(api)!: remove /v1/legacy endpoint`
- `Refs: #123` — references an issue
- `Closes: #123` — closes an issue
- `Co-Authored-By: Claude ...` — **required for this project**, matches existing
  commit style (see `git log`)

### Breaking Changes

Two equivalent forms:
```
feat(api)!: remove deprecated auth endpoint
```
```
feat(api): remove deprecated auth endpoint

BREAKING CHANGE: the /v1/auth endpoint returns 410 Gone instead of 200
```

## Workflow

### Step 0: Check ownership — only commit YOUR changes

In a shared workspace, other agents or people may have modified files that are
unrelated to your task. **You must never commit files that belong to someone
else's work.** Before staging anything:

```
git status --porcelain          # list every modified file
```

For each modified file, ask yourself: *"Did I change this as part of the task
I was asked to do?"* If the answer is no, that file does not belong in your
commit.

Signs a file is not yours:
- You did not read, edit, or create it during this session.
- The diff shows changes unrelated to the feature you worked on.
- It's in a package or directory you never touched.
- The file was already modified before you started (check `git status` at the
  start of your session, or compare with `git stash list`).

**If you see unrelated modified files:** do NOT use `all: true`. Stage
selectively using `git add <file>` (via `run_command`) for only the files that
belong to your change. If you're unsure whether a file is yours, ask the user.

### Step 1: Inspect

```
git status                    # what's changed? what's already staged?
git diff --stat               # overview of unstaged changes
git diff                      # full diff of YOUR files
git diff --staged             # review what's staged before committing
```

**Never use `all: true` when unrelated modified files exist** — it will stage
everything, including changes that don't belong to you. Only use `all: true`
when every single modified file is part of your change.

### Step 2: Decide boundaries

Ask yourself: can a reviewer understand this as one logical unit?

Split into separate commits when:
- A bug fix and a refactor are mixed in the same files
- Two unrelated features landed together
- Tests for a feature are in the same diff as the feature
- A dependency bump is mixed with code changes
- Formatting-only changes are mixed with logic changes

If you need to split, use `git add -p` or stage specific files. In this
project you can also use `git add <file>` via `run_command` to stage
selectively, then `git_commit` without `all: true`.

### Step 3: Describe before you write

Say out loud in 1-2 sentences what the change does and why. If you can't
describe it cleanly, the commit is too big or mixed — go back to Step 2.

### Step 4: Write the message

Use the format above. For single-line commits, the subject alone is fine if
it's truly trivial (typo fix, godoc fix). For anything non-trivial, add a
body.

### Step 5: Commit

Use `git_commit` tool. If you staged selectively (Step 2), omit `all`. If you
want to commit everything at once, use `all: true`.

### Step 6: Verify (optional, for high-risk changes)

Run the fastest meaningful check before moving on:
```
go build ./...     # does it compile?
go vet ./...       # basic static analysis
go test ./<pkg>/.. # relevant tests
```

## Safety Protocol

These are hard rules, not suggestions:

- **NEVER** run `git push --force` to main/master
- **NEVER** skip hooks (`--no-verify`, `--no-gpg-sign`) unless the user
  explicitly asks
- **NEVER** run destructive commands (`git reset --hard`, `git clean -fd`)
  without explicit user confirmation
- **NEVER** commit files you did not modify — if other agents changed files in
  the same workspace, staging everything (`all: true`) will wrongfully claim
  their work as yours. Verify ownership via `git status --porcelain` first,
  and stage only the files you personally changed.
- **NEVER** commit secrets — scan the diff for `.env`, `credentials.json`,
  private keys, API tokens, passwords
- **NEVER** amend a commit that has already been pushed — create a new commit
  instead
- **NEVER** update git config (`git config`)
- If a hook fails, fix the issue and create a NEW commit — do not amend

## Project-Specific Rules

These apply to THIS repository (code-agent):

- **Work on a branch**, do not commit to `main` directly. Create a feature
  branch first if you are on `main`.
- **End every commit message with:**
  ```
  Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
  ```
  Check `git log -5 --format="%H %s%n%b"` to confirm the exact format if
  unsure.
- **Match the existing style.** This project uses lowercase subjects, scopes
  in parentheses, and detailed bullet-point bodies. Match the density and
  voice of the last 10 commits.

## Examples (from this project's own history)

Good — feature with scope and detailed body:
```
feat(assets): blob endpoint with range support + MIME-driven asset kinds

- Add GET /v1/conversations/{id}/assets/{asset_id}/blob — serves raw
  binary file bytes with HTTP range support...
- AssetPreviewResponse gains Metadata field...
- Tests: binary preview metadata, blob range request (206)...

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
```

Good — single-purpose with concise body:
```
fix(runtime): v1.2 — clean suspend/resume (no cancel error, no spurious resume)
```

Good — docs change:
```
docs: unify embedding runtime to Core ML for both Mac and iOS
```

Bad (avoid these patterns):
```
fix bug                    # too vague — what bug? where?
feat: add thing            # "thing" is not a description
WIP                       # not a conventional commit type
updated code              # present tense, imperative mood
```
