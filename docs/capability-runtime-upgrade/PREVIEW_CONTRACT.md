# Preview Contract v1

Status: superseded by PREVIEW_CONTRACT_V1_1.md
Date: 2026-11-09
Contract ID: `preview_contract_v1`
Owners: code-agent backend + AgentKit GUI
Reviewer: independent integration/security reviewer

## 1. Purpose

This contract is the frozen interface between the code-agent runtime and
AgentKit for generic interactive previews.

It defines:

- Preview artifact identity and lifecycle.
- Runtime-to-client events.
- Capability negotiation.
- Viewer and asset references.
- Interactive/static/degraded states.
- Replay and revision semantics.
- Security profiles.
- Contract fixtures and compatibility tests.

It does not define implementation internals or specific tools.

## 2. Non-negotiable Rules

1. Backend and AgentKit implement this contract independently.
2. Neither side may infer preview behavior from tool names.
3. Talkify must not implement generic preview parsing or rendering.
4. AgentKit must display generic previews directly in ConversationWorkbench.
5. Model-authored JavaScript must run in an isolated preview surface, never in
   the main ConversationWorkbench DOM or its native message handler.
6. Preview artifacts and asset bytes are immutable by revision.
7. Every ready artifact must have a static fallback or explicit unavailable state.
8. Deferred Tool Discovery, Tool Search, and Code Mode are post-v1.

## 3. Capability Negotiation

### 3.1 Runtime capability

The code-agent runtime advertises:

```text
preview_contract_v1
preview_interactive_html_v1
preview_static_asset_v1
```

### 3.2 AgentKit client capability

AgentKit advertises:

```text
preview_contract_v1
preview_interactive_html_v1
preview_static_asset_v1
preview_replay_v1
```

### 3.3 Degraded behavior

| Runtime | AgentKit | Result |
| --- | --- | --- |
| interactive supported | interactive supported | Render inline interactive preview |
| interactive supported | interactive unsupported | Render static fallback |
| static supported | static supported | Render static evidence card |
| unsupported | any | Show unavailable state without breaking the turn |

No capability mismatch may fail the containing agent turn.

## 4. Preview Artifact

### 4.1 Schema

```json
{
  "schema_version": "preview.artifact.v1",
  "artifact_id": "preview_01J...",
  "revision": 1,
  "status": "ready",
  "kind": "interactive_html",
  "title": "DreamLog Duo Home",
  "summary": "Outer screen, inner portrait, and inner landscape previews.",
  "capability": "ui-adaptation",
  "conversation_id": "conv_...",
  "turn_id": "turn_...",
  "call_id": "call_...",
  "created_at": "2026-11-09T12:00:00Z",
  "viewer": {
    "url": "/v1/conversations/conv_.../previews/preview_01J.../viewer",
    "mime_type": "text/html",
    "size_bytes": 182304,
    "sha256": "sha256:..."
  },
  "assets": [
    {
      "asset_id": "asset_...",
      "kind": "image",
      "role": "static_fallback",
      "mime_type": "image/png",
      "width": 1400,
      "height": 900,
      "alt": "DreamLog Duo inner landscape layout"
    }
  ],
  "security": {
    "profile": "preview.security.v1",
    "javascript": "isolated_only",
    "network": "deny",
    "navigation": "deny",
    "persistent_data": "deny",
    "actions": "none"
  },
  "fallback": {
    "asset_id": "asset_...",
    "text": "Interactive preview unavailable. Showing static evidence."
  },
  "metadata": {
    "viewport": "duo-matrix",
    "document_kind": "ui-preview"
  }
}
```

### 4.2 Required fields

- `schema_version`
- `artifact_id`
- `revision`
- `status`
- `kind`
- `title`
- `conversation_id`
- `turn_id`
- `viewer` when `status=ready` and `kind=interactive_html`
- `security`
- `fallback` unless status is `unavailable`

### 4.3 Status

| Status | Meaning |
| --- | --- |
| `pending` | Artifact is expected but not yet renderable |
| `ready` | Viewer and/or fallback assets are available |
| `failed` | Artifact generation failed; fallback may still be available |
| `unavailable` | Client/runtime cannot render this kind |
| `expired` | Bytes are no longer available; metadata remains for replay |

### 4.4 Kind

V1 kinds:

- `interactive_html`
- `static_image`
- `evidence_bundle`

`interactive_html` must include a `viewer` and a static fallback.

## 5. Viewer Resource

The `viewer.url` is a runtime-authorized resource, not a raw workspace path.

The runtime must return a trusted viewer document that:

- Wraps the model-authored fragment.
- Applies the `preview.security.v1` CSP and resource policy.
- Contains only allowlisted local/bundled resources.
- Does not expose filesystem paths or credentials.
- Sets immutable cache headers for a revision.
- Supports range/caching behavior only when safe.

AgentKit must fetch the viewer through `RuntimeHTTPClient` or an equivalent
authenticated runtime transport. It must not read a backend filesystem path.

## 6. Asset References

