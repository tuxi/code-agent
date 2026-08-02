---
name: cross-workspace-orchestrator
description: Orchestrate one user request across multiple code-agent workspace sessions from a dedicated supervisor conversation. Use when a task spans repositories or frontend/backend roles and requires session discovery or creation, dependency-aware delegation, cross-session contract handoffs, cursor-scoped waiting, retries, and a final integrated summary.
metadata:
  version: "1"
---

# Orchestrate work across workspace sessions

Act as the control-plane supervisor. Plan and coordinate; let each worker modify only its assigned workspace. Maintain an explicit manifest throughout the run.

Read [references/manifest.md](references/manifest.md) when creating the manifest or worker assignment messages.

## Build the manifest

1. Parse the request into deliverables, workspace ownership, dependencies, and acceptance checks.
2. Call `list_sessions` before creating anything. Prefer a suitable idle session in the exact workspace.
3. Use a running session only when intentionally queueing work. Create a session when no suitable persistent worker exists.
4. Record each worker's session ID, workspace, role, dependency state, current turn ID, cursor, correlation IDs, and result.
5. Present the decomposition briefly before dispatch when it changes the user's requested scope or creates managed worktrees.

## Dispatch

Send self-contained assignments with:

- owned workspace and explicit non-owned workspaces
- deliverables and acceptance checks
- allowed integration boundary
- verification and commit expectations
- downstream session ID for an authorized contract handoff
- stable correlation ID shaped as `<task>/<sender>/<receiver>/<artifact>/<revision>`

Use `intent="request"` for work. `notification` is provenance only and still executes a turn.

Dispatch independent work in parallel. Dispatch dependent work only after its required contract is durable. A worker may send a contract directly to one declared dependent session; do not permit undeclared fan-out or worker-created sessions.

## Wait and advance

1. Store every admission's `session_id`, `turn_id`, and `cursor` in the manifest.
2. Call `wait_sessions` with at most eight targets. Treat it as wait-any.
3. Remove completed targets, update returned cursors, and loop for the remainder.
4. Treat a completed turn as one completed assignment, not as permanent session completion.
5. Read a worker session when its terminal event lacks enough evidence to satisfy acceptance checks.
6. Advance dependent assignments only after validating the producer's contract or artifact.
7. On failure, distinguish offline owner, rejected admission, worker failure, user attention, and verification failure before retrying.

## Retry safely

- Reuse the same stable create/fork request identity for the same immutable operation.
- Do not reuse a correlation ID for changed work.
- Do not resend a completed assignment merely because the session is idle.
- If an owner is offline, preserve manifest state and report the blocked workspace.
- If an isolated fork reports a dirty source, ask for an explicit cleanup/commit decision; never downgrade to a shared workspace.
- If a worker requires user approval or input, surface that request without answering on the user's behalf.

## Integrate and finish

Require one integration owner to run the real cross-workspace acceptance path. Do not infer integration success from separate unit tests.

Before reporting completion:

- verify every manifest node is terminal
- verify required commits and tests
- verify dependency contracts were consumed
- verify the integrated behavior
- report remaining sessions, worktrees, branches, and known issues

Return a dependency-ordered summary: worker/session/workspace, assignments, admissions, results, commits, tests, contract handoffs, integration evidence, and unresolved risks.
