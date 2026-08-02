# Supervisor manifest and message contracts

## Manifest

Keep this structure in working memory or the plan:

```json
{
  "task_id": "todo-demo",
  "acceptance": ["backend tests pass", "frontend uses live API"],
  "workers": {
    "frontend": {
      "workspace": "/absolute/frontend",
      "session_id": "...",
      "source": "existing | created | forked",
      "depends_on": ["backend.api-contract"],
      "status": "planned | sent | queued | running | completed | failed | blocked",
      "turn_id": "...",
      "cursor": 0,
      "commit": "...",
      "result": "..."
    }
  },
  "artifacts": {
    "backend.api-contract": {
      "producer": "backend",
      "consumer": "frontend",
      "correlation_id": "todo-demo/backend/frontend/api-contract/1",
      "status": "pending | delivered | consumed"
    }
  }
}
```

## Worker assignment

```text
Role: <role>
Owned workspace: <absolute path>
Do not modify: <other workspaces>

Deliverables:
- <concrete output>

Acceptance:
- <test or observable result>

Dependencies:
- <artifact or none>

After completion:
- run <verification>
- commit if requested
- report commit, tests, remaining risks
- send <artifact> to <session_id> with correlation_id <id> when authorized
```

## Flux Map submission

Use `plan_workflow` with `template="cross_workspace_collaboration_v1"` after session discovery has resolved concrete worker IDs and assignments. The template creates a deterministic two-level DAG:

```
Parent (Map, parallel=N):
  run_agents → fan-in → end

Child per agent (dispatch → await → read → end):
  dispatch  (NodeTool: send_to_session)  → admission {session_id, turn_id, cursor}
  wait      (NodeAwait: external_task + fallback_poll=check_turn) → suspends worker
  read      (NodeTool: read_session)     → session report
  end
```

The `wait` node uses `await_type=external_task` with `fallback_poll` calling `check_turn` every 15 seconds. The AwaitPollWorker polls the target session's event store via the global index; when the target turn reaches `turn_finished`, the await binding completes, the child task is enqueued and resumed by a task worker, and execution continues to `read`. This is non-blocking: child tasks only occupy workers during `dispatch` (~ms) and `read` (~ms), not during the entire turn duration (minutes).

**Auto-approve**: Worker sessions dispatched with `intent="request"` receive an auto-approving approver. The supervisor's DAG approval in `plan_workflow` is the gate; individual tool calls in worker sessions execute without interactive confirmation. If a worker requires user input, it will surface the request through the session's event stream — the supervisor should monitor `workflow_events` for blocked states.

**Submission form**:

```json
{
  "goal": "Implement the independent frontend shell and backend API. Each worker must run its stated checks and report evidence.",
  "template": "cross_workspace_collaboration_v1",
  "parallelism": 3,
  "timeout_ms": 3600000,
  "agents": [
    {
      "role": "frontend",
      "session_id": "<FRONTEND_ID>",
      "workspace_path": "<FRONTEND_WORKSPACE>",
      "message": "<complete frontend assignment with deliverables, acceptance checks, and verification commands>",
      "correlation_id": "<task>/supervisor/frontend/initial/1",
      "intent": "request"
    },
    {
      "role": "backend",
      "session_id": "<BACKEND_ID>",
      "workspace_path": "<BACKEND_WORKSPACE>",
      "message": "<complete backend assignment>",
      "correlation_id": "<task>/supervisor/backend/initial/1",
      "intent": "request"
    },
    {
      "role": "mobile",
      "session_id": "<MOBILE_ID>",
      "workspace_path": "<MOBILE_WORKSPACE>",
      "message": "<complete mobile assignment>",
      "correlation_id": "<task>/supervisor/mobile/initial/1",
      "intent": "request"
    }
  ]
}
```

**Constraints**:
- 1–8 agents per submission. Session IDs and correlation IDs must be unique within the batch.
- `intent` must be `"request"` for auto-approve; `"notification"` skips approval.
- `timeout_ms` caps each child's `wait` node. For implementation tasks budget ≥ 30 minutes (1800000 ms); for analysis tasks 10 minutes (600000 ms) is sufficient.
- Dependent assignments belong in a later template run — do not put a consumer in the same Map batch as its producer.
- Template success proves every mapped turn reached a terminal state. It does NOT independently verify commits, tests, contracts, or integrated behavior. The supervisor must validate acceptance claims before advancing dependent work.

**After submission**: retain the returned `workflow_id`. Use `workflow_status` for progress, `workflow_events` for per-node diagnostics, and `workflow_definition` to inspect the compiled DAG. The Map output carries per-agent results under `nodes.run_agents.output.results`.

## Contract handoff

Require the producer to send a versioned, implementation-independent contract:

```text
Artifact: <name>
Revision: <n>
Producer commit: <sha>
Endpoints or interface: <exact contract>
Error behavior: <exact contract>
Verification: <commands and results>
Consumer action: <what must change or validate>
```

## Final summary

Report:

1. dependency-ordered timeline
2. session and workspace mapping
3. each turn ID and terminal result
4. commits and verification
5. delivered and consumed artifacts
6. integrated acceptance result
7. blocked or retained resources
