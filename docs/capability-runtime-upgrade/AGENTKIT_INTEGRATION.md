# AgentKit Integration Design

Status: proposed engineering direction
Date: 2026-11-09
Related: [PRD.md](./PRD.md), [DECISIONS.md](./DECISIONS.md)

## 1. Decision

AgentKit owns the generic Agent GUI for the Talkify desktop client.

Generic interactive preview, asset presentation, timeline integration,
navigation policy, and client capability handling belong in AgentKit. The
preview must be a first-class block inside the macOS ConversationWorkbench.
Talkify should consume that surface directly instead of implementing a dedicated
`TimelineExtension` for every preview-producing tool.

Product-specific extensions remain appropriate for domain-specific experiences,
such as Desktop Control evidence. They are not the template for the generic
interactive preview host.

## 2. Current AgentKit Capabilities

AgentKit already provides the relevant GUI seams:

- `Sources/AgentKit/Features/Conversation/TimelineExtensions/TimelineExtension.swift`
  - `TimelineExtension` for product-specific content.
  - `WebTimelineExtension` for semantic Timeline nodes.
  - `TimelineWebNode` and `TimelineWebDocument` data models.
- `Sources/AgentKit/Features/Inspector/TimelineDocumentInspectorView.swift`
  - Native Inspector host for HTML, Markdown, and plain text.
  - The current HTML viewer uses a non-persistent WebKit store, blocks network
    and navigation, and disables JavaScript.
- `Sources/AgentKit/Features/Inspector/AssetInspectorView.swift`
  - Structured asset list and preview UI.
  - Runtime asset preview/content loading.
- `Sources/AgentKit/Core/AgentCapabilityFlags.swift`
  - Runtime/client capability negotiation.
- `Sources/AgentKit/Core/RuntimeHTTPClient.swift`
  - Asset preview and content retrieval APIs.
- `Sources/AgentKit/Features/Conversation/Web/ConversationWebWorkbenchView.swift`
  - The macOS conversation renderer is a single WebKit workbench with a custom
    scheme handler, non-persistent store, JavaScript enabled for the trusted
    shell, and a typed action bridge.
- `Sources/AgentKit/Features/Workbench/WorkbenchState.swift`
  - Selection state for work products, suitable for expanding or focusing a
    preview block.

This is enough to add a first-class preview feature in the conversation itself
without making Talkify own the generic renderer.

## 3. Why Not Reuse DesktopControlEvidenceTimeline

`DesktopControlEvidenceTimeline` is intentionally product-specific:

- It listens for private `desktop_control` MCP tools.
- It decodes Desktop Control-specific evidence models.
- It renders a feature-specific card and document fallback.
- Its WebKit view disables JavaScript.

Using it as the generic preview host would couple visual capability to one
plugin, duplicate security policy, and force other tools to imitate private
Desktop Control output.

The correct generalization is an AgentKit preview subsystem whose inputs are
generic runtime assets and preview descriptors.

## 4. Ownership Boundary

| Responsibility | code-agent runtime | AgentKit | Talkify |
| --- | --- | --- | --- |
| Capability activation and tool execution | Owns | No | No |
| Preview artifact creation and validation | Owns | No | No |
| Preview descriptor serialization | Owns contract | Decodes/uses | No |
| Generic timeline and Inspector UI | No | Owns | No |
| Interactive WebKit host and navigation policy | No | Owns | No |
| Capability flag negotiation | Provides runtime side | Negotiates client side | No |
| Product-specific Desktop Control evidence | No | Extension point | May remain feature-specific |
| App shell, branding, routing | Runtime integration only | GUI primitives | Owns product shell |

## 5. Proposed AgentKit Components

Names are provisional; interfaces and responsibilities are the requirement.

### 5.1 `PreviewArtifact`

AgentKit decodes the canonical artifact schema from
[PREVIEW_CONTRACT_V1_6.md](./PREVIEW_CONTRACT_V1_6.md). It must not maintain a
second behavior-bearing Swift model with different kinds, lifecycle, security,
or fallback fields.

AgentKit may add a client-only `PreviewPresentationState` for loading,
interactive-ready, static fallback, unavailable, render failure, and expiry.
That state is not serialized back to the runtime artifact.

### 5.2 `PreviewCapabilityFlags`

Extend AgentKit capability negotiation with exactly the canonical strings:

- `preview_contract_v1`
- `preview_interactive_html_v1`
- `preview_static_asset_v1`
- `preview_replay_v1`

The runtime advertises its preview contract version; AgentKit advertises the
client surfaces it can render. A capability with a required unsupported surface
must show a blocked/degraded state.

### 5.3 `InteractivePreviewView`

An AgentKit-owned preview block rendered directly in ConversationWorkbench:

- macOS v1 primary path is a sandboxed iframe inside ConversationWorkbench.
- A child `WKWebView` is a tested platform fallback, not an unrestricted iframe
  fallback.
