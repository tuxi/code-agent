# P1-F Preview Lineage Backend

Status: backend frozen 2026-11-14; see `docs/P1-F_FREEZE.md`. Independent acceptance passed 7/7 positive and 28/28 negative. macOS UI acceptance passed 2026-11-15 after the AgentKit viewer fixes; iOS preview presentation is deferred (native iOS timeline has no inline card/viewer); the iOS Runtime transport itself was verified.
Date: 2026-11-14
Base: code-agent `71fd16a`
Scope: `ui_preview_lineage.v2` backend only; v1 remains frozen

P1-F binds a UI adaptation preview to the reviewed `ui_layout.v1` revision. It
does not redesign the preview runtime, add a five-form generation matrix, or
add AgentKit lineage UI.

## Delivered

- `schemas/ui_preview_lineage.v1.schema.json` remains byte-for-byte frozen.
  `schemas/ui_preview_lineage.v2.schema.json` is the P1-F adaptation contract and
  binds `source_asset_id` / `source_asset_sha256`, `evidence_sha256`,
  `layout_artifact_id` / `layout_revision` / `layout_revision_sha256`, one
  `target`, one `state`, and the `target_matrix` context.
- `schemas/ui_preview_result.v1.schema.json` is the stable `ui_preview` output
  side-channel: `mode` (`adaptation` | `explore`), the complete lineage or
  `null`, and the fallback asset identity.
- `internal/preview/lineage.go` validates the schema, cross-field identity, and
  the persisted layout revision. Before a preview draft is accepted it checks:
  - layout revision identity and digest;
  - layout document source identity;
  - current source image bytes against `source_asset_sha256`;
  - evidence digest against the layout revision and the current evidence file
    when the layout recorded an evidence path;
  - `target` in the layout's `responsive.targets`;
  - `state` in the layout's `screen.states`.
- `ui_preview` defaults to `mode=adaptation`, requires a v2 lineage, and rejects
  v1 lineage for adaptation previews. Standalone previews must pass
  `mode=explore` and must not carry lineage.
- Validated lineage and mode are stored in preview artifact metadata under
  `ui_preview_lineage` and `preview_mode`. Both are part of
  `revision_sha256`, so replay returns the recorded lineage with the exact
  revision. Artifact validation rejects adaptation metadata without lineage,
  explore metadata with lineage, and lineage without a mode.
- HTML sandbox, fallback PNG, viewer/asset/revision addressing, immutability,
  expiry, and replay behavior are unchanged.
- R4a/R4b acceptance producers now explicitly declare `mode=explore`.

## Failure kinds

`lineage_required`, `lineage_invalid`, `lineage_mismatch`, and
`lineage_unavailable`. A rejected lineage never creates a preview artifact.

## Verification

- `go build ./...`
- `go vet ./internal/preview/... ./internal/uicontract/... ./internal/tools/preview/... ./internal/tools/uilayout/... ./schemas/...`
- `go test -count=1 ./internal/preview/... ./internal/uicontract/... ./internal/tools/preview/... ./internal/tools/uilayout/... ./internal/server/ ./internal/capability/`

Positive and negative coverage includes: v1 compatibility and the v1/v2 boundary;
valid and invalid v2 lineage fixtures; valid and invalid `ui_preview_result.v1`
fixtures; adaptation default without lineage; v1 lineage rejected for adaptation;
explore with lineage; unknown lineage and input fields; target and state not
declared by the layout; tampered source image; tampered persisted layout
revision; evidence digest mismatch; missing or mismatched source identity;
artifact metadata mode/lineage coupling; persistence and replay of lineage
metadata; and existing preview fixture, security, revision, and replay suites.

## Known limitations

- `internal/agent` `TestRunTurnWithAssetsPreservesUserGatewayAsset` and several
  `internal/runtime` Flux/event-store tests fail on unmodified `71fd16a` in this
  environment; they are unrelated to P1-F.
- The five-form generation matrix is not implemented.
- No AgentKit lineage presentation or production UI change is included.
- `approval` is recorded provenance; P1-F does not add a server-side user
  approval event source.
