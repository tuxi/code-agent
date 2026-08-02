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

Use this form after session discovery has resolved concrete worker IDs and assignments:

```json
{
  "goal": "Implement the independent frontend shell and backend API. Each worker must run its stated checks and report evidence.",
  "template": "cross_workspace_collaboration_v1",
  "parallelism": 2,
  "timeout_ms": 300000,
  "agents": [
    {
      "role": "frontend",
      "session_id": "<FRONTEND_ID>",
      "workspace_path": "<FRONTEND_WORKSPACE>",
      "message": "<complete frontend assignment>",
      "correlation_id": "<TASK>/supervisor/frontend/initial/1",
      "intent": "request"
    },
    {
      "role": "backend",
      "session_id": "<BACKEND_ID>",
      "workspace_path": "<BACKEND_WORKSPACE>",
      "message": "<complete backend assignment>",
      "correlation_id": "<TASK>/supervisor/backend/initial/1",
      "intent": "request"
    }
  ]
}
```

The template validates unique session and correlation IDs, maps agents in parallel, waits on each exact admission cursor, reads each terminal report, and returns the child results in the workflow output. Submit a later template run for dependency stages such as contract consumption or integration; do not put a dependent assignment in the same initial Map batch. This first implementation uses blocking `wait_sessions` inside each child task; native Flux AwaitBinding is the next durability optimization.

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