Preview assets use the existing runtime asset identity:

- `asset_id`
- `kind`
- `mime_type`
- `workspace_id` and workspace-relative path when applicable
- `uri`
- `preview` metadata
- immutable revision binding

AgentKit may load:

- Runtime asset preview/content endpoints.
- Local signed media URLs returned by the runtime asset API.

Asset URLs must not grant access to unrelated workspace files.

## 7. Event Contract

V1 adds one generic preview event/item:

```json
{
  "type": "preview_artifact",
  "conversation_id": "conv_...",
  "turn_id": "turn_...",
  "call_id": "call_...",
  "artifact": { "...": "PreviewArtifact" }
}
```

Semantics:

- Repeating the same `artifact_id` with a higher `revision` replaces the view.
- A lower/duplicate revision is ignored.
- A `failed` or `unavailable` update preserves the latest valid fallback.
- Event replay must reconstruct the same artifact state.

No separate `preview_delta` event exists in v1.

## 8. ConversationWorkbench Presentation

AgentKit owns presentation:

```text
ConversationWorkbench
├── Preview card
│   ├── title and status
│   ├── open/expand action
│   ├── static fallback thumbnail
│   └── unavailable/degraded reason
└── Inline Preview Block
    ├── isolated iframe or child WKWebView
    ├── toolbar: expand / restore / open static evidence
    └── no access to the main workbench DOM or bridge
```

The first render may be a card with an explicit open action if inline preview is
expensive. Once open, it appears in ConversationWorkbench, not a Talkify feature
extension.

`WorkbenchState.selectedWorkProductID` may be used for expansion/focus but must
not be required for inline viewing.

## 9. Security Profile `preview.security.v1`

### Required

- Non-persistent WebKit data store.
- No remote navigation.
- No new windows.
- No arbitrary `file://` access.
- No persistent cookies, local storage, or cache across conversations.
- No access to the main ConversationWorkbench message handler.
- No access to native app APIs.
- CSP `default-src 'none'` baseline.
- Script allowed only for the isolated preview viewer.
- Images/styles/fonts only from bundled or explicitly authorized preview assets.
- Connect/XHR/WebSocket/fetch denied.
- Fragment size hard limit of 2 MB in v1.
- Viewer digest verified before display when provided.

### Recommended isolation

Primary implementation:

- Sandboxed iframe inside the ConversationWorkbench document.
- `sandbox="allow-scripts"` without `allow-same-origin`.
- Runtime viewer supplies the inner document and CSP.

Fallback implementation when iframe cannot enforce the required policy:

- Isolated child `WKWebView` managed by AgentKit.
- Same non-persistent store, navigation delegate, and CSP rules.

## 10. Action Bridge

V1 model-authored previews are local-interactive only:

- Tabs, filters, carousels, and other client-side interactions are allowed.
- Preview-to-runtime actions are disabled.
- AgentKit-owned controls remain available: expand, collapse, open static
  evidence, retry, and close.

Post-v1 may add typed actions with opaque tokens:

```json
{
  "artifact_id": "preview_01J...",
  "revision": 1,
  "action_id": "select_variant",
  "payload": {"variant": "inner-wide"}
}
```

No arbitrary JavaScript-to-native bridge is permitted.

## 11. Replay Contract

The runtime and AgentKit must preserve:

- Artifact ID.
- Revision.
- Viewer digest.
- Asset IDs and roles.
- Status and degraded reason.
- Static fallback.

On replay:

- The same artifact revision of preview is shown in ConversationWorkbench.
- Expired bytes show metadata plus static fallback.
- A failed preview does not remove the tool call, evidence, or assistant answer.
- Interactive state is not required to persist across app restart in v1.

## 12. Error Codes

Preview errors use stable codes:

- `preview_unsupported`
- `preview_artifact_missing`
- `preview_viewer_too_large`
- `preview_digest_mismatch`
- `preview_security_violation`
- `preview_render_failed`
- `preview_asset_unavailable`
- `preview_expired`

Errors are data, not fatal turn errors.

## 13. Contract Fixtures

Both implementations must test against the same fixtures:

1. Ready interactive HTML with static fallback.
2. Static-only artifact.
3. Failed artifact with fallback.
4. Unavailable interactive client.
5. Expired artifact during replay.
6. Duplicate event with same revision.
7. Higher revision replacing lower revision.
8. Oversized viewer.
9. Digest mismatch.
10. Remote navigation attempt rejected.
11. `fetch`/WebSocket/connect attempt rejected.
12. Preview asset outside allowed root rejected.

Fixture files must be committed before parallel implementation starts.

## 14. R0 Freeze Checklist

- Backend agrees no filesystem path leaks.
- AgentKit agrees no tool-name parsing.
- AgentKit confirms ConversationWorkbench inline placement.
- Static fallback is present in every supported artifact.
- Event/revision semantics are accepted.
- Security profile is accepted.
- Action bridge is local-only in v1.
- Both sides accept the fixture list.
- Reviewer approves the contract version.
