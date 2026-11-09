# Preview Contract v1.6

Status: frozen
Date: 2026-11-09
Contract ID: `preview_contract_v1`
Supersedes: [PREVIEW_CONTRACT.md](./PREVIEW_CONTRACT.md), [PREVIEW_CONTRACT_V1_1.md](./PREVIEW_CONTRACT_V1_1.md), [PREVIEW_CONTRACT_V1_2.md](./PREVIEW_CONTRACT_V1_2.md), [PREVIEW_CONTRACT_V1_3.md](./PREVIEW_CONTRACT_V1_3.md), [PREVIEW_CONTRACT_V1_4.md](./PREVIEW_CONTRACT_V1_4.md), [PREVIEW_CONTRACT_V1_5.md](./PREVIEW_CONTRACT_V1_5.md)
Owners: code-agent backend + AgentKit GUI
Reviewer: independent integration/security reviewer

## 1. Authority

This document is the single source of truth for the preview contract. If another
upgrade document conflicts with this file, this file wins until the next
versioned contract replaces it.

The v1 through v1.5 drafts are retained only for review history.

## 2. Changes from v1.5

Version 1.6 fixes the final fallback-invariant conflict:

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
- Adds stable producer keys and typed asset drafts for real revision updates.
- Separates runtime artifact errors from HTTP and client presentation errors.
- Removes runtime `expired` mutation and derives expiry on the client.
- Removes `evidence_bundle` from v1 to reduce unresolved schema surface.
- Uses the existing `/v1/conversations` namespace.
- Freezes a raster-only fallback allowlist and self-contained viewer CSP.
- Adds a canonical scenario manifest and executable fixture cases.
- Freezes RFC 8785 JCS for `revision_sha256`.
- Freezes pending-to-ready revision allocation.
- Freezes outgoing client-tool `previews[]` and `register_tools` bindings.
- Freezes capability implications and reconciliation.
- Freezes the existing runtime HTTP envelope and CLI/TUI resolver interface.
- Expands manifest expectations and negative security fixtures.
- Makes runtime-derived producer identity/capability authoritative.
- Uses existing nonzero numeric error codes for non-2xx preview responses.
- Freezes failed/unavailable/expired presentation precedence.
- Separates security violation presentation from render/fetch failure.
- Adds a forged-producer negative fixture.
- Removes the invalid fallbackless ready interactive scenario.
- Treats missing fallback on ready interactive as artifact corruption.

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
- `preview_interactive_html_v1` implies `preview_static_asset_v1`; advertising
  interactive without static is invalid and fails the preview block closed.
- `preview_static_asset_v1` is required on both sides to render static evidence.
- `preview_replay_v1` is required on both sides for historical replay and is a
  contract baseline, not a client-only extension.

Runtime may advertise interactive/replay only when the artifact store, viewer
materializer, authenticated API, and replay path are all available.

The runtime `hello` capability list is authoritative for the active connection.
`/v1/runtime/capabilities` must expose the same preview flags. A mismatch is
logged and the intersection is used; no preview may be rendered from a flag that
is absent from either source.

### 4.3 Degraded behavior

| Runtime | Client | Result |
| --- | --- | --- |
| interactive + static | interactive + static | Inline interactive preview with static fallback |
| static only | static | Static evidence card |
| interactive only | static | Invalid runtime advertisement; static fallback with degraded warning |
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
    ArtifactKey   string
    Kind          string // interactive_html | static_image in v1
    Title         string
    Summary       string
    ViewerHTML    string
    Assets        []PreviewAssetDraft
    Fallback      *PreviewFallbackDraft
    Presentation  *PreviewPresentationHint
}

type PreviewAssetDraft struct {
    AssetID  string
    Role     string // primary | fallback
    MIMEType string
    Alt      string
    Width    int
    Height   int
}

