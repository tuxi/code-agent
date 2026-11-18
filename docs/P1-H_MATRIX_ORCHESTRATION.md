# P1-H Model-Driven Preview Matrix Orchestration

Status: checkpoint implementation (real-model acceptance recorded separately)
Date: 2026-11-17
Base: P1-G frozen backend (`aeccd49`), `ui_layout`, `ui_preview`, `ui_preview_matrix`
Scope: model-driven orchestration of independent preview cells. No frozen schema,
P1-F/P1-G contract, AgentKit, ClientToolsKit, or production UI change.

## What this phase adds

P1-G can aggregate independent preview revisions. P1-H defines how the main
model produces those revisions without weakening the frozen backend:

- one reviewed `ui_layout.v1` revision for the whole matrix;
- one independent `ui_preview` call per target × state cell;
- one cell-specific `artifact_key`, one lineage v2 per cell;
- one final `ui_preview_matrix` aggregation with every cell exactly once;
- explicit completed / failed / missing outcomes;
- single-cell retry, cancel/resume, and partial matrices;
- coverage derived by the backend from the actual entries.

The runtime still never generates HTML. The main model authors each cell's
viewer and fallback. The backend only validates, persists, resolves references,
aggregates, and reports coverage/state.

## Skill contract

The orchestration lives in the `ui-adaptation` system skill (version 7):

- `skills/ui-adaptation/SKILL.md` §7;
- `skills/ui-adaptation/references/docs/preview-matrix-orchestration.md`;
- mirrored reference at `docs/preview-matrix-orchestration.md`.

Core rules:

1. Freeze `matrix_id`, sorted targets, state order, and one stable artifact key
   per cell before drawing.
2. Create one layout revision and reuse its identity in every cell lineage.
3. Each cell calls `ui_preview` with its own lineage target/state and the same
   matrix target set. No preview is reused across cells.
4. A failed cell records the tool error and does not stop other cells. A missing
   cell carries no preview reference and no error.
5. Retry only the failed cell. Use the latest successful revision in the final
   matrix; do not re-run completed cells.
6. Aggregate once with `ui_preview_matrix`. Failed cells do not fail the matrix;
   request-level errors are fixed and retried.
7. On cancel, aggregate attempted cells as a partial matrix. On resume, reuse
   completed `preview_ref`s and generate only failed/missing cells.
8. Never state coverage that disagrees with the entries.

## First acceptance matrix

One source image, one layout revision, 2 targets × 2 states:

```text
targets: ["duo_folded", "iphone_portrait"]
states:  ["default", "loading"]
cells:   duo_folded/default, duo_folded/loading,
         iphone_portrait/default, iphone_portrait/loading
```

Acceptance must exercise the real visual-model chain:

```text
view_image -> inspect_ui_image -> ui_layout -> ui_preview (per cell)
           -> ui_preview_matrix
```

The run must include at least:

- one failed-then-retry cell;
- one missing cell;
- completed, failed, missing and coverage counts consistent with entries;
- a failed cell that leaves the other cells intact;
- a cancel/partial matrix followed by a resume;
- one matrix block per revision in AgentKit, with authenticated viewer/fallback
  requests and no token or absolute-path leakage.

## Out of scope

- frozen schemas and P1-F/P1-G validation rules;
- backend HTML generation;
- five-form automatic generation;
- production SwiftUI/UIKit;
- any change to AgentKit, ClientToolsKit, or Talkify.

## Verification

```bash
go build ./...
go test -count=1 ./internal/systemskills/
go run ./cmd/system-skills --root . \
  --manifest skills/system-skills.json \
  --output internal/systemskills/bundle --check
```

The system skill test asserts that the matrix orchestration section and the
canonical 2×2 reference remain present in the embedded bundle. Independent
acceptance is performed by a separate thread against the checkpoint commit.
