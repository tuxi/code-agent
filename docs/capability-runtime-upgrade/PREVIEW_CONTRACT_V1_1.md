# Preview Contract v1.1

Status: superseded by PREVIEW_CONTRACT_V1_2.md
Date: 2026-11-09
Contract ID: `preview_contract_v1`
Supersedes: [PREVIEW_CONTRACT.md](./PREVIEW_CONTRACT.md)
Owners: code-agent backend + AgentKit GUI
Reviewer: independent integration/security reviewer

## 1. Authority

This document is the single source of truth for the preview contract. If another
upgrade document conflicts with this file, this file wins until the next
versioned contract replaces it.

The v1 draft is retained only for review history.

## 2. Changes from v1 Draft

Version 1.1 resolves the R0 blockers:

- Adds a typed producer contract from tools to the runtime.
- Uses the existing Agent Wire event envelope (`kind`, not `type`).
- Defines runtime-owned artifact IDs and revision allocation.
- Makes viewer and fallback bytes revision-addressed and immutable.
- Defines a sanitized preview asset view with no absolute filesystem paths.
- Freezes viewer transport, authentication, digest, MIME, size, and caching rules.
- Defines the status/kind/fallback matrix and client-side presentation state.
- Unifies capability names and AND semantics.
- Splits tool visibility from tool execution mode.
- Makes sandboxed iframe the primary v1 renderer and closes bridge isolation.
- Keeps preview-to-runtime actions out of v1.
- Makes fixtures an implementation prerequisite.

## 3. Non-negotiable Rules

1. Backend and AgentKit implement this contract independently.
2. No behavior may be inferred from a tool name or from arbitrary `output`.
3. Talkify does not implement generic preview parsing or rendering.
4. AgentKit renders generic previews directly in ConversationWorkbench.
5. Preview JavaScript never shares the main ConversationWorkbench DOM or bridge.
6. Artifact and asset bytes are immutable by revision.
7. Every preview event is a complete self-contained snapshot.
8. Preview failures never fail the containing turn.
9. Deferred Tool Discovery, Tool Search, Code Mode, preview navigation, and
   preview action bridge are post-v1.

## 4. Capability Negotiation

### 4.1 Canonical capability strings

Runtime `hello` and client `client_capabilities` use the same strings:

```text
preview_contract_v1
preview_interactive_html_v1
preview_static_asset_v1
preview_replay_v1
```

### 4.2 Direction and semantics

- `preview_contract_v1` declares support for this contract.
- `preview_interactive_html_v1` is required on both sides to render iframe preview.
- `preview_static_asset_v1` is required on both sides to render static evidence.
- `preview_replay_v1` is required on both sides for historical replay and is a
  contract baseline, not a client-only extension.

Runtime may advertise interactive/replay only when the artifact store, viewer
materializer, authenticated API, and replay path are all available.

### 4.3 Degraded behavior

| Runtime | Client | Result |
| --- | --- | --- |
| interactive + static | interactive + static | Inline interactive preview with static fallback |
| static only | static | Static evidence card |
| interactive only | static | Static fallback |
| unavailable | any | Unavailable presentation state |

Missing preview capabilities affect only the preview block. The turn, tool card,
assets, evidence, and assistant answer remain intact.

### 4.4 Transport

- Runtime capability declaration: existing runtime hello/capability snapshot.
- Client declaration: existing `client_capabilities` message, extended with the
  canonical strings above.
- Scope: connection/session as implemented by the existing Agent Wire. The
  effective capability set is the intersection of runtime and client support.
- Unknown capability strings are ignored.
- Absence means unsupported.

## 5. Producer Contract

### 5.1 Typed preview drafts

Tools may submit a typed preview draft via a dedicated side-channel. They must
not encode previews in `ToolResult.Output` or rely on tool names.

Server-side logical contract:

```go
type PreviewDraft struct {
    Kind          string
    Title         string
    Summary       string
    Capability    string
    ViewerHTML    string
    AssetIDs      []string
    FallbackAsset string
    Presentation  *PreviewPresentationHint
}
```

The runtime validates the draft, materializes a viewer when required, snapshots
assets, assigns `artifact_id` and `revision`, and stamps all correlation IDs.

### 5.2 Producer matrix for v1