- Uses a non-persistent data store.
- Enables JavaScript only inside the isolated preview document.
- Disables remote navigation by default.
- Fetches viewer bytes through the authenticated runtime client and verifies
  MIME, size, digest, and artifact/revision binding.
- Registers verified bytes under an opaque preview scheme token.
- Drops new-window requests and external navigation.
- Keeps the preview sandbox separate from the existing script-disabled evidence
  Inspector.
- Restores preview artifacts on conversation replay.
- Can expand into a focused Workbench/Inspector state without changing the
  artifact or security model.

### 5.4 `PreviewArtifactStore`

An AgentKit store that:

- Collects preview artifacts from runtime events and asset APIs.
- Associates them with conversation, turn, tool call, and capability.
- Deduplicates by stable artifact ID.
- Keeps the latest valid artifact for each projection key.
- Preserves immutable evidence snapshots for replay.

### 5.5 Generic Timeline card

AgentKit may render a generic “Preview” card from the artifact descriptor. The
card belongs to AgentKit, not to a specific tool extension. Opening the card
shows the shared `InteractivePreviewView` directly in the conversation
workbench. Expansion into Inspector or a focused pane is optional and uses the
same preview artifact.

### 5.6 No v1 action bridge

V1 has no preview-to-runtime action bridge. Preview scripts may update local HTML
state only. AgentKit owns open, collapse, expand, retry, and static-evidence
controls. `previewNavigation`, `previewActionBridge`, and opaque preview action
tokens are post-v1.

## 6. code-agent Runtime Contract

The runtime must provide:

1. A preview artifact descriptor with stable IDs.
2. A trusted materialized viewer entry point.
3. Asset endpoints or references reachable by AgentKit.
4. CSP and viewer metadata generated by the runtime.
5. Capability activation and preview contract version.
6. A structured degraded result when no safe preview can be produced.

The runtime must not assume WebKit, SwiftUI, or macOS. CLI/TUI consumes the same
artifact contract through its own trusted viewer surface.

## 7. Talkify Integration

Talkify should:

- Keep AgentKit as the Agent GUI dependency.
- Use the AgentKit generic preview card and Inspector surface.
- Register host-level WebKit prerequisites if required by AgentKit.
- Avoid creating a new Talkify `TimelineExtension` for generic preview.
- Keep Desktop Control evidence as a product-specific extension until it can be
  migrated to the generic evidence model.

Talkify should not decide whether a preview capability is supported by matching
specific tool names. It uses runtime capability flags and AgentKit client flags.

AgentKit's client tool registration must encode the canonical
`model_visibility` and `executor` fields. `flex` must either be supported
explicitly or declared post-v1; it must not be silently reinterpreted as client
execution.

## 8. CLI/TUI Integration

CLI/TUI implements the same preview artifact contract:

- The runtime materializes a trusted viewer document.
- CLI/TUI displays a clickable preview entry.
- Opening the entry uses a trusted local preview surface or the system browser.
- Raw model-authored HTML is never opened directly from the workspace.
- Static screenshots remain available in the terminal or through the asset
  preview/open flow.

The transport and presentation differ from AgentKit, but the artifact and
security contract are shared.

## 9. Security Requirements

- Preview documents are fragments generated by the model but wrapped by a
  trusted runtime/viewer shell.
- Only the preview artifact root may be mounted read-only.
- JavaScript runs in an isolated WebKit content world.
- No arbitrary network access.
- No unrestricted `file://` access.
- No remote navigation.
- No persistent cookies or website data.
- No preview-specific native bridge or message handler in v1.
- Hard size limits and canonical-path checks.
- Preview failure must never execute fallback JavaScript in the main app context.

## 10. Tests

AgentKit:

- Capability negotiation for interactive preview.
- Artifact decode, deduplication, replay, and stale revision handling.
- WebKit navigation policy rejects remote and file-escape requests.
- JavaScript-disabled evidence viewer remains unchanged.
- JavaScript-enabled preview surface loads only trusted viewer documents.
- No preview message reaches the main ConversationWorkbench bridge.
- Iframe navigation policy permits only the exact preview scheme token.

Talkify:

- Conversation renders a generic preview card without a preview-specific
  `TimelineExtension`.
- The preview block is displayed directly in ConversationWorkbench.
- Missing WebKit capability degrades to static evidence.
- Inspector expansion is post-v1 unless separately approved.

code-agent:

- Preview artifact and security metadata are stable across replay.
- Asset endpoints accept only valid conversation/turn/call bindings.
- CLI/TUI and AgentKit receive the same preview capability contract.

## 11. Non-goals

- Moving all Talkify UI into AgentKit.
- Injecting model-authored preview scripts into the main ConversationWorkbench
  DOM or its native message bridge.
- Migrating Desktop Control evidence in the first milestone.
- Allowing arbitrary host HTML/JavaScript in the conversation document.
- Sharing native view code between CLI/TUI and AgentKit.
- Adding per-tool preview extensions for every capability.
