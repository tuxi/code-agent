# Project Rules

## Core Principles

CodeAgent is an AI-native coding agent runtime. The LLM decides what to do next; the runtime controls what can actually be executed. Tools are explicit, typed, observable capabilities.

- Inspect before editing. Read files in full before making changes.
- Plan before complex changes. Ask when requirements are ambiguous.
- Every step must be traceable. No hidden automation.
- Never apply patches silently. Show git diff after changes.
- Validate with tests when command execution is available.
- No database before the basic agent loop works.
- Keep answers short and concise. Technical prose only.

## Safety Hardlines

These rules are non-negotiable. Violating them can destroy work.

- NEVER: `git reset --hard`, `git checkout .`, `git clean -fd`
- NEVER: `git stash`, `git add -A`, `git add .`
- NEVER: `git push --force` or `git push --force-with-lease`
- NEVER: silently delete or overwrite user files without confirmation
- NEVER: run unapproved dependency installs (`go get`, `npm install`, `pip install` without user consent)
- When in doubt, ask before acting.

## Project Conventions

- Language: Go
- Build: `go build ./...`
- Test: `go test ./...`
- Lint: `go vet ./...`
- Commit message format: concise, technical, no emojis
- Use `go test ./internal/...` for specific packages; avoid running the full test suite unless asked
- Do not commit unless the user explicitly asks

## Skills

This project uses a progressive-disclosure skill system (`skills/` directory). Skills are task-specific playbooks loaded on demand. When you repeatedly give similar instructions, consider moving them into a skill via `codeagent skill init <name>`.

<!-- This file is the starting template from `codeagent init`. Customize it as you learn what your agent needs. Rules should be high-frequency and non-obvious — low-frequency workflows belong in skills. -->
