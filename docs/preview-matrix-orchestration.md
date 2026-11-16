# Preview Matrix Orchestration

Status: P1-H model-driven orchestration reference
Applies to: `ui_layout` + `ui_preview` + `ui_preview_matrix`

This document is the worked example for the "Generate the preview matrix"
section of `ui-adaptation`. The backend validates, persists, and aggregates; the
main model authors every preview cell. The runtime never generates HTML.

## Matrix identity

For the first P1-H round:

```json
{
  "matrix_id": "matrix_dreamlog_duo_iphone",
  "target_matrix": {
    "id": "matrix_dreamlog_duo_iphone",
    "targets": ["duo_folded", "iphone_portrait"]
  },
  "states": ["default", "loading"]
}
```

This is a 2 × 2 matrix with four cells:

| cell | artifact_key |
|---|---|
| `duo_folded/default` | `matrix-dreamlog-duo_folded-default-r1` |
| `duo_folded/loading` | `matrix-dreamlog-duo_folded-loading-r1` |
| `iphone_portrait/default` | `matrix-dreamlog-iphone_portrait-default-r1` |
| `iphone_portrait/loading` | `matrix-dreamlog-iphone_portrait-loading-r1` |

Use one stable key per cell for its whole lifetime. A retry either keeps the key
(new revision) or uses a new key that still belongs only to that cell.

## 1. Layout revision

Create one `ui_layout.v1` revision for the source, both targets and both states.
The result supplies the exact identity every cell reuses:

```json
{
  "layout": {
    "artifact_id": "layout_dreamlog_home",
    "revision": 1,
    "revision_sha256": "sha256:<64 hex>"
  },
  "source_asset": {"asset_id": "asset_...", "sha256": "<64 hex>"},
  "evidence_sha256": "<64 hex or null>"
}
```

Do not create one layout per target. A matrix with mixed layout revisions is
invalid.

## 2. One `ui_preview` call per cell

Each call is independent. The lineage must name the layout revision, source,
evidence and the cell's exact target/state:

```json
{
  "artifact_key": "matrix-dreamlog-duo_folded-default-r1",
  "mode": "adaptation",
  "title": "DreamLog Home · Duo folded · default",
  "viewer_html": "<self-contained HTML>",
  "fallback_path": "source.png",
  "lineage": {
    "schema": "ui_preview_lineage.v2",
    "layout_artifact_id": "layout_dreamlog_home",
    "layout_revision": 1,
    "layout_revision_sha256": "sha256:<layout digest>",
    "source_asset_id": "asset_...",
    "source_asset_sha256": "<source digest>",
    "evidence_sha256": "<evidence digest or null>",
    "approval": {"state": "not_required"},
    "target": "duo_folded",
    "state": "default",
    "target_matrix": {
      "id": "matrix_dreamlog_duo_iphone",
      "targets": ["duo_folded", "iphone_portrait"]
    }
  }
}
```

Repeat for `duo_folded/loading`, `iphone_portrait/default`, and
`iphone_portrait/loading`. Never point two cells at one artifact key or revision.

## 3. Status tracking

```text
duo_folded/default         completed  artifact_key=...duo_folded-default-r1
duo_folded/loading         failed     error=preview_render_failed
iphone_portrait/default    completed  artifact_key=...iphone_portrait-default-r1
iphone_portrait/loading    missing    (no preview attempt)
```

A failed cell keeps the tool's error. A missing cell has no preview reference and
no error. A completed cell has both a preview reference and a fallback.

## 4. Retry one cell

Suppose `iphone_portrait/default` first fails because its fallback path is
missing:

```text
ui_preview(fallback_path="missing-fallback.png") -> not_found
```

Retry only that cell:

```json
{
  "artifact_key": "matrix-dreamlog-iphone_portrait-default-r2",
  "mode": "adaptation",
  "lineage": { "...": "same layout/source/evidence, target=iphone_portrait, state=default" },
  "fallback_path": "source.png"
}
```

Other cells are not re-run. If the retry succeeds, the final matrix uses the
retry revision. If it fails again, the final matrix records the latest error.

Do not retry capability refusals with unchanged capabilities, and do not reuse
`duo_folded`'s revision to make the cell look completed.

## 5. Final aggregation

One call, all four cells:

```json
{
  "matrix_id": "matrix_dreamlog_duo_iphone",
  "layout": {
    "artifact_id": "layout_dreamlog_home",
    "revision": 1,
    "revision_sha256": "sha256:<layout digest>"
  },
  "target_matrix": {
    "id": "matrix_dreamlog_duo_iphone",
    "targets": ["duo_folded", "iphone_portrait"]
  },
  "states": ["default", "loading"],
  "entries": [
    {
      "target": "duo_folded",
      "state": "default",
      "preview_ref": {"artifact_key": "matrix-dreamlog-duo_folded-default-r1"}
    },
    {
      "target": "duo_folded",
      "state": "loading",
      "error": {"code": "preview_render_failed", "message": "..."}
    },
    {
      "target": "iphone_portrait",
      "state": "default",
      "preview_ref": {"artifact_key": "matrix-dreamlog-iphone_portrait-default-r2"}
    },
    {
      "target": "iphone_portrait",
      "state": "loading"
    }
  ]
}
```

Expected coverage: `expected=4`, `completed=2`, `failed=1`, `missing=1`,
`complete=false`. The backend computes these counts from the entries; never
report different numbers.

## 6. Cancel and resume

### Cancel

If the user cancels after two cells, do not start new cells. Immediately
aggregate the partial matrix:

```text
completed: duo_folded/default
failed:    duo_folded/loading (if already attempted)
missing:   iphone_portrait/default, iphone_portrait/loading
```

The completed revisions remain in the preview store. Do not delete or rewrite
them.

### Resume

Read the previous matrix result or
`.codeagent/assets/ui-preview-matrix/<matrix_id>/revision-<n>/matrix.json`.
Reuse each completed entry's `preview_ref`; generate only the failed/missing
cells with new per-cell keys; aggregate a new matrix revision containing all
four entries. If the layout revision changed, start a new matrix identity.

If a previous `preview_ref` is unavailable, regenerate that cell. Never guess an
artifact ID and never cross-wire another cell's revision.

## 7. Invariants

- One matrix block per matrix revision in the client; one cell per target/state.
- Each completed lineage's target/state equals its cell.
- Every completed lineage binds the matrix's layout, source, evidence and target
  set.
- No preview artifact is shared by two cells.
- Completed + failed + missing equals the entry census.
- Failed and missing cells never fabricate a viewer, fallback, or lineage.
- The matrix path and manifest are workspace-relative and path-free.
