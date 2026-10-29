# Tool validation and development execution evidence

This change addresses the second P0-A experiment's missing `edit_file.path`,
approval wait, and incomplete validation evidence. It adds no Task database,
Flux workflow, UI, automatic approval, or change to the generic loop's stop rule.

## Dispatcher boundary

`tools.ValidateInput` validates JSON and the tool's existing `InputSchema` before
pre-tool hooks, Inspector, permission checks, client dispatch, or Execute. Both
ordinary batches and `ExecuteNestedTool` use it. Existing reference handles are
resolved before validating the resulting wire value; reference safety checks
remain in place. Malformed JSON also returns the structured argument error.
The provider's earlier history/envelope validation still rejects malformed
model responses before dispatch; this change does not relax persisted-history
invariants or make malformed provider envelopes resumable. Missing fields and
other schema-invalid JSON objects reach the correctable Dispatcher path.

Validation uses the already-present `github.com/google/jsonschema-go` dependency,
including required fields, types, enums, arrays, nested objects, local references,
alternatives and constraints. No remote schema loader is installed. A tool without
a schema retains syntax-only validation. An absent argument body is treated as an
empty object for zero-argument legacy tools. No defaults or coercion are applied.

Missing `path` produces a tool result, recorded in `tool_finished.output` and
returned to the model without contacting an approver:

```json
{"error":{"code":"invalid_tool_arguments","tool":"edit_file","field":"path","message":"required field is missing","retryable_by_model":true}}
```

Other schema violations use the same code; ambiguous errors may omit `field`.
Invalid/unresolvable schemas are `invalid_tool_schema`, not model-retryable.
Messages omit input values. An invalid client call emits a server-side error
lifecycle, never `tool_started(executor=client)`, which is a device execution
instruction. Unknown tools and existing reference errors retain their own errors.

Schema validation does not grant permission, inspect the filesystem, or prove a
path is safe. Inspector, protected paths, workspace containment, approval and the
tools' execution-time checks continue to enforce those boundaries. Empty strings
remain valid unless a tool's schema explicitly constrains them. `propose_plan`'s
schema now accurately permits its existing inline-plan fallback when `plan_path`
is omitted; its readiness, planning-state, file and approval checks are unchanged.

## MCP attention and experiment driver

`codeagent_read_session` and `codeagent_wait_sessions` return:

| Status | terminal | attention_required | Driver behavior |
| --- | --- | --- | --- |
| running / queued / waiting | false | false | Keep listening |
| waiting_for_human | false | true | Show attention and pending control references |
| paused | false | true | Return control for intervention |
| completed | true | false | Final result/evidence can be collected |
| failed | true | false | Stop and show the error |
| cancelled | true | false | Stop waiting |

`attention` contains `session_id`, `turn_id`, `reason`, and `controls` references
(`id`, `type`, optional `tool_name`). Original request payloads remain in
`pending_controls`. Known foreign session/turn controls are filtered. Runtime
activity is checked even if the live connection has not delivered the request;
references can be empty when only the runtime's pending count is available.
Current runtimes do not send a per-request resolution frame, so cached payloads
are best-effort session/turn hints until the pending count clears.

Event cursors limit returned events, not lifecycle state. Completion and evidence
are derived from the full exact-turn stream, so advancing a read cursor cannot
turn completed into waiting or hide a prior edit.

The reusable `mcpbridge.AwaitCompletion` driver boundary returns immediately on
attention, pause, failure, cancellation or unknown state, without approving or
reading artifacts. The opt-in live driver writes `state.json` and
`attention-or-stop.json` on intervention. Only completion permits final artifact
collection. The artifact endpoint can still return diagnostic artifacts for
failed/cancelled turns; it rejects active and attention states.

## Optional development evidence contract

A delegating caller may declare expected checks when sending a new turn:

```json
{
  "session_id": "session-id",
  "request_id": "implementation-1",
  "message": "Implement the reviewed change and run the checks.",
  "development": {
    "validation_commands": [
      "go test ./internal/example -count=1",
      "go vet ./internal/example"
    ]
  }
}
```

The declaration does not execute commands or inject a loop policy. Omitted
`development` preserves the existing single-agent/ordinary conversation behavior.
The contract is bounded, copied, and part of the request's idempotency identity;
changing it under the same request ID is rejected. P0-A keeps it process-local,
with no restart recovery or new persistence guarantees.

`internal/executionevidence.Check` is a pure shared checker for future adapters.
MCP reads and artifacts expose its `execution_evidence` report:

- `collecting`: the turn is still running/queued/waiting.
- `attention_required`: human control is pending.
- `needs_validation`: a completed turn has missing/failed/stale checks, unfinished
  calls, unresolved argument errors, ambiguous evidence, or no declared checks;
  failed/cancelled turns also cannot become evidence-complete.
- `evidence_complete`: all declared foreground checks have recorded zero exits
  after the last recorded mutation, with no remaining blockers.

The checker pairs call IDs, requires the reported command to match its invocation,
and rejects missing exits, running/background jobs, denied commands and later
failed reruns. Validation must start after the last mutation finishes, covering
overlap as well as ordinary edit/test order. File mutation tools count as changes
even on error, because a failed tool may partially write. Shell commands outside
the declared checks conservatively invalidate prior validation (e.g. gofmt).
Declare non-mutating validation commands; execute formatting first.

A successful retry can resolve an argument error only for the same tool and
matching original intent, with the offending top-level field corrected. Ambiguous
JSON/type/nested errors without a usable field remain conservatively unresolved.
This can require further human review; no error is silently discarded.

Only the current turn's recorded tool work is observable. Out-of-band writers,
older turns' work, test coverage, correctness and unchanged-workspace proofs are
not certified. `task_acceptance` remains `not_reviewed` even when evidence is
complete. An upper-level Task may display needs_validation independently of the
turn lifecycle; this change does not create that Task layer.

## Offline checks and live opt-in

The scripted stdio MCP test uses the actual Runtime/SQLite/HTTP/WebSocket stack,
reproduces missing-path input, returns a correctable error, repairs the call,
changes a disposable clone, runs a foreground command and checks actual Diff and
evidence. The existing hypothesis reminder remains enabled; the script reissues
the corrected mutation after that reminder. This is a protocol test, not a claim
about real-model coding quality.

Run targeted tests with network downloads disabled and without provider secrets.
Loopback integration tests need permission to bind a local test port. No real
model is required. `TestRunnerNativeLoop` now requires `CODEAGENT_LIVE_TESTS=1`
as well as a key; an inherited key alone is not a live-test opt-in.
`TestLiveDaemonRoundTrip` still requires `CODEAGENT_P0A_LIVE=1`, explicit isolated
paths/settings, and now `CODEAGENT_P0A_VALIDATION_COMMANDS` containing a nonempty
JSON array of checks. It rejects needs_validation without pretending the turn
failed. These flags do not replace user authorization for source disclosure and
model budget; no third real-model experiment is included in this change.
