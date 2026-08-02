---
name: cross-workspace-case
description: Run a repeatable end-to-end collaboration case across two code-agent workspaces and sessions. Use when validating cross-session discovery, task delivery, idle-session reactivation, contract handoff, wait cursors, independent commits, or frontend-backend integration after control-plane changes.
metadata:
  version: "1"
---

# Run a cross-workspace collaboration case

Use a small frontend/backend delivery to validate the complete control-plane path without relying on the implementation workspace. Keep the two Git workspaces clean and independent.

Read [references/todo-fullstack.md](references/todo-fullstack.md) for the canonical Case 1 requirements, worker prompts, and evidence checklist.

## Execute the case

1. Confirm two clean Git workspaces and one persistent session per workspace. Record both session IDs and workspace paths.
2. Send the initial frontend and backend assignments with `intent="request"` and distinct correlation IDs.
3. Let the frontend finish its independent first turn. Treat turn completion as idle, not as permanent session completion.
4. Have the backend send its final API contract to the frontend session with a stable correlation ID.
5. Wait on every returned `turn_id + cursor`. Loop because `wait_sessions` is wait-any.
6. Confirm the frontend starts a later, distinct turn and completes real API integration.
7. Read both sessions and collect commits, tests, contract, workspace paths, and smoke-test evidence.
8. Report pass/fail per acceptance item. Keep known product issues explicit instead of weakening the case.

## Control-plane rules

- Use existing sessions when their workspace and role match. Create a session only when none is suitable.
- Use `shared_workspace` for one worker per independent repository. Use `isolated_worktree` when concurrent workers would modify the same repository.
- Never infer completion from a session becoming idle. Require the assigned acceptance report.
- Treat `request` and `notification` as provenance only; both execute a target turn and may reply.
- Do not reuse a correlation ID for a different artifact or payload.
- Do not ask either worker to modify the other workspace.
- Do not claim integration success from unit tests alone. Run a real HTTP smoke test.

## Deterministic regression

Run the repository test that mirrors this lifecycle without invoking a model:

```bash
go test ./internal/controlplane -run TestCrossWorkspaceFrontendBackendWorkflowReactivatesCompletedFrontend -count=1
```

This test covers authenticated owner RPC, durable admission, frontend first-turn completion, backend contract delivery, a distinct frontend follow-up turn, cursor-scoped waiting, workspace isolation, and provenance persistence.

## Finish

Return a compact timeline containing each session ID, admission delivery, turn ID, terminal status, commit, test result, contract handoff, and final integration result. Call out any orphan session, worktree, branch, or provisioning edge.