| Producer | V1 support |
| --- | --- |
| Server-side built-in tool | Supported through typed `PreviewDraft` |
| Client-executed tool | Supported through explicit `previews` field in client tool result |
| MCP tool | Not automatically promoted in v1; MCP preview support is post-v1 |

Client tool preview drafts use the same logical fields over Agent Wire. Asset IDs
must reference already authorized client assets or runtime snapshots. Inline
base64 preview content is not a v1 wire mechanism.

### 5.3 Forbidden shortcuts

- Parsing `output.kind`, `output.type`, or `metadata` to discover previews.
- Mapping specific tool names to preview types.
- Letting tool results choose final artifact IDs or revisions.
- Accepting viewer URLs or filesystem paths directly from tools.

## 6. Event Envelope

V1 uses the existing Agent Wire event shape:

```json
{
  "kind": "preview_artifact",
  "event_id": "optional-transport-id",
  "seq": 123,
  "session_id": "session_...",
  "turn_id": "turn_...",
  "call_id": "call_...",
  "artifact": { "...": "PreviewArtifact" }
}
```

Frozen rules:

- `kind` is `preview_artifact`; `type` is not accepted on raw Agent Wire.
- `seq` is required for persisted replay.
- `session_id` is the canonical wire identity. `conversation_id` must not be
  duplicated in the artifact.
- `turn_id` is required.
- `call_id` is optional; if present in both envelope and artifact, values must
  match.
- The event is persisted and returned by `/events` replay with the same semantics.
- Unknown contract/artifact versions degrade the preview block only.

`seq` orders events. `revision` selects the current version of one artifact.

## 7. Preview Artifact

```json
{
  "schema_version": "preview.artifact.v1",
  "artifact_id": "preview_...",
  "revision": 1,
  "status": "ready",
  "kind": "interactive_html",
  "title": "DreamLog Duo Home",
  "summary": "Outer, inner portrait, and inner landscape previews.",
  "capability": "ui-adaptation",
  "session_id": "session_...",
  "turn_id": "turn_...",
  "call_id": "call_...",
  "created_at": "2026-11-09T12:00:00Z",
  "updated_at": "2026-11-09T12:00:00Z",
  "expires_at": null,
  "presentation": {
    "placement": "conversation_inline",
    "preferred_aspect_ratio": 1.78,
    "min_height": 420,
    "initial_state": "collapsed"
  },
  "viewer": {
    "url": "/v1/sessions/session_.../previews/preview_.../revisions/1/viewer",
    "mime_type": "text/html",
    "size_bytes": 182304,
    "sha256": "sha256:..."
  },
  "assets": [
    {
      "asset_id": "asset_...",
      "role": "fallback",
      "mime_type": "image/png",
      "size_bytes": 220123,
      "sha256": "sha256:...",
      "content_url": "/v1/sessions/session_.../previews/preview_.../revisions/1/assets/asset_.../content",
      "width": 1400,
      "height": 900,
      "alt": "DreamLog Duo inner landscape layout"
    }
  ],
  "fallback": {
    "asset_id": "asset_...",
    "text": "Interactive preview unavailable. Showing static evidence."
  },
  "security": {
    "profile": "preview.security.v1",
    "javascript": "isolated_only",
    "network": "deny",
    "navigation": "deny",
    "persistent_data": "deny",
    "actions": "none"
  },
  "error": null,
  "metadata": {}
}
```

### 7.1 Identity

- `artifact_id` is runtime-owned and session-scoped.
- `revision` is a positive monotonically increasing integer.
- `session_id`, `turn_id`, and `call_id` are runtime-stamped.
- Tool drafts cannot override correlation IDs.

### 7.2 Presentation

`presentation` is a typed, allowlisted hint object:

- `placement`: v1 only `conversation_inline`.
- `preferred_aspect_ratio`: positive number.
- `min_height`: bounded positive integer.
- `initial_state`: `collapsed` or `expanded`.

AgentKit owns layout and final sizing. Unknown presentation fields are ignored
unless marked required by a future schema.

### 7.3 Metadata

`metadata` is non-behavioral, JSON only, size-limited, and path/secret-free. It
must not control rendering or security behavior.

## 8. Status and Kind Matrix

