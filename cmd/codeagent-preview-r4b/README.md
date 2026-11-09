# R4b Persistent Live Preview Runtime

`codeagent-preview-r4b` starts a persistent authenticated Runtime session for the
R4b cross-repository harness. It does not use the R3 fixture server and does not
inject preview events.

The process:

1. Creates a real SQLite session/event store.
2. Creates the real preview service and generated fallback PNG.
3. Starts the production `server.NewMux`, `TurnExecutor`, Agent Wire stream,
   bearer authentication, viewer/asset routes, and replay route.
4. Prints a readiness JSON record as its first stdout line.
5. Waits for a normal client turn. The exact `turn_prompt` deterministically calls
   the server-side `ui_preview` tool through the production Agent loop.

## Start

```sh
go run ./cmd/codeagent-preview-r4b
```

For a stable workspace/state directory:

```sh
go run ./cmd/codeagent-preview-r4b \
  --workspace /tmp/codeagent-r4b-workspace \
  --state-dir /tmp/codeagent-r4b-state
```

The process remains alive until `SIGINT` or `SIGTERM`.

Readiness example:

```json
{
  "base_url": "http://127.0.0.1:54321",
  "bearer_token": "<64 lowercase hex characters>",
  "session_id": "session_r4b_<16 hex characters>",
  "turn_prompt": "R4B_TRIGGER_PREVIEW_v1: use ui_preview to render the Backend R4b live readiness preview."
}
```

Every HTTP and WebSocket request requires:

```text
Authorization: Bearer <bearer_token>
```

The sole exception is `GET /healthz`, which is public for
`RuntimeHTTPClient.healthCheck()` and returns exactly `ok`.

`GET /v1/runtime/models` is authenticated and reports a complete
`r4b-scripted` connection/model with `available=true`, valid loopback BaseURL,
`default_model=r4b-scripted/r4b-scripted`, and
`default_runtime_alias=r4b-scripted`.

## Trigger sequence

1. Read the first stdout line as readiness JSON.
2. `GET /v1/conversations/{session_id}/events?since=0` with bearer auth.
   The pre-trigger response is an empty array and the cursor is `0`.
3. Connect `GET /v1/conversations/{session_id}/stream?since=0` over WebSocket.
4. Send a normal Agent Wire turn with `type=agent_input`, `kind=text`, and
   `text` equal to the readiness `turn_prompt`.
5. Read live events until `kind=preview_artifact`.
6. The live preview sequence must be greater than the pre-trigger `nextSince`.
7. Replay `/events?since=0`; it must contain the same artifact ID and revision
   with the same `revision_sha256`.
8. Fetch the artifact's revision-addressed viewer/asset URLs. Authentication,
   MIME, size, SHA-256, CSP, and PNG decoding must all pass.

The deterministic provider only calls `ui_preview` for the exact
`turn_prompt`. Other prompts do not synthesize preview artifacts.

## AgentKit `PREVIEW_R4_READY`

Start the runtime with stdout captured:

```sh
go run ./cmd/codeagent-preview-r4b > /tmp/preview-r4b-ready.json 2>/tmp/preview-r4b.log &
```

Wait until the file is non-empty, then export the first JSON line:

```sh
export PREVIEW_R4_READY="$(head -n1 /tmp/preview-r4b-ready.json)"
```

The AgentKit R4b harness reads `base_url`, `bearer_token`, `session_id`, and
`turn_prompt`. After connecting with `DefaultAgentClient`, it must send
`turn_prompt` through a normal `AgentInput` text turn rather than waiting for the
server to inject an event.

## Verify

```sh
go test ./cmd/codeagent-preview-r4b -count=1
```

The integration tests exercise the production HTTP/WebSocket server with
bearer auth, public health, the complete model catalog, a normal turn, live
`preview_artifact`, replay, viewer, asset, digest, and PNG decoding.
