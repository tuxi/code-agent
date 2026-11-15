# P1-F Backend and macOS UI Freeze

Status: frozen for backend and macOS UI; iOS preview presentation deferred
Date: 2026-11-14 (backend); updated 2026-11-15 after the macOS UI acceptance and viewer fixes
Scope: P1-F preview lineage, presentation and viewer. No five-form generation matrix (that is P1-G).

## Platform acceptance status

| Surface | Status |
|---|---|
| Backend P1-F: tools, lineage validation, preview service, filesystem store, viewer/asset resolution, replay | **PASS** |
| AgentKit macOS UI: preview cards, adaptation/explore labels, revision 1/2, v2 lineage facts, target/state/matrix, approval, layout/evidence Inspector, fallback, evidence card, live/replay, one Block per revision, negative suppression | **PASS** |
| Interactive viewer (Mac): request issued, viewer bytes served, iframe rendered for revisions 1 and 2 | **PASS** |
| iOS Runtime transport: HTTPS + bearer pairing, server persistence, session list, event replay, tool rendering | **PASS** |
| iOS preview presentation: inline preview card, interactive viewer, fallback presentation, lineage Inspector entry | **Not implemented (deferred)** |
| Full cross-platform freeze | **Not claimed** |

iOS was exercised against the same acceptance Runtime over HTTPS with a trusted CA: the app paired with the server, switched to it, listed the remote workspace and its sessions, replayed the P1-F event stream, and rendered the turns and their tool activity. It does **not** render the P1-F inline preview card or viewer: the iOS conversation uses the native timeline, while the card and viewer are implemented only in the macOS Web workbench. No iOS viewer request occurs. P1-F is therefore frozen **for macOS and backend only**; iOS preview presentation is deferred until it is implemented and must not be counted as a cross-platform pass.

Viewer defects found during the macOS acceptance were fixed in AgentKit on `main`:

| Commit | Fix |
|---|---|
| `458b5d2` | Every viewer/asset resolution failure now answers the page with an explicit error code; late preview artifacts refresh the bridge snapshot; the page times out after 10 s and shows the error plus Retry above the fallback. |
| `d2edab2` | Projection correlates each tool result with the artifact revision its own lineage and fallback identity describe, not the newest revision of a reused call ID. |
| `d81ab99` | `RuntimeSnapshot` keeps the full accepted revision history so replayed turns and exact-revision viewer requests can resolve earlier revisions; latest-only semantics remain for current-state consumers. |

## Pinned versions

| Repo | Commit | Role |
|---|---|---|
| code-agent | `20e2374` | frozen backend |
| AgentKit | `e610a36` (acceptance build); `d81ab99` (viewer fixes on `main`) | macOS presentation |
| ClientToolsKit | `4b79e6e` | UI inspection tools |
| Talkify | `eb90777` | pinned acceptance build; the user's final verification used the current working tree |

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

macOS UI acceptance ran against the same real backend through the Runtime HTTP/WebSocket path, using the real P1-E artifacts, and covered the positive matrix above plus the negative sessions. Full evidence: `chater` acceptance report for P1-F (2026-11-15), including runtime viewer request logs, artifact/revision digests, and the app-side negative results.

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

- iOS preview presentation (inline card, viewer, fallback, lineage Inspector): not implemented on the native iOS timeline; deferred (see Platform acceptance status).
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
