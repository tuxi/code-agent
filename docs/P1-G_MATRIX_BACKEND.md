# P1-G Preview Matrix Backend

Status: backend checkpoint (not final acceptance)
Date: 2026-11-16
Base: code-agent `d20cdaa` (P1-F frozen backend)
Scope: `ui_preview_matrix.v1` aggregation of independent `ui_preview` revisions

P1-G turns several independent preview revisions into one verifiable matrix. It
reads the P1-F contracts and never rewrites them: the frozen `ui_preview`
result, `ui_preview_lineage.v1`/`.v2`, and persisted artifact metadata stay
byte-identical, and every completed cell carries the exact lineage the runtime
validated.

## What this phase delivers

| Artifact | Purpose |
|---|---|
| `schemas/ui_preview_matrix.v1.schema.json` | Frozen-shape matrix document: identity, targets, states, entries, coverage, digest |
| `internal/preview/matrix/matrix.go` | Document types, cell statuses, canonical digest |
| `internal/preview/matrix/build.go` | Layout resolution, cell aggregation, per-cell failure recording |
| `internal/preview/matrix/validate.go` | Schema validation plus cross-field matrix rules |
| `internal/preview/matrix/store.go` | Idempotent workspace persistence of matrix revisions |
| `internal/preview/matrix/manifest.go` | Stable, path-free manifest projection |

Out of scope: generating the five form-factor HTML variants, calling a model per
target, an AgentKit matrix UI, and any production UI.

## Document

```json
{
  "schema": "ui_preview_matrix.v1",
  "matrix_id": "matrix_...",
  "source_asset": {"asset_id": "asset_...", "sha256": "<64 hex>"},
  "layout": {"artifact_id": "layout_...", "revision": 1, "revision_sha256": "sha256:..."},
  "evidence_sha256": "<64 hex> | null",
  "target_matrix": {"id": "matrix_...", "targets": ["ipad_landscape", "iphone_portrait"]},
  "states": ["default", "loading"],
  "entries": [{"target": "...", "state": "...", "status": "completed|failed|missing"}],
  "coverage": {"targets": 2, "states": 2, "expected": 4, "completed": 2, "failed": 1, "missing": 1, "complete": false},
  "matrix_sha256": "sha256:..."
}
```

`target_matrix.id` equals `matrix_id`; the pair is the matrix identity a preview
lineage binds to through `lineage.target_matrix`.

### Canonical form

- `target_matrix.targets` is sorted and unique. Targets outside the
  `ui_preview_lineage.v2` enum are rejected, even when the layout declares them.
- `states` keeps the layout's `screen.states` declaration order.
- Entries are exactly one per `target × state` cell, in target-then-state order.
  A cell can therefore never be silently omitted.
- `matrix_sha256` is
  `sha256(UTF-8(RFC 8785 JCS(matrix without matrix_sha256)))`, the same digest
  construction the preview runtime uses for `revision_sha256`.

Two builds from the same layout revision, cells, and preview revisions produce
identical bytes, entries, coverage, and digest.

## Entries

| Field | completed | failed | missing |
|---|---|---|---|
| `preview_artifact_id` | required | optional (recorded revision) | null |
| `preview_revision` | required | optional | null |
| `preview_revision_sha256` | required | optional | null |
| `preview_mode` | `adaptation` | `adaptation` / `explore` | null |
| `lineage` | complete `ui_preview_lineage.v2` | null | null |
| `fallback_asset` | required identity | null | null |
| `error` | null | required `{code, message}` | null |

A failed cell records the failure and its error code/message, optionally the
revision that failed, but never a lineage, fallback, or viewer. A missing cell
claims nothing. A completed cell must be an `interactive_html` adaptation
preview whose artifact status is `ready` and whose fallback asset resolves
inside the artifact.

`error.code` is a closed set: the P1-F lineage kinds (`lineage_required`,
`lineage_invalid`, `lineage_mismatch`, `lineage_unavailable`) plus the frozen
preview error codes (`preview_*`). Messages are bounded to 1024 bytes.

## Validation rules

Document level (`ValidateDocument`, applied to every built and every persisted
matrix):

1. JSON Schema for `ui_preview_matrix.v1`.
2. `target_matrix.id == matrix_id`.
3. Targets are sorted, unique, and belong to the lineage target enum.
4. Entry count equals `len(targets) × len(states)`; entries follow canonical cell
   order, which makes cell uniqueness structural.
5. `completed` cells: preview identity and mode present, mode `adaptation`,
   complete v2 lineage, fallback identity, no error.
