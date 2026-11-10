# R4d DreamLog CLI/TUI Acceptance

Status: accepted for Backend CLI/TUI evidence
Date: 2026-11-10
Contract: `preview_contract_v1`

## Scope

This record covers the code-agent CLI/TUI acceptance path for the corrected
DreamLog Duo creation-home preview. It does not claim AgentKit, Talkify, or
production AgentKit UI acceptance.

The acceptance turn used the deterministic R4a provider to invoke the real
`ui_preview` tool through the production Agent loop. Preview materialization,
JCS hashing, event persistence, replay, viewer/asset resolution, and trusted
viewer handoff were the production code paths. No R3 fixture server was used.

## Inputs

Preview HTML:

`/Users/xiaoyuan/Documents/work/git/AgentKit/docs/design/previews/dreamlog-duo-creation-home/current.html`

```yaml
size_bytes: 446779
sha256: sha256:23695af5fa0187abfed70046fc26a2920d35ee3313da33a52eee05180b300817
```

Native fallback:

`/Users/xiaoyuan/.codex/visualizations/2026/11/10/01a1c122-56cf-7d32-bac9-ef202f0d8677/r4d-dreamlog-native/outer-screen_light.png`

```yaml
size_bytes: 2613515
sha256: sha256:a6e29942e9dffd12daae6a6f1522c52a1a5a44aa49fda8d880834aea0a8ef8f7
```

The HTML contains 14 inline SVG icons and no remote resource/navigation action.

## Artifact and Event

```yaml
session_id: 20261110-131904-dd5d22be
turn_id: turn_18e6b19275109948_1
capability: ui-adaptation
producer_id: ui_preview
artifact_id: preview_d1bb8cbccdf57f455575723b
revision: 1
revision_sha256: sha256:7dba466335732d88af70952a630cca7f6619b7e958ba0e85fed4ef2b29ab55de
event_kind: preview_artifact
event_seq: 9
```

Replay contains the same artifact ID, revision, digest, and sequence.

## Viewer and Asset

Viewer:

```yaml
url: /v1/conversations/20261110-131904-dd5d22be/previews/preview_d1bb8cbccdf57f455575723b/revisions/1/viewer
size_bytes: 447430
sha256: sha256:d98522f4ae12369fbdd153c4464023f2f66f6513551e51ba2fa3d07a872e6993
```

Fallback asset:

```yaml
asset_id: asset_turn_18e6b19275109948_1_faux_tc_0_0_000_ed19f6f0
url: /v1/conversations/20261110-131904-dd5d22be/previews/preview_d1bb8cbccdf57f455575723b/revisions/1/assets/asset_turn_18e6b19275109948_1_faux_tc_0_0_000_ed19f6f0/content
size_bytes: 2613515
sha256: sha256:a6e29942e9dffd12daae6a6f1522c52a1a5a44aa49fda8d880834aea0a8ef8f7
```

## R4d Copy

```text
turn_prompt: R4D_TRIGGER_PREVIEW_v1: use ui_preview to render the corrected DreamLog Duo creation home preview.
final_answer: R4d CLI acceptance completed. The corrected DreamLog Duo preview artifact was persisted and verified through the trusted viewer resolver.
```

## Screenshots

Evidence root:

`/Users/xiaoyuan/.codex/visualizations/2026/11/10/01a1bedc-c0c3-7842-9275-519148257db8/r4d-codeagent-rev2/`

Screenshots are JPEG bytes with `.jpg` names:

```yaml
outer:
  path: dreamlog-outer-screen.jpg
  sha256: sha256:1163ea12f69e0117dd47622d719b501cd8991cacd700d7feda6257b696469a6d
portrait:
  path: dreamlog-inner-portrait.jpg
  sha256: sha256:d1c319dd9f6b9e6ffd97ef13c2fcc3f751d4f13439dd6d97e531a776e68c039a
landscape:
  path: dreamlog-inner-landscape.jpg
  sha256: sha256:ee7592da80ddbef70faa67b09d4fc6e78aca2cdab09a725684b7d1333b1ee509
```

The outer screenshot shows all 14 inline icons. The inner-landscape screenshot
uses the corrected two-column layout.

## Validation

```text
go test ./cmd/codeagent-preview-r4a ./cmd/codeagent-preview-r4b -count=1
go test ./cmd/codeagent/tui -run Preview -count=1 -v
go test ./internal/preview/r4input ./internal/preview ./internal/preview/fixturetest -count=1
go build ./...
git diff --check
```

All commands passed. TUI preview tests passed 3/3. Frozen preview fixtures
remained 16/16.

## Supersession

The earlier evidence directory:

`/Users/xiaoyuan/.codex/visualizations/2026/11/10/01a1bedc-c0c3-7842-9275-519148257db8/r4d-codeagent/`

is explicitly marked superseded in `SUPERSEDED.md`. Its old HTML and viewer
hashes are not acceptance evidence for this record.

## Boundaries

- Frozen Preview Contract v1.6 was not modified.
- Frozen preview fixtures were not modified.
- AgentKit and Talkify were not modified by the Backend acceptance work.
