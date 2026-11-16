# P1-G Preview Matrix Tool Binding

Status: checkpoint (independent backend acceptance passed; layout-unavailable sanitization checkpoint 2026-11-16)
Date: 2026-11-16
Base: code-agent `48601e1`
Scope: model-callable `ui_preview_matrix` built-in over the merged backend

This phase binds the `ui_preview_matrix.v1` backend to the agent loop. It does
not change the P1-F frozen schemas, the lineage v1/v2 contracts, the preview
artifact contract, or the merged matrix backend semantics.

## Tool

| Field | Value |
|---|---|
| Name | `ui_preview_matrix` |
| Package | `internal/tools/previewmatrix` |
| Binding | `model_visibility=direct`, `executor=server`, `capability=ui-adaptation` |
| Input schema | `Tool.InputSchema()` (closed object) |
| Output schema | `schemas/ui_preview_matrix_result.v1.schema.json` |

The tool is registered with the built-ins in
`internal/runtime/registry.go`, so the existing `agent.builtin_tools` allowlist
gates it exactly like `ui_layout` and `ui_preview`.

## Input

```json
{
  "matrix_id": "matrix_demo",
  "layout": {"artifact_id": "layout_demo", "revision": 1, "revision_sha256": "sha256:..."},
  "target_matrix": {"id": "matrix_demo", "targets": ["ipad_landscape", "iphone_portrait"]},
  "states": ["default", "loading"],
  "entries": [
    {"target": "ipad_landscape", "state": "default", "preview_ref": {"artifact_key": "ui-adaptation:ipad-default"}},
    {"target": "ipad_landscape", "state": "loading", "preview_ref": {"artifact_id": "preview_...", "revision": 1, "revision_sha256": "sha256:..."}},
    {"target": "iphone_portrait", "state": "default", "error": {"code": "preview_render_failed", "message": "..."}}
  ]
}
```

`preview_ref` is controlled:

- `artifact_key` is the key the model passed to `ui_preview`. The runtime derives
  `artifact_id` from `(session_id, "ui-adaptation", "ui_preview", artifact_key)`,
  so a model cannot forge an ID or name another session's artifact.
- `artifact_id` + `revision` is accepted for revisions a client already holds.
  It is resolved through the current session's store only; a foreign or unknown
  ID never resolves.
- `revision` with `artifact_key` pins an explicit revision; otherwise the latest
  revision is used.
- `revision_sha256` is optional. When present it must equal the stored
  `revision_sha256`; a mismatch fails that cell with `preview_digest_mismatch`.

No input field accepts a path. `workspace_root`, absolute paths, and unknown
fields are rejected.

## Resolution and rejection rules

| Condition | Outcome |
|---|---|
| Malformed input, path-shaped artifact ID, `preview_ref` + `error`, both or neither of `artifact_key`/`artifact_id` | request failure, kind `invalid_arguments` |
| `target_matrix.id != matrix_id` | request failure, kind `invalid_arguments` |
| layout revision digest disagrees with the requested `revision_sha256` | request failure, kind `invalid_arguments`; nothing is persisted |
| cell target/state outside the matrix | request failure, kind `invalid_arguments` (backend rule) |
| one artifact or artifact@revision used by two cells | request failure, kind `invalid_arguments` (backend rule) |
| reference not found in this session, cross-session ID, corrupt stored revision | failed cell with `preview_artifact_missing` / `preview_invalid_artifact`; the cell is preserved |
| supplied digest mismatch | failed cell with `preview_digest_mismatch`; the cell is preserved |
| lineage target/state/layout/source/evidence/matrix mismatch | failed cell with `lineage_mismatch` (backend rule) |
| artifact status failed/unavailable | failed cell with the artifact's recorded error (backend rule) |
| no `preview_ref` and no `error` | missing cell, claiming nothing (backend rule) |

Every expected cell is present exactly once. Failed and missing cells are never
silently omitted, and coverage always equals the actual entry census.

## Model-visible result

There are two model-facing channels, both stable:

1. `ToolResult.Output` is the structured `ui_preview_matrix_result.v1`
   projection:

- matrix identity: `matrix_id`, `matrix_revision`, `matrix_sha256`,
  `created`, and the workspace-relative `matrix_path`;