6. `failed` cells: error present, no lineage, no fallback, all-or-nothing preview
   identity.
7. `missing` cells: no preview identity, lineage, fallback, or error.
8. Every completed lineage binds the matrix layout artifact/revision/digest, the
   matrix source asset id/digest, and the matrix evidence digest (or null).
9. Every completed lineage's `target` and `state` equal the entry's cell; a
   preview for one target cannot stand in for another.
10. Every completed lineage's `target_matrix.id` equals `matrix_id` and its
    target set equals the matrix target set.
11. No preview artifact or preview revision is referenced by more than one cell.
12. `coverage` equals the actual entry census, with
    `complete == (completed == expected)`.
13. `matrix_sha256` matches the recomputed digest.

Build level (`Builder.Build`):

- The layout revision is loaded from
  `.codeagent/assets/ui-layout/<layout_id>/revision-<n>/`, its persisted
  metadata and document digests are verified, and `layout.json` is checked
  against `ui_layout.v1`.
- Request targets and states must be declared by that layout revision.
- Request cells must stay inside the requested matrix; duplicate cells,
  duplicate artifact references, and malformed references are rejected.
- A matrix request failure (`matrix_invalid`, `matrix_layout_*`) produces no
  document. A per-cell problem is recorded as a failed entry and the matrix is
  still produced.

## Failure kinds (build)

| Code | Meaning |
|---|---|
| `matrix_invalid` | Request, layout document, or document validation problem |
| `matrix_layout_unavailable` | Layout revision files are missing or unreadable |
| `matrix_layout_invalid` | Layout document fails `ui_layout.v1` or declares unsupported identity |
| `matrix_layout_mismatch` | Persisted layout identity/digest disagrees with its bytes or the request |
| `matrix_store_unavailable` | Preview store or matrix store is unavailable |

## Persistence

`Persist` writes one matrix revision under

```
.codeagent/assets/ui-preview-matrix/<matrix_id>/revision-<000001>/
  matrix.json      # ui_preview_matrix.v1
  manifest.json    # ui_preview_matrix_manifest.v1
  metadata.json    # kind, revision, digest, coverage, created_at
```

The manifest is the stable, path-free projection: matrix id/digest, layout and
source identity, evidence digest, targets, states, coverage, and per-entry
status/identity. It is byte-identical across replays.

Persistence is idempotent by matrix digest: replaying an identical build reuses
the stored revision (`created=false`) instead of appending a new one. A changed
document gets the next revision number. A stored revision whose bytes no longer
match its metadata is ignored rather than reused, so corruption cannot pass as a
replay.

## Verification

```bash
go build ./...
go vet ./internal/preview/... ./schemas/...
go test -count=1 ./internal/preview/... ./internal/tools/preview/... \
  ./internal/uicontract/... ./internal/tools/uilayout/... ./schemas/...
```

Coverage in `internal/preview/matrix`:

- Positive: full matrix, subset matrix, missing cell, declared failure, failed
  artifact, evidence-bound matrix, replay stability, build+persist reuse.
- Negative: unknown/invalid target and state, duplicate cell, duplicate artifact,
  failure with preview, unknown/empty error, invalid matrix/session identity,
  missing revision, tampered layout bytes, layout identity and source mismatch,
  invalid layout document, unsupported layout target, store failure, missing
  artifact, explore artifact, and ten lineage-mismatch cases (target, state,
  layout revision, layout digest, source id, source digest, matrix id, target
  set, evidence dropped, evidence digest).
- Document drift: coverage, omitted cell, cell order, duplicate cell, missing
  lineage/fallback, failed-cell fabrication, missing-cell claims, lineage
  binding, artifact sharing, unknown error code, digest tampering, malformed
  JSON, unknown fields, wrong schema.
- Golden: `testdata/golden/matrix.json` and `manifest.json` for a 2×2 matrix with
  two completed cells, one failed cell, and one missing cell.

## Known limitations

- The model-callable binding lives in `docs/P1-G_MATRIX_TOOL.md`
  (`internal/tools/previewmatrix`). This document remains the backend
  aggregation contract.
- A subset matrix is a distinct target set: its previews must declare the same
  `target_matrix.targets` in their lineage, otherwise every cell fails with
  `lineage_mismatch` (by design).
- `approval` remains recorded provenance from P1-F; the matrix does not add a
  server-side approval event.
- P1-F's known unrelated failures in `internal/agent` and `internal/runtime` are
  unchanged.
