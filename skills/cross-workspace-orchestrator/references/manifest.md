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