- manifest identity: `manifest_schema`, `manifest_sha256`,
  workspace-relative `manifest_path`;
- layout/source identity, evidence digest, targets, states, coverage;
- one entry per cell with `status`, the resolved `preview_ref`
  (`artifact_id`, `revision`, `revision_sha256`), `preview_mode`, the fallback
  asset identity, and the typed `error`;
- `error_types`: sorted unique failure kinds across failed cells.

2. `ToolResult.Content` is the text the agent loop commits to the model
   transcript. It carries the same identity: matrix revision/digest and
   workspace-relative paths, layout and source identity, and one line per cell
   with the resolved `preview_id`/`revision`/`preview_sha256`, preview mode,
   fallback asset id/digest, and error code/message. A follow-up call can be made
   from the text alone; the structured output remains available to clients.

Only workspace-relative paths appear in either channel, so the model can follow
the matrix or manifest with ordinary read tools. Absolute paths and credentials
never appear.

## Execution-context wiring

`tools.ExecutionContext` now carries `PreviewStore preview.Store`. The agent
loop populates it from the runner's `PreviewProducer` when that producer
implements `agent.PreviewStoreProvider` (satisfied by `preview.Service`). A host
without a preview service leaves it nil and the tool degrades with
`unavailable` instead of resolving anything.

## Verification

```bash
go build ./...
go vet ./internal/tools/previewmatrix/ ./internal/preview/... ./internal/agent/ ./internal/runtime/ ./schemas/
go test -count=1 ./internal/tools/previewmatrix/ ./internal/preview/... \
  ./internal/tools/preview/ ./internal/tools/uilayout/ ./internal/uicontract/ \
  ./internal/agent/ ./internal/runtime/ ./schemas/
```

Tests cover:

- positive: backend-key refs, explicit artifact-ID refs, replay reuse, persisted
  matrix/manifest digest agreement, schema validation;
- negative: target-matrix mismatch, cell outside the matrix, shared artifact,
  layout digest mismatch, malformed refs (absolute path, missing revision, both
  or neither handle, bad digest, ref+error), unknown fields, missing preview
  store;
- failure preservation: cross-session ref, nonexistent revision, corrupt stored
  revision, wrong lineage, failed revision, missing cell;
- model-visible contract: no absolute paths or credentials, and completed cells
  carry an actionable `preview_ref` plus fallback asset identity;
- golden: `internal/tools/previewmatrix/testdata/golden/result.json` for a 2×2
  matrix with two completed cells, one failed cell, and one missing cell.

## Checkpoint: model-visible layout-unavailable sanitization (2026-11-16)

Independent backend acceptance over `27c19dc` passed 32/33. The only failure was
a request-level `matrix_layout_unavailable` result whose `ToolResult.Content`
carried the absolute revision path from `os.Lstat`.

Change:

- `internal/preview/matrix.Error` now separates `Message` (model-safe) from a
  runtime-only `Diagnostic()`. Path-bearing layout load and matrix persistence
  causes are recorded only in the diagnostic; `Error()` stays path-free.
- `ui_preview_matrix` projects every `matrix_layout_unavailable` failure to the
  stable `layout revision metadata is unavailable` and every store-unavailable
  failure to `preview store is unavailable`. No absolute path, `.codeagent`
  path, or raw `os` error text reaches `ToolResult.Content` or `Failure`.
- Regression tests cover nonexistent revisions, non-regular metadata, permission
  errors, and revision-directory symlink escapes in both
  `internal/preview/matrix` and `internal/tools/previewmatrix`.

Verification:

- 33/33 acceptance scenarios pass on the fixed tree, including N10 (layout
  unavailable, no model-visible leak) and all persistence/replay scenarios.
- `go test -count=1 ./internal/preview/... ./internal/tools/previewmatrix/
  ./internal/tools/preview/ ./internal/tools/uilayout/ ./internal/uicontract/`
  passes.
- No matrix/P1-F schema, AgentKit, Talkify, or frozen fixture changes.

## Out of scope

Five-form-factor HTML generation, an AgentKit matrix UI, and production UI work
remain out of scope, matching the backend checkpoint.
