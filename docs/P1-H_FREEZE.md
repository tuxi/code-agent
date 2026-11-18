# P1-H Closeout Freeze

Status: P1-H closeout freeze: PASS
Date: 2026-11-18
Scope: model-driven preview matrix orchestration, matrix UI, fragment guidance,
and the paired system skill bundle. No schema, preview backend, AgentKit UI, or
Talkify production changes.

## Closeout

| Item | Result |
|---|---|
| Real-model 2×2 matrix orchestration | **PASS** |
| Partial / resume / retry / missing | **PASS** |
| `ui_preview` error → matrix error kind mapping | **PASS** |
| Fragment-only `viewer_html` guidance (v8) | **PASS** |
| First-attempt fragment acceptance | **PASS** |
| macOS Web Workbench matrix UI | **PASS** (harness scope) |
| iOS native timeline matrix UI | **PASS** (harness scope) |
| Bundle-identical (canonical / embedded / AgentKit `.system`) | **PASS** |
| `go build`, skill tests, matrix/tool tests | **PASS** |
| AgentKit resource check | **PASS** |
| AgentKit main / Talkify production UI | **Not changed** |

## Pinned commits

| Repository | Commit | Role |
|---|---|---|
| code-agent | `1407fa6` | main merge of P1-H and fragment guidance |
| code-agent | `2640541` | P1-H matrix orchestration and error-kind mapping |
| code-agent | `836eb0e` | fragment guidance, v8, bundle `2026.11.18.1` |
| AgentKit | `1d48926` | `ui_preview_matrix_result.v1` matrix UI |
| AgentKit | `dcd1896` | paired system skill resource checkpoint (branch `p1-h-resource-checkpoint`, based on `1d48926`) |
| ClientToolsKit | `4b79e6e` | real `inspect_ui_image` / `ui_evidence.v1` producer |
| FileViewerKit | `4f4c70d` | pinned client file surface |
| Talkify | `accb4f9` | `uiPreviewMatrix` inspector destination |

The AgentKit resource checkpoint is intentionally on a branch based on
`1d48926`; AgentKit `main` does not contain the P1-H matrix UI and was not
modified by this closeout.

## Real-model 2×2 acceptance

Model: `deepseek/deepseek-flash` with `tool_image_input=true`. The model ran the
full chain through `load_skill`, `view_image`, `inspect_ui_image`, `ui_layout`,
one `ui_preview` per cell, and `ui_preview_matrix`. The client
`inspect_ui_image` was executed by the pinned ClientToolsKit evidence producer.

Matrix identity: `matrix_dreamlog_duo_iphone`, targets
`["duo_folded","iphone_portrait"]`, states `["default","loading"]`.

### Phase 1: partial (revision 1)

| Cell | Status | Reference |
|---|---|---|
| `duo_folded/default` | completed | `preview_798aba61cc57e9f331e4b810` r1 |
| `duo_folded/loading` | failed | `preview_artifact_missing` |
| `iphone_portrait/default` | missing | none |
| `iphone_portrait/loading` | missing | none |

- Coverage: `expected=4, completed=1, failed=1, missing=2, complete=false`.
- Matrix digest:
  `sha256:8ae36126be204756ee1e1b3162719e83f768a1697203d547297bef18b537702e`.

### Phase 2: resume / final (revision 2)

| Cell | Status | Reference |
|---|---|---|
| `duo_folded/default` | completed | reused `preview_798aba61cc57e9f331e4b810` r1, not regenerated |
| `duo_folded/loading` | failed | `preview_artifact_missing` after two `not_found` attempts |
| `iphone_portrait/default` | completed | `not_found` then retry `preview_5097fc2d22882c4cf161b24c` r1 |
| `iphone_portrait/loading` | missing | unattempted; no reference and no error |

- Coverage: `expected=4, completed=2, failed=1, missing=1, complete=false`.
- Matrix digest:
  `sha256:c030e90ab468b1e813f24242f134e6f65558ca1113206755311eebc5c1638773`.
- Replay does not duplicate a matrix revision; one client block per revision.
- Completed cells have distinct artifact IDs; each lineage binds its own
  target/state and the shared layout revision, source, evidence, and target set.

## Error mapping

- `ui_preview` tool failure `not_found` is not a matrix error kind.
- A matrix entry using `error.code="not_found"` is rejected with
  `invalid_arguments`; no matrix is persisted.
- The failed cell is recorded as `preview_artifact_missing` while the message
  preserves the raw `not_found` and fallback path.
- macOS and iOS UIs render the mapped frozen kind and do not fabricate a viewer,
  fallback, or lineage for the failed cell.

## Fragment guidance (ui-adaptation v8)

`ui_preview.viewer_html` is a self-contained body fragment. Full document shells
(`<!doctype html>`, `<html>`, `<head>`, `<body>`) are forbidden.
`preview_security_violation` is handled by rewriting the fragment, never by
changing the schema, adding an external resource, or using a bridge.