type PreviewFallbackDraft struct {
    AssetID string
    Text    string
}
```

`artifact_key` is scoped by runtime-derived `(session_id, capability, producer_id)`.
Producers do not submit trusted capability or producer identity.

- `producer_id` is derived by the runtime from the resolved tool binding name.
- `capability` is derived from the active capability that bound the tool.
- If a tool is bound by multiple active capabilities, the producing invocation's
  capability context selects exactly one.
- Client-submitted `producer_id` or `capability` fields are rejected as unknown
  fields rather than trusted.
- The runtime stamps both values into the persisted artifact for audit/replay.
- A client cannot forge another producer's revision namespace.

The runtime validates the draft, materializes a viewer when required, snapshots
assets, assigns `artifact_id` and `revision`, and stamps all correlation IDs.
Producers may submit drafts that resolve to `pending` or `ready`; only the
runtime may persist `failed` or `unavailable` artifact states.

Pending semantics are frozen:

- A produced pending artifact is persisted as revision N.
- The first ready snapshot for the same artifact is revision N+1.
- Pending never transitions to ready in place.
- Pending may omit viewer and fallback; a ready interactive artifact must include
  both.
- Pending and ready use separate `revision_sha256` values.

### 5.2 Producer matrix for v1

| Producer | V1 support |
| --- | --- |
| Server-side built-in tool | Supported through typed `PreviewDraft` |
| Client-executed tool | Supported through explicit `previews` field in client tool result |
| MCP tool | Not automatically promoted in v1; MCP preview support is post-v1 |

Client tool preview drafts use the same logical fields over Agent Wire. Asset IDs
must reference already authorized client assets or runtime snapshots. Inline
base64 preview content is not a v1 wire mechanism.

Client tool result wire:

```json
{
  "type": "agent_input",
  "kind": "tool_result",
  "tool_result": {
    "tool_use_id": "call_...",
    "subtype": "result",
    "content": "Preview generated",
    "previews": [
      {
        "artifact_key": "ui-adaptation:dreamlog-duo-home",
        "kind": "interactive_html",
        "title": "DreamLog Duo Home",
        "summary": "Three device forms",
        "viewer_html": "<div>...</div>",
        "assets": [
          {
            "asset_id": "asset_fixture_fallback",
            "role": "fallback",
            "mime_type": "image/png",
            "alt": "Static evidence",
            "width": 1200,
            "height": 800
          }
        ],
        "fallback": {"asset_id": "asset_fixture_fallback", "text": "Static evidence"}
      }
    ]
  }
}
```

V1 limits a single tool result to four preview drafts. `previews` is optional and
appears only inside `agent_input.tool_result`. Required draft fields are
`artifact_key`, `kind`, `title`, and `viewer_html` for interactive drafts;
`assets` and `fallback` are required when the kind requires them. Unknown fields,
unsupported kinds, forged identity fields, duplicate artifact keys, missing
fallback assets, or malformed drafts make that draft fail validation without
failing the tool result or turn.

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
  "revision_sha256": "sha256:...",
  "status": "ready",
  "kind": "interactive_html",
  "title": "DreamLog Duo Home",
  "summary": "Outer, inner portrait, and inner landscape previews.",
  "capability": "ui-adaptation",
  "producer_id": "ui_preview",
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
    "url": "/v1/conversations/session_.../previews/preview_.../revisions/1/viewer",
    "mime_type": "text/html; charset=utf-8",
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
      "content_url": "/v1/conversations/session_.../previews/preview_.../revisions/1/assets/asset_.../content",
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
- `revision_sha256` is `sha256(UTF-8(RFC8785-JCS(artifact_without_revision_sha256)))`.
  It is the idempotency/conflict key. RFC 8785 controls recursive key ordering,
  number serialization, Unicode escaping, nulls, arrays, and compact formatting.
  No trailing newline is included. Missing fields and explicit `null` are not
  equivalent and are preserved exactly.
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

### 7.4 Error Schema

Runtime artifact errors use:

```json
{
  "code": "preview_invalid_artifact",
  "message": "The preview draft was rejected.",
  "retryable": false
}
```

Rules:

- `code`, `message`, and `retryable` are required for runtime `failed` and
  `unavailable` artifacts.
- `code` uses the stable enum in §17.
- `message` is bounded to 1024 UTF-8 bytes and contains no paths or secrets.
- `retryable` describes producer/runtime retry, not client presentation retry.
- `preview_render_failed` is not a runtime artifact error. It is AgentKit local
  presentation state only.

### 7.5 Correlation

For a persisted preview event:

- Envelope and artifact `session_id`, `turn_id`, and `call_id` must match exactly
  when present in both locations.
- A mismatch fails that preview event closed; it does not fail the turn.
- `artifact_id` and `revision` are assigned by the runtime.
- `artifact_key` is never exposed as artifact identity on the wire.
- The canonical payload used for `revision_sha256` excludes `revision_sha256`
  itself, transport `event_id`, and `seq`.
- Missing, malformed, or algorithm-mismatched `revision_sha256` is
  `preview_invalid_artifact`.
- Same revision with the same hash is idempotent; same revision with a different
  hash is `preview_revision_conflict`.

## 8. Status and Kind Matrix

| Status / kind | Required fields | Client behavior |
| --- | --- | --- |
| `pending` | identity, kind, title | Show pending card; viewer optional |
| `ready` + `interactive_html` | viewer, security, fallback asset | Render iframe with static fallback |
| `ready` + `static_image` | at least one primary image asset | Render static card |
| `failed` | `error`; fallback optional | Show failed state; use fallback if present |
| `unavailable` | `error` | Show unavailable card |

There is no runtime `expired` status. `expires_at` is metadata on a ready/failed
artifact. AgentKit derives `expired` presentation state when the current time is
at or after `expires_at`, or when the viewer returns HTTP 410. Expiry never
creates a new artifact revision.

`evidence_bundle` is removed from v1 and becomes post-v1.

Every event carries the complete effective snapshot. A failed/unavailable event
must include any inherited fallback; clients must not rely on seeing an older
event.

## 9. Viewer Resource

Canonical route:

```text
GET /v1/conversations/{session_id}/previews/{artifact_id}/revisions/{revision}/viewer
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
- READY viewer resources are self-contained: no external scripts, styles, fonts,
  images, network requests, iframes, or navigation.
