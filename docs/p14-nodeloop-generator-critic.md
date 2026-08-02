# Phase 14 — NodeLoop Generator-Critic — PRD

> Status: planning. This spec defines a Flux `NodeLoop`-based Generator-Critic
> pattern for `cross_workspace_collaboration_v2`: an implementing agent produces
> a change, a reviewer agent checks it, and the loop repeats until the reviewer
> approves or a stop condition is met.
>
> Builds on: P13 (Graph Agent Runtime), `cross_workspace_collaboration_v1`
> (Map + AwaitBinding + fan-in).

## 1. Problem

v1 (`cross_workspace_collaboration_v1`) dispatches a single assignment per
agent and collects the terminal result. There is no built-in quality gate:
the agent's first attempt is the final output. For implementation tasks
(editing code, running builds, verifying tests), this is insufficient —
the agent may produce incorrect code that compiles but fails the
acceptance checks, and there is no built-in correction loop.

The user currently handles this manually: review the Map output, find
problems, re-dispatch with fix instructions. This breaks the "fire and
forget" workflow promise.

## 2. Target Experience

```
Supervisor: plan_workflow(template=cross_workspace_collaboration_v2, agents=[{
    role: "backend",
    session_id: "...",
    message: "实现 phone 字段结构体化...",
    acceptance: "go build && go test ./...",
    max_iterations: 3
}])

Flux executes per agent:
  iteration 1: implement → review (VERDICT: REQUEST_CHANGES, "missing nil check")
  iteration 2: fix → review (VERDICT: REQUEST_CHANGES, "test case not updated")  
  iteration 3: fix → review (VERDICT: PASS, "all checks pass")
  → agent output = iteration 3 result with VERDICT: PASS
```

The Supervisor sees one aggregated result per agent. Failed iterations are
visible in `workflow_events` for debugging but don't block the Map fan-in.

## 3. Architecture

### 3.1 DAG Structure

```
Parent (Map, parallel=N):
  run_agents → fan-in → end

Child per agent (NodeLoop):
  implement (NodeTool: send_to_session) → admission
  await_impl  (NodeAwait: external_task + fallback_poll=check_turn)
  read_impl   (NodeTool: read_session) → implementation result
  dispatch_review (NodeTool: send_to_session) → reviewer gets the diff
  await_review    (NodeAwait: external_task + fallback_poll=check_turn)
  read_review     (NodeTool: read_session) → VERDICT: PASS | REQUEST_CHANGES
  → loop back to implement if REQUEST_CHANGES and iterations < max
  → exit to end if PASS or max reached
```

### 3.2 NodeLoop `carry` Schema

The loop passes state between iterations via `carry`:

```json
{
  "iteration": 2,
  "max_iterations": 3,
  "last_verdict": "REQUEST_CHANGES",
  "review_feedback": "Line 45: nil check missing. Line 78: test case expects old format.",
  "implement_session_id": "<same session reused>",
  "reviewer_session_id": "<same session reused>"
}
```

### 3.3 Stop Conditions

| Condition | Action |
|---|---|
| `reviewer returns VERDICT: PASS` | Exit loop → fan-in with success |
| `iteration >= max_iterations` | Exit loop → fan-in with last result + warning |
| `implement turn fails` | Exit loop → fan-in with failure |
| `reviewer turn fails` | Retry reviewer once, then exit with failure |
| `timeout` (per-loop timeout_seconds) | Exit loop with timeout |

### 3.4 Reviewer Agent

A dedicated reviewer session (separate from the implementer). It receives:
- The original assignment
- The implementer's output (diff, test results, commit)
- The acceptance criteria

It returns a structured verdict. The `carry` carries review feedback to the
next implementation iteration.

The reviewer can be:
- **Same-workspace**: a separate session in the same repository (sees the diff directly)
- **Cross-workspace**: a session in a different repository (relies on the implementer's report)

For v2, start with same-workspace reviewer (simpler, no need to transfer diffs
between workspaces).

### 3.5 Tool: `review_implementation`

A new read-only tool for the reviewer session that formalizes the review output:

```
Name: review_implementation
Input: { verdict: "PASS" | "REQUEST_CHANGES", feedback: "...", evidence: ["file:line", ...] }
Output: { verdict, feedback, evidence, iteration }
```

This gives the loop a structured signal to decide whether to continue.

## 4. Implementation Plan

### Phase A: Single-Agent Loop (NodeLoop skeleton)

1. Add `cross_workspace_collaboration_v2` template to `flux_cross_workspace.go`
2. Define `NodeLoop` child workflow with `implement → review` cycle
3. Implement `carry` schema: `{iteration, max_iterations, last_verdict, feedback}`
4. Reuse existing `send_to_session`, `check_turn`, `read_session` nodes
5. Stop conditions: `VERDICT: PASS` or `iteration >= max`
6. Tests: single agent, 1-iteration PASS, 3-iteration REQUEST_CHANGES→PASS, max exceeded

### Phase B: Reviewer Agent

1. Add `review_implementation` tool
2. Wire reviewer session into the loop
3. Carry review feedback to next implementation iteration
4. Tests: reviewer PASS, reviewer REQUEST_CHANGES with feedback propagation

### Phase C: Map Integration

1. Register v2 template in `plan_workflow` schema
2. Map `max_iterations` and `acceptance` fields in agent spec
3. Integration test: 2 agents, one passes in 1 iteration, one needs 2

## 5. Agent Spec Extension (v2)

```json
{
  "role": "backend",
  "session_id": "<IMPLEMENTER_ID>",
  "reviewer_session_id": "<REVIEWER_ID>",
  "workspace_path": "/path/to/repo",
  "message": "<complete implementation assignment>",
  "acceptance": "go build ./... && go test ./...",
  "max_iterations": 3,
  "correlation_id": "task/backend/1",
  "intent": "request"
}
```

New fields: `reviewer_session_id`, `acceptance`, `max_iterations` (default 1,
which degrades to v1 behavior).

## 6. Non-Goals

| Item | Why deferred |
|---|---|
| Cross-workspace reviewer | Start with same-workspace; reviewer reads the diff from the repo directly |
| Auto-fix (LLM generates fix without re-implementing) | The implementer re-does the work with review feedback; auto-fix is a different pattern |
| Reviewer-in-the-loop for every tool call | The reviewer checks the final output, not intermediate steps — same as human code review |
| `continue_on_failure` policy for Map | v1 fail_fast is sufficient; partial-failure fan-in is a separate concern |