| Status / kind | Required fields | Client behavior |
| --- | --- | --- |
| `pending` | identity, kind, title | Show pending card; viewer optional |
| `ready` + `interactive_html` | viewer, security, fallback asset | Render iframe with static fallback |
| `ready` + `static_image` | at least one primary image asset | Render static card |
| `ready` + `evidence_bundle` | assets + manifest | Render evidence card |
| `failed` | `error`; fallback optional | Show failed state; use fallback if present |
| `unavailable` | `error` or reason | Show unavailable card |
| `expired` | `expires_at`, fallback if available | Show metadata + fallback; viewer returns 410 |

Every event carries the complete effective snapshot. A failed/unavailable event
must include any inherited fallback; clients must not rely on seeing an older
event.

## 9. Viewer Resource

Canonical route:

```text
GET /v1/sessions/{session_id}/previews/{artifact_id}/revisions/{revision}/viewer
```

Rules:

- Authenticated runtime request only; no absolute URL in the artifact.
- No redirects.
- Response MIME exactly `text/html; charset=utf-8`.
- Maximum viewer bytes: 2 MiB in v1.
- `size_bytes` and `sha256` are mandatory for ready interactive previews.
- `sha256` is computed over the exact uncompressed response body bytes served by
  the runtime.
- AgentKit verifies size, MIME, digest, and session/artifact/revision binding
  before creating a preview scheme token.
- Viewer responses use immutable content headers and do not require Range.
- CLI/TUI uses the same authenticated fetch and trusted-viewer verification.

The runtime is responsible for wrapping the model fragment in a trusted viewer
document. The fragment itself must never be loaded as the top-level page.

## 10. Preview Assets

Preview assets use a sanitized DTO, not the general `assets.Ref` wire shape.

Required fields:

- `asset_id`
- `role`: `primary`, `fallback`, or `evidence`
- `mime_type`
- `size_bytes`
- `sha256`
- `content_url`
- `alt` for images

Optional fields:

- `width`, `height`
- workspace-relative display path

Forbidden fields:

- `absolute_path`
- materialized filesystem paths
- credentials, tokens, or signed URLs embedded in metadata
- arbitrary file locators outside the runtime asset API

Fallback assets must appear in `assets[]` and be immutable for the artifact
revision. Asset content may support Range/ETag where appropriate, but fallback
bytes must remain stable for that revision.

## 11. Revision and Replay

- Highest revision wins for one artifact.
- Duplicate same-revision event with identical canonical payload is idempotent.
- Same revision with different canonical payload is `preview_revision_conflict`
  and must not overwrite the current valid view.
- Lower revision never overwrites a higher revision.
- Higher revision replaces the same ConversationWorkbench block in place.
- Older live/history pages are merged by artifact ID and revision.
- Replay preserves artifact ID, revision, viewer digest metadata, asset IDs,
  status, error, fallback, and presentation hints.
- Expired bytes show metadata plus fallback, without requesting the viewer.
- Preview failure never removes the tool card, assets, evidence, or answer.
- Interactive iframe state need not survive app restart in v1.

## 12. Client Presentation State

Runtime `status` and AgentKit presentation state are separate.

AgentKit state:

- `loading`
- `interactiveReady`
- `staticFallback`
- `unavailable`
- `renderFailed`
- `expired`

AgentKit state is derived from runtime status, capability intersection, fetch and
digest checks, renderer result, and fallback availability. It is not serialized
back into the runtime artifact.

## 13. ConversationWorkbench Presentation

AgentKit owns a first-class inline Preview Block in the macOS
ConversationWorkbench.

Required behavior:

- Preview card displays title, summary, status, fallback, and degraded reason.
- Opening the preview renders the interactive block directly in the conversation
  workbench.
- The block is keyed by `artifact_id` and `revision`, not `call_id`.
- `WorkbenchState.selectedWorkProductID` is not required for inline preview and
  must not be the artifact identity.
- Focused/Inspector expansion is optional and post-v1 unless separately approved.
- Repeated streaming updates must not recreate an unchanged iframe revision.

## 14. Renderer and Isolation

V1 primary renderer is a sandboxed iframe inside ConversationWorkbench.

Required iframe attributes:

```html
sandbox="allow-scripts"
```

No `allow-same-origin`, forms, popups, top navigation, downloads, or modals.

Required AgentKit behavior:

