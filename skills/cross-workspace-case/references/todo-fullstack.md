# Case 1: Todo frontend/backend collaboration

## Setup

Use two clean Git repositories and two persistent sessions:

- frontend: static HTML/CSS/JavaScript, no package install required
- backend: Go standard-library HTTP service

Record `<FRONTEND_ID>`, `<BACKEND_ID>`, both workspace paths, and initial commits.

## Backend assignment

```text
Implement the Todo Demo backend in the current workspace.

Provide GET /api/health, GET /api/tasks, POST /api/tasks with {"title":"..."},
and PATCH /api/tasks/{id} with {"completed":true}. Use in-memory storage,
listen on 127.0.0.1:18080, support CORS, add Go tests and startup documentation.

Do not modify the frontend workspace. Run go test ./... and commit the result.
Then send the final request/response/error contract to <FRONTEND_ID> with
correlation_id todo-case/backend/frontend-contract and intent request.
Report the commit, tests, endpoint, and contract handoff turn.
```

## Frontend assignment

```text
Implement the Todo Demo frontend in the current workspace using plain
HTML/CSS/JavaScript. Display tasks, add tasks, toggle completion, and render
loading, empty, and error states. Default the API base URL to
http://127.0.0.1:18080 and allow it to be overridden.

Do not modify the backend workspace. Complete and commit the independent UI
shell first. When the backend contract arrives in a later turn, reconcile the
adapter with the contract, run real GET/POST/PATCH smoke tests, commit the
integration, and report both commits and verification results.
```

## Acceptance evidence

- Both reported workspace paths match their assigned repositories.
- The frontend first turn reaches a terminal state before the contract turn.
- The backend contract carries the declared correlation ID and sender session.
- The frontend contract turn has a new turn ID and a cursor after its first turn.
- Both repositories have independent commits and clean status after completion.
- Backend unit tests pass.
- Real health, list, create, and update requests pass against the running server.
- The browser-visible frontend uses backend data rather than fixtures.
- Re-reading both sessions shows the assignment, contract, and final reports.

## Failure probes

- Stop the backend owner and confirm send fails as offline without losing the request context.
- Reuse a stable create/fork request ID with a changed payload and confirm conflict.
- Send the contract while the frontend is busy and confirm `delivery=queued` without steering the active turn.
- Restart the Runtime after both commits and confirm both sessions remain discoverable and readable.