- Minimum CSP is `default-src 'none'; script-src 'unsafe-inline'; style-src
  'unsafe-inline'; img-src data:; connect-src 'none'; frame-src 'none';
  object-src 'none'; base-uri 'none'; form-action 'none'`.
- The viewer must not require nonce/hash support in v1; inline script/style are
  allowed only inside the isolated preview document.

The runtime is responsible for wrapping the model fragment in a trusted viewer
document. The fragment itself must never be loaded as the top-level page.

### 9.1 Transport-neutral resolver

CLI/TUI and AgentKit use the same semantic resolver:

```text
PreviewViewerResolver.Resolve(session_id, artifact_id, revision)
PreviewAssetResolver.ResolveContent(session_id, artifact_id, revision, asset_id)
```

- Remote AgentKit resolves through authenticated RuntimeHTTPClient routes.
- CLI/TUI resolves through an in-process authenticated/materialized resolver.
- Both return bytes plus status, MIME, size, digest, and expiry metadata.
- Both apply the same size, MIME, digest, navigation, and fallback checks before
  presenting content.
- The CLI/TUI resolver writes verified viewers to a private temporary directory
  with restrictive permissions or serves them through a single-use local
  loopback token. It never opens the raw model fragment.

## 10. Preview Assets

Preview assets use a sanitized DTO, not the general `assets.Ref` wire shape.

Required fields:

- `asset_id`
- `role`: `primary` or `fallback`
- `mime_type`
- `size_bytes`
- `sha256`
- `content_url`
- `alt` for images

V1 preview asset MIME allowlist is raster-only: `image/png`, `image/jpeg`, and
`image/webp`. SVG is rejected because it can carry active content.

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
- Duplicate same-revision event with identical `revision_sha256` is idempotent.
- Same revision with different `revision_sha256` is `preview_revision_conflict`
  and must not overwrite the current valid view.
- Lower revision never overwrites a higher revision.
- Higher revision replaces the same ConversationWorkbench block in place.
- Older live/history pages are merged by artifact ID and revision.
- Replay preserves artifact ID, revision, viewer digest metadata, asset IDs,
  status, error, fallback, and presentation hints.
- `expires_at` is evaluated at `now >= expires_at`. Expiry never mutates the
  artifact revision.
- Expired bytes show metadata plus fallback, without requesting the viewer; a
  viewer/asset request returns HTTP 410.
- Preview failure never removes the tool card, assets, evidence, or answer.
- Interactive iframe state need not survive app restart in v1.
- Missing `preview_replay_v1` disables historical interactive recreation only.
  Live events may still render. Historical cards degrade to static/unavailable
  and never fail the turn.

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

