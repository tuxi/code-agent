# Background job control v1

The host can observe and stop an individual background command without asking
the model to call a tool. Stopping a conversation Turn does not stop its jobs.
No scheduling, model control flow or automatic retries are added.

## Capability and scope

`background_job_control_v1` is present in the WebSocket hello, and true in
`GET /v1/runtime/capabilities`'s `data.capabilities`, only when the runtime has
the live registry used by its subprocess tools. Unsupported hosts return 404
for these routes. Requests use the existing Runtime authentication, including
authenticated shared-device connections; the same conversation access scope
as existing conversation routes applies. The URL conversation must exist and
must own the requested job.

The registry is process-local. Job IDs are opaque and unique across registry
creation and daemon restarts. Persisted history is not proof that a process is
still alive; an old job absent from the live registry cannot be controlled.

## List

`GET /v1/conversations/{session_id}/jobs` returns the normal API envelope:

```json
{"code":0,"msg":"ok","data":{"jobs":[{
  "job_id":"job_<opaque-id>",
  "session_id":"session-id",
  "turn_id":"origin-turn-id",
  "call_id":"origin-call-id",
  "command":"actual command",
  "status":"running",
  "duration_ms":1234,
  "cancel_requested":false,
  "output_tail":"recent output"
}]}}
```

The response includes every active job owned by the conversation (across
Turns), plus at most its 20 most recent terminal jobs. It is not a history
pagination API. Output tails use the existing UTF-8-safe tail truncation at
2000 bytes. Clients should refresh conservatively while active, and after
initial load, reconnect or stopping a Turn. Log details continue using
`/v1/jobs/{id}/events` and `/v1/jobs/{id}/stream`; do not open every log stream
just to show the task count.

`status` is `running`, `exited`, `failed` or `canceled`. `exit_code` is absent
until cleanup and terminal event publication are complete. A requested stop
remains `running` with `cancel_requested:true`; clients display “Stopping”.
Disconnection or a missing historical task is not a confirmed cancellation.

## Stop one job

`POST /v1/conversations/{session_id}/jobs/{job_id}/cancel` takes:

```json
{"turn_id":"origin-turn-id","call_id":"origin-call-id"}
```

Both origin IDs are required and matched against the registry's owner. Unknown
jobs or mismatched ownership return 404 without executing a cancellation.
Malformed, missing or unknown fields, multiple JSON values and oversized
bodies return 400. Authentication failures use the existing 401 envelope.

The response is `data:{"job":<same snapshot shape>}`. HTTP 202 means stopping
is in progress. HTTP 200 means the job is already terminal, including natural
completion that raced the request. Repeating a valid request is safe. A 200
response is not a claim that the request caused the process to stop. The UI
must use the returned status and subsequent authoritative snapshots/events.

On supported Unix hosts cancellation uses the existing owned process-group
termination and bounded pipe cleanup. No process name matching, global kill
operation, shell command generation or model invocation is used.