The guidance also restricts `href`/`src` to `data:` URLs. `href="#"` and
relative navigation are rejected even though they look local; inert actions use
`<button type="button">`.

### First-attempt acceptance

Final v8 skill, real model, one minimal adaptation cell:

- `ui_preview` calls: 1
- `viewer_html` starts with `<style>`; no forbidden document tags, `href`, or
  `src`
- tool result: `failure=none`
- preview artifact: `preview_48deb899a7646561ac5c39eb` r1 `ready`
- `preview_security_violation` events: 0

An earlier exploratory run used `<a href="#">`; the tool returned “Prepared…” but
the preview materialization rejected it with `preview_security_violation`. The
`data:`-only `href`/`src` rule and a navigation regression fixture were added
before the final acceptance run.

### Bundle identity

- `bundle_version`: `2026.11.18.1`
- `ui-adaptation` version: `8`
- Skill digest:
  `sha256:3c59931776911e06c2e1a339a5d007e33c392d80b267af154b2cacca305b43d1`
- `SKILL.md`:
  `sha256:18685ba127f9f65aa9494abfda1df7056c1b0d143c4baba70f2bd81e28c513c2`
- `preview-matrix-orchestration.md`:
  `sha256:9dbc4872eb1d8c3f09c9e5a239cb80e63941ddb6c59fdf7d5e6904f7bcfe985e`
- Manifest:
  `sha256:09836e465b876e81ba1f4e372c32076c70177d72e52818ee0d562379ffd03fde`

## Matrix UI verification

- macOS Web Workbench: matrix grid, cell viewer, fallback thumbnail, lineage,
  layout/evidence digest, failed-cell mapped error, replay dedupe, and
  token/path safety. Rendered from the real recorded session through the pinned
  AgentKit `ConversationWebWorkbenchView`.
- iOS native timeline: 2×2 matrix block, cell sheet with interactive viewer,
  failed-cell `preview_artifact_missing`, and lineage/layout/evidence facts.
  Rendered in an iOS Simulator harness using the pinned AgentKit public
  `ConversationDetailView` / `ConversationViewModel`.
- Viewer and fallback bytes were fetched through the authenticated resolvers;
  the request log contains session/artifact/revision/asset/digest only, with no
  bearer token, absolute path, or URL.

### Harness scope

The UI evidence comes from the pinned AgentKit test host and an iOS Simulator
harness, not from the Talkify production shell. It replays the real runtime
session and uses live resolver requests; it does not start a new model turn in
the UI. The headless `inspect_ui_image` build used pinned ClientToolsKit
`InspectUIImageTool` / `UIEvidence` / `ToolWorkspace` sources with
`AnalyzeLocalImageTool` excluded because its CoreML resource compile is blocked
by the local toolchain sandbox. The tool source itself is unmodified.

## Runtime entry limitation

`codeagent serve` does not wire the preview producer. Under it, `ui_preview`
returns a prepared result but no preview artifact is materialized, so the matrix
cannot resolve the cell. `codeagentd` wires `PreviewProducer` and is the
supported entry for preview and matrix work. This closeout used `codeagentd`.

## Verification

```bash
go build ./...
go test ./internal/systemskills/... ./internal/skills/...
go test -count=1 ./internal/preview/... ./internal/tools/preview/ \
  ./internal/tools/previewmatrix/ ./internal/tools/uilayout/ ./internal/uicontract/
go run ./cmd/system-skills --check \
  --agentkit-resources <AgentKit>/Sources/AgentKit/Resources/skills/.system
```

All commands passed. The canonical, embedded, and AgentKit `.system` bundles are
byte-identical (`diff -r`), and the AgentKit
`BundledSkillsInstallerTests` resource suite passed (8/8).

## Frozen contracts and out of scope

- Matrix schema, preview backend, lineage contracts, and error-kind set are
  unchanged.
- AgentKit UI source and Talkify production code are unchanged.
- The AgentKit resource checkpoint is not merged into AgentKit `main`.
- Five-form-factor generation, a production matrix UI rollout, and final
  production UI acceptance remain out of scope for this freeze.

## Evidence

Local acceptance evidence is under
`/Users/xiaoyuan/p1h-acceptance/run/evidence/`:

- `P1H_ACCEPTANCE_REPORT.md` — 2×2 acceptance report
- `acceptance-evidence.json` — machine-readable matrix/model/UI evidence
- `events-20261117-165809-c3652a64.jsonl` — real model events
- `viewer-fallback-requests.jsonl` — resolver request log
- `repo-integrity.txt` — five-repository byte identity
- `P1H_SKILL_FRAGMENT_REPORT.md` — v8 fragment guidance report
- `minimal-acceptance.json`, `minimal-cell-first-attempt.png` — first-attempt
  fragment acceptance
- macOS and iOS screenshots for grid, viewer, and failed cell
