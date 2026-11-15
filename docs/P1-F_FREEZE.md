# P1-F Cross-Platform Freeze

Status: P1-F cross-platform functional freeze: PASS
Date: 2026-11-14 (backend); updated 2026-11-15 after the macOS and iOS UI acceptance
Scope: P1-F preview lineage, presentation and viewer. No five-form generation matrix (that is P1-G).

## Closeout

P1-F cross-platform functional freeze: PASS

Known stability debt:

- failed ui_preview tool-group expansion main-thread stall
- transient first viewer/asset 404 on session switch/replay

## Platform acceptance status

| Surface | Status |
|---|---|
| Backend P1-F: tools, lineage validation, preview service, filesystem store, viewer/asset resolution, replay | **PASS** |
| AgentKit macOS UI: preview cards, adaptation/explore labels, revision 1/2, v2 lineage facts, target/state/matrix, approval, layout/evidence Inspector, fallback, evidence card, live/replay, one Block per revision, negative suppression | **PASS** |
| Interactive viewer (Mac): request issued, viewer bytes served, iframe rendered for revisions 1 and 2 | **PASS** |
| iOS Runtime transport: HTTPS + bearer pairing, server persistence, session list, event replay, tool rendering | **PASS** |
| iOS preview presentation: inline preview card, interactive viewer, fallback presentation, lineage Inspector entry | **PASS** |
| Full cross-platform freeze | **PASS** |

P1-F.1 iOS acceptance used clean archives at code-agent `20e2374`, AgentKit `ea683e1`, ClientToolsKit `4b79e6e`, Talkify `511c2c5`, and FileViewerKit `dce9bb5`, connected to the acceptance Runtime over HTTPS with Bearer authentication. The native iOS timeline rendered adaptation and explore cards, revisions 1 and 2, target/state/matrix and lineage digests, fallback PNGs, the authenticated viewer, lineage/layout/evidence Inspector documents, live and replayed events, one Block per revision, the viewer-404 error + Retry + fallback path, and both required negative paths. No source or frozen fixture was modified.

Presentation and viewer work recorded in AgentKit on `main`:

| Commit | Fix |
|---|---|
| `458b5d2` | Every viewer/asset resolution failure now answers the page with an explicit error code; late preview artifacts refresh the bridge snapshot; the page times out after 10 s and shows the error plus Retry above the fallback. |
| `d2edab2` | Projection correlates each tool result with the artifact revision its own lineage and fallback identity describe, not the newest revision of a reused call ID. |
| `d81ab99` | `RuntimeSnapshot` keeps the full accepted revision history so replayed turns and exact-revision viewer requests can resolve earlier revisions; latest-only semantics remain for current-state consumers. |
| `ea683e1` | The native iOS timeline renders the validated `ui_preview_result.v1` card, authenticated viewer, fallback and lineage Inspector; viewer HTML is fetched with the bearer token in the Authorization header only. |

## Pinned versions

| Repo | Commit | Role |
|---|---|---|
| code-agent | `20e2374` | frozen backend |
| AgentKit | `ea683e1` | P1-F.1 native iOS preview card; includes the viewer fixes above |
| ClientToolsKit | `4b79e6e` | UI inspection tools |
| FileViewerKit | `dce9bb5` | local Talkify dependency |
| Talkify | `511c2c5` | clean-archive P1-F.1 iOS acceptance build |

## Independent acceptance

Backend acceptance ran against an isolated copy of code-agent `20e2374` using the real P1-E source, evidence, layout, and artifact fixtures.

- Positive matrix: **7/7 passed**
- Negative matrix: **28/28 passed**
- Two independent runs produced identical outcomes, revisions, and revision digests.

Confirmed invariants:

- Adaptation previews require `ui_preview_lineage.v2`; v1 lineage is rejected for adaptation.
- Explore previews must not carry lineage at the tool result, draft, or artifact metadata level.
- Rejected requests create no preview artifact, no revision, no fallback bytes, and no workspace writes.
- Lineage is bound to one specific layout revision: a rev2 lineage carrying a rev1 digest fails.

macOS UI acceptance ran against the same real backend through the Runtime HTTP/WebSocket path, using the real P1-E artifacts, and covered the positive matrix above plus the negative sessions. iOS P1-F.1 acceptance ran the native timeline against an HTTPS + Bearer Runtime and covered the preview cards, viewer, fallback, Inspector documents, live/replay, one Block per revision, the viewer-404 path and the negative sessions. Full evidence: the macOS and iOS P1-F acceptance reports (2026-11-15), including runtime viewer request logs, artifact/revision digests, and the app-side negative results.

Known stability debt is recorded in **Closeout**; it does not change the functional freeze or any frozen contract.

## Frozen contracts

### Lineage v1

`schemas/ui_preview_lineage.v1.schema.json` remains byte-for-byte frozen at the `71fd16a` content. It stays valid for existing consumers only and cannot authorize an adaptation preview.

### Lineage v2

`schemas/ui_preview_lineage.v2.schema.json` is the adaptation contract. It binds:

- layout artifact ID, revision, and revision SHA-256;
- source asset ID and source asset SHA-256;
- evidence SHA-256, nullable only when the layout revision records no evidence;
- exactly one `target` and one `state`;
- the `target_matrix` context.

Before preview creation the runtime verifies the persisted layout revision digest, layout source identity, current source-image bytes, the bound evidence file when present, and target/state membership in the layout.

### Result and metadata

`schemas/ui_preview_result.v1.schema.json` remains the stable `ui_preview` output: `mode` (`adaptation` | `explore`), the complete v2 lineage or `null`, and the fallback asset identity/SHA.

Persisted artifact metadata carries `preview_mode` and `ui_preview_lineage`. Both participate in `revision_sha256`, so replay returns the recorded lineage with the exact revision. Artifact validation rejects adaptation metadata without lineage, explore metadata with lineage, and lineage without a mode.

Failure kinds are frozen as `lineage_required`, `lineage_invalid`, `lineage_mismatch`, and `lineage_unavailable`.

## Out of scope

- The two known stability items in **Closeout**: the failed `ui_preview` tool-group expansion main-thread stall and the transient first viewer/asset 404 on session switch/replay. They are recorded debt, not part of this freeze.
- The five-form generation matrix (`P1-G`): not implemented here.
- `approval` is recorded provenance; there is no server-side user approval event source.

## Change policy

Any change to the v1/v2/result schemas, lineage validation rules, preview metadata keys, failure kinds, or revision digest participation requires a new phase and a new acceptance round. The v1 schema must remain byte-identical. A P1-G matrix manifest is a separate contract and must not modify these frozen artifacts.

## Recorded verification

```bash
go build ./...
go test -count=1 ./internal/preview/... ./internal/tools/preview/... ./internal/uicontract/...
```

AgentKit (after viewer fixes): `PreviewWorkbenchTests` 7/7, `UIPreviewPresentationTests` 15/15, `PreviewWorkbenchRenderTests` 10/10, `ConversationWeb*` 49/49.

iOS P1-F.1: clean-archive simulator build succeeded at the pinned versions above; the real HTTPS + Bearer acceptance covered the cards, revisions, lineage/digest facts, viewer, fallback, Inspector documents, live/replay, one Block per revision, the viewer-404 path and both required negative paths. Evidence: the iOS P1-F.1 acceptance report (2026-11-15).