Presentation precedence is frozen:

| Runtime/client condition | Presentation state |
| --- | --- |
| `ready` + interactive capability + valid viewer | `interactiveReady` |
| `failed` + fallback | `staticFallback` |
| `failed` without fallback | `unavailable` |
| `unavailable` + fallback | `staticFallback` |
| `unavailable` without fallback | `unavailable` |
| render/fetch failure + fallback | `staticFallback` |
| render/fetch failure without fallback | `renderFailed` |
| security violation + fallback | `staticFallback` |
| security violation without fallback | `unavailable` |
| expired bytes + fallback | `expired` with fallback content |

Expiry takes precedence over interactive rendering. Fallback availability takes
precedence over `renderFailed`/`failed`/`unavailable` where listed. Security
violations never present as `renderFailed`; without a fallback they are
`unavailable`. A ready `interactive_html` artifact without its required fallback
is corrupt and presents as `unavailable`; it does not get an additional expiry
state.

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
- Preview may be able to enumerate `window.webkit`, but every message sent to an
  existing native handler must be rejected when `frameInfo.isMainFrame == false`
  and produce no native side effect.
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

Capability manifests use typed bindings rather than `Tools []string`:

```json
{
  "tool_bindings": [
    {
      "name": "read_file",
      "model_visibility": "direct",
      "executor": "server"
    }
  ]
}
```

Client `register_tools` uses the same two fields. Unknown values, conflicting
duplicate names, and hidden tools without an authorized runtime dispatch path
fail closed.

Exact client registration shape:

```json
{
  "type": "register_tools",
  "tools": [
    {
      "name": "ios_preview_capture",
      "description": "Capture a native preview.",
      "input_schema": {"type": "object"},
      "model_visibility": "hidden",
      "executor": "client"
    }
  ]
}
```

The array key is `tools`. For legacy clients, omitted fields default to
`model_visibility=direct` and `executor=client`. A client may send `flex`; the
runtime resolves the actual executor per call and reports the chosen executor in
`tool_started.executor` as `server` or `client`. A client must not receive a flex
call it did not advertise support for.

## 17. Error Codes and HTTP Mapping

Preview HTTP failures use the existing runtime response envelope:

```json
{
  "trace_id": "trace_...",
  "code": 42200,
  "msg": "preview_digest_mismatch",
  "data": {
    "error": {
      "code": "preview_digest_mismatch",
      "message": "Viewer digest did not match.",
      "retryable": false
    }
  }
}
```

For every non-2xx response, numeric `code` is nonzero and follows the runtime's
existing `HTTPStatus * 100` convention: 404→40400, 409→40900, 410→41000,
413→41300, 415→41500, 422→42200, 502→50200, 503→50300. The HTTP status remains
the primary transport outcome. String preview codes live in `data.error.code`
and repeat in top-level `msg` for diagnostics. HTTP errors
describe runtime/API failures. Client-side fetch, render, security, and expiry
presentation failures use AgentKit presentation state locally and never create a
runtime artifact revision.

`preview_unsupported` is not an HTTP failure. It is a successful 200 response
with `code=0` and a degraded preview presentation in `data`.

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
| `preview_render_failed` | client-only | AgentKit renderer failed; never persisted |
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
3. Pending artifact.
4. Failed artifact with inherited fallback.
5. Unavailable artifact without fallback.
6. Expiry presentation derived from `expires_at` during replay.
7. Revision 1 to revision 2 replacement.
8. Duplicate same-revision idempotent event.
9. Same-revision conflicting payload.
10. Viewer at 2 MiB and 2 MiB + 1.
11. Invalid UTF-8 boundary.
12. Viewer digest mismatch.
13. Absolute-path and traversal rejection.
14. Capability matrix for runtime/client combinations.
15. Server tool, hidden client tool, and direct client tool produce the same
    preview artifact shape.
16. Replay frame golden data.
17. Viewer CSP/navigation/network/bridge negative cases.

Every fixture is referenced by a `manifest.json` with:

- `fixture_version`
- `scenario_id`
- input event/file list
- fixed digests where applicable
- expected final artifact state
- expected AgentKit presentation state
- expected error/fallback outcome

Fixture files must be committed before parallel implementation begins.

## 19. R0 Freeze Checklist

- v1.6 is the sole contract authority.
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