- Fetch and verify viewer bytes before registering them.
- Register verified viewer bytes under an opaque preview scheme token, for
  example `agentkit-workbench://preview/<opaque-token>`.
- Parent shell CSP allows only the exact preview scheme in `frame-src`.
- Viewer document carries its own restrictive CSP.
- Navigation delegate allows only the initial preview-frame load for the exact
  token.
- All later preview-frame navigation is canceled.
- All existing `WKScriptMessage` handlers reject non-main-frame messages.
- Preview has no access to `agentkitWorkbench` or any other native message handler.
- Network access is denied by both CSP and a WebKit content-rule or equivalent
  defense-in-depth layer.

If a supported WebKit/macOS version cannot prove these constraints, AgentKit may
use an isolated child WKWebView inline or show static fallback. It must not fall
back to an unrestricted iframe.

## 15. Actions

V1 has no preview-to-runtime action bridge:

- No opaque action tokens.
- No `previewNavigation` capability.
- No preview-specific native message handler.
- No runtime mutation from preview JavaScript.

Allowed interactions are local HTML/CSS/JavaScript only, plus AgentKit-owned
host controls such as open, collapse, expand, retry, and view static evidence.

Any future action bridge requires a new contract version and security review.

## 16. Tool Exposure

Tool visibility and execution are orthogonal:

```text
model_visibility: direct | hidden
executor:        server | client | flex
```

Examples:

- Direct server tool: visible and server-executed.
- Hidden client tool: not model-visible, client-executed by runtime control.
- Direct client tool: visible and client-executed.
- Flex tool: visible; executor is resolved by the runtime.

`client` must never mean both visibility and execution.

## 17. Error Codes and HTTP Mapping

| Code | HTTP | Meaning |
| --- | --- | --- |
| `preview_unsupported` | 200 degraded state | Capability unavailable |
| `preview_artifact_missing` | 404 | Artifact not found |
| `preview_invalid_artifact` | 422 | Invalid draft/artifact |
| `preview_viewer_too_large` | 413 | Viewer exceeds 2 MiB |
| `preview_viewer_mime_mismatch` | 415 | Viewer is not trusted HTML |
| `preview_digest_mismatch` | 422 | Viewer digest failed |
| `preview_viewer_fetch_failed` | 502 | Authenticated fetch failed |
| `preview_security_violation` | 403 | Navigation/network/bridge violation |
| `preview_render_failed` | 200 degraded state | Client renderer failed |
| `preview_asset_unavailable` | 404 | Fallback asset unavailable |
| `preview_expired` | 410 | Immutable bytes expired |
| `preview_revision_conflict` | 409 | Same revision, different payload |
| `preview_store_unavailable` | 503 | Preview store/materializer unavailable |

Errors are preview data. They do not terminate the agent turn.

## 18. Fixtures

The canonical fixture directory will be:

```text
docs/capability-runtime-upgrade/fixtures/preview/v1/
```

Required fixtures:

1. Ready interactive artifact with fallback.
2. Static image artifact.
3. Evidence bundle artifact.
4. Pending artifact.
5. Failed artifact with inherited fallback.
6. Unavailable artifact without fallback.
7. Expired replay artifact.
8. Revision 1 to revision 2 replacement.
9. Duplicate same-revision idempotent event.
10. Same-revision conflicting payload.
11. Viewer at 2 MiB and 2 MiB + 1.
12. Invalid UTF-8 boundary.
13. Viewer digest mismatch.
14. Absolute-path and traversal rejection.
15. Capability matrix for runtime/client combinations.
16. Server tool, hidden client tool, and direct client tool produce the same
    preview artifact shape.
17. Replay frame golden data.

Fixture files must be committed before parallel implementation begins.

## 19. R0 Freeze Checklist

- v1.1 is the sole contract authority.
- Event uses `kind`, `seq`, `session_id`, and `artifact`.
- Producer side-channel is accepted.
- Artifact identity/revision/conflict rules are accepted.
- Viewer and asset transport/digest/auth rules are accepted.
- Status/kind/fallback matrix is accepted.
- Capability strings and AND semantics are accepted.
- Tool visibility and executor dimensions are accepted.
- Sandboxed iframe rules and main-frame message rejection are accepted.
- V1 has no preview action bridge.
- Fixture directory and golden cases are committed.
- Backend and AgentKit both return GO after re-review.
