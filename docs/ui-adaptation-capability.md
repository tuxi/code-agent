# UI Adaptation Capability Design

Status: proposal — downstream capability overview
Date: 2026-11-09
Related: [Visual Layout Understanding Design](./visual-layout-understanding.md)
Normative tool boundary: [Primary Perception Ownership and Tool Prompt Separation](./primary-perception-ownership-contract.md)

This document is the end-to-end overview for UI adaptation. The upstream
contracts for `view_image`, `ui_evidence.v1`, and `ui_layout.v1` live in
[Visual Layout Understanding Design](./visual-layout-understanding.md); preview
artifact transport and replay remain governed by
[Preview Contract v1.6](./capability-runtime-upgrade/PREVIEW_CONTRACT_V1_6.md).
The main model is the primary visual perceiver; OCR and UI evidence tools do not
substitute for `view_image` or choose the design.

## 1. Summary

The capability demonstrated by the DreamLog Duo design session is not a single
image-generation tool. It is a closed loop:

```text
official platform guidance + existing code
        -> interactive layout preview
        -> user confirmation
        -> native implementation
        -> build and device screenshots
        -> visual inspection and correction
```

The recommended code-agent design uses four distinct capability layers plus a
client-side rendering and safety boundary:

- **Plugin/Skill** packages task knowledge and declares the capability.
- **Tool** provides explicit, typed, observable operations.
- **Extension** adds host-owned tools and lifecycle behavior without changing
  the core agent loop.
- **MCP** connects to optional external execution environments.
- **UI rendering and security** safely display generated HTML and screenshots.
- **Runtime** composes the available layers and carries visual assets.
- **Agent loop** remains tool-agnostic and model-driven.

Do not implement this as a special-purpose agent, a hidden design state machine,
or one monolithic MCP server.

## 2. Goals

- Study platform and device adaptation guidance before designing UI.
- Inspect the existing UI, layout, state, and design-system constraints.
- Produce multiple responsive/device-form previews before editing production UI.
- Return previews and screenshots as visible assets, not opaque text.
- Implement the approved design using shared production components and state.
- Verify the result with builds, device matrices, screenshots, and visual review.
- Keep every step explicit, permissioned, replayable, and inspectable.

## 3. Non-goals

- Runtime keyword matching or automatic skill injection.
- An autonomous design scheduler or fixed retry state machine.
- A new agent type whose loop is separate from the main code-agent loop.
- Treating a design mockup as proof that the production app is implemented.
- Using MCP as a substitute for the runtime's tool, asset, or permission model.
- Using unlicensed third-party media in design or App Store artifacts.

## 4. Capability Boundary

The capability should be named `ui-adaptation` until a broader product name is
needed.

```text
Capability: ui-adaptation
├── Plugin/Skill layer
│   └── ui-adaptation
├── Tool layer
│   ├── Existing: web_search, web_fetch
│   ├── Existing: read_file, grep
│   ├── Existing: run_command, apply_patch
│   ├── New: view_image
│   └── New: render_preview
├── Extension layer
│   ├── Capability manifest and tool activation
│   └── Optional preview renderer or image service adapter
├── MCP layer
│   ├── computer_use
│   ├── browser automation
│   └── simulator or device-farm control
├── UI rendering and security boundary
│   ├── Interactive HTML artifact viewer
│   ├── Screenshot and media asset preview
│   └── Path, CSP, iframe, size, and symlink protections
└── Runtime data
    ├── ToolResult.Output for structured preview metadata
    ├── ToolResult.Assets for preview and screenshot assets
    └── LocalAssets for vision-capable model input
```

### 4.1 Layer responsibilities

| Layer | Owns | Must not own |
| --- | --- | --- |
| Plugin/Skill | When and how to perform UI adaptation | Executing commands or displaying UI |
| Tool | One atomic typed operation | Multi-step design workflow |
| Extension | Host-specific tool registration and lifecycle | Agent control flow |
| MCP | External transport and host integration | Core capability policy |
| UI host | Safe rendering and asset presentation | Choosing the next action |
| Runtime | Composition, permissions, events, assets | Hard-coded design decisions |

## 5. Skill

The skill lives at `skills/ui-adaptation/SKILL.md`. It stays short and directs
the model to the visual layout understanding contract when schema details are
needed.

### Required workflow

1. Identify the target platforms, device forms, orientations, themes, and text
   sizes.
2. Read official platform adaptation guidance and record the constraints that
   affect layout, safe areas, navigation, and input.
3. Read the existing screens, layout helpers, navigation, and state ownership.
4. Define a viewport/state matrix before drawing the solution.
5. Build an interactive prototype or preview showing each target form.
6. Obtain user confirmation before changing production UI.
7. Implement with the same production components and state where possible.
8. Verify build plus the real device/viewport matrix.
9. Inspect screenshots and correct layout issues.
10. Separate completed code verification from incomplete end-to-end product
    verification.

### Skill gotchas

- A full-bleed background does not mean controls may ignore safe areas.
- A wide screen needs a new information layout, not just stretched phone UI.
- Nested scrolling changes behavior when the layout changes orientation.
- Folding and rotation must preserve selection, navigation, and editing state.
- System rail and status-bar mockups in an HTML preview are illustrative.
- A mockup screenshot is design evidence; a native screenshot is layout evidence.
- A successful build does not prove import, editing, and export work across
  device-form transitions.

### References

Keep platform-specific details in `references/`, for example:

```text
skills/ui-adaptation/
├── SKILL.md
└── references/
    ├── evidence-checklist.md
    ├── ios-device-matrix.md
    └── visual-review.md
```

## 6. Tools

### 6.1 Existing tools

Reuse these tools; do not create duplicates:

- `web_search` and `web_fetch`: official documentation and platform guidance.
- `read_file` and `grep`: existing UI, layout, and test code.
- `run_command`: browser rendering scripts, builds, simulators, and screenshots.
- `apply_patch`: prototype and production code edits.

### 6.2 `view_image`

`view_image` is the highest-value new built-in tool.

Input:

```json
{
  "path": "artifacts/ui/home-wide.png"
}
```

Output:

- Short text content with the path and image metadata.
- One `assets.Ref` with `kind: "image"`.
- `AbsolutePath` and MIME type so the existing runner can stage a
  `LocalAssetRef` for a vision-capable model.

Requirements:

- Read-only.
- Respect workspace and external-path approval rules.
- Reject protected paths and unsupported image types.
- Enforce the existing vision image-size/total-budget controls.
- Return an asset reference even when the selected model is not vision-capable;
  simply do not rely on visual observation in that case.

The runtime already has the important side channel:

- `ToolResult.Assets` in `internal/tools/tool.go`.
- Image-to-`LocalAssets` staging in `internal/agent/tool_batch.go`.
- Request-time image injection for vision models.

No new event type is required for the first version.

### 6.3 `render_preview`

`render_preview` renders a local prototype into deterministic screenshots for a
declared viewport matrix.

Proposed input:

```json
{
  "source_path": ".codeagent/previews/home.html",
  "variants": [
    {"id": "outer", "width": 390, "height": 844, "theme": "light"},
    {"id": "inner-portrait", "width": 800, "height": 1000, "theme": "light"},
    {"id": "inner-wide", "width": 1400, "height": 900, "theme": "dark"}
  ]
}
```

Proposed output:

```json
{
  "source_path": ".codeagent/previews/home.html",
  "variants": [
    {
      "id": "outer",
      "width": 390,
      "height": 844,
      "asset_id": "asset_turn_call_001_...",
      "path": ".codeagent/artifacts/ui/home-outer.png"
    }
  ]
}
```

Requirements:

- Render only explicit local files by default.
- Disable arbitrary network access unless separately approved.
- Write outputs under the capability's artifact directory.
- Return one image asset per variant.
- Keep the source HTML available to clients that support an interactive WebView.
- Do not claim production verification from rendered HTML.

Implementation options:

- Local headless browser process controlled by a small renderer script.
- Optional browser MCP server when the runtime has no suitable local renderer.

The tool owns rendering, not design decisions. Variants, layout strategy, and
corrections remain model decisions.

### 6.4 Optional visual capture

Native app and device capture should not be baked into the core renderer.

Use one of:

- Existing `run_command` plus simulator-specific CLI commands.
- A strict-client `capture_screen` or `capture_device` tool when the capture must
  happen on the user's host.
- An optional MCP integration for Device Hub, remote simulators, or a device
  farm.

The current `cua_repl`-style desktop automation maps to a client tool or an
optional MCP server, not to a runtime special case.

## 7. Runtime Assembly

### 7.1 Capability Pack and Plugin-compatible Manifest

Introduce a composition layer, not a new execution loop.

Proposed shape:

```go
type CapabilityManifest struct {
    Name               string
    Description        string
    Skills             []string
    Tools              []string
    Extensions         []string
    OptionalMCP        []string
    ClientCapabilities []string
}
```

Example:

```yaml
agent:
  capabilities:
    - ui-adaptation
```

The pack is an explicit assembly recipe:

- Add the skill to the progressive-disclosure index.
- Register the selected tools through the normal registry.
- Activate host-owned extensions only when their runtime requirements are met.
- Include optional MCP tools only when the host and configuration allow them.
- Declare required client abilities such as vision or an interactive preview.

It must not:

- Run a predefined design workflow.
- Select the next tool.
- Auto-retry failed visual checks.
- Override approval or observation rules.

### 7.2 Where to assemble

Assemble capability packs near the existing built-in and MCP registration paths:

- `internal/runtime/registry.go`
- `internal/runtime/headless.go`
- daemon workspace registry setup when MCP is resolved per workspace

Keep `internal/agent/loop.go` unchanged. The loop should continue to reason over
whatever typed tools the registry exposes.

### 7.3 Do not overload platform Profile

`settings.Profile` describes host capability (`full` or `sandboxed`). The UI
adaptation pack is a task capability. They are orthogonal and should remain
separate.

For the first implementation, existing `agent.builtin_tools` can express a
temporary allowlist. Add a named capability layer only after the tools and skill
prove useful in real sessions.

## 8. MCP Boundary

Use MCP for external or host-specific services:

- Computer Use and GUI automation.
- Browser/Playwright execution.
- Xcode Device Hub, simulators, or remote device farms.
- Figma or another design source.
- Remote image processing or generation.

Do not use MCP for the whole capability because:

- MCP is a transport and integration protocol, not a workflow abstraction.
- Code-agent currently classifies every remote MCP tool as side-effecting.
- Side-effecting tools are approval-gated and serialized more conservatively.
- Core UI design should work locally and remain testable without an external
  server.

## 9. Evidence Model

Phase 1 can use the existing asset and output channels:

- Preview screenshot: `ToolResult.Assets` with `kind: "image"`.
- Structured viewport metadata: `ToolResult.Output`.
- Model visual observation: staged `LocalAssets`.
- Build and simulator evidence: `run_command` output and execution evidence.

If clients need a first-class matrix view, add a structured output such as:

```json
{
  "capability": "ui-adaptation",
  "variants": [
    {
      "id": "outer",
      "preview_asset_id": "asset_...",
      "native_asset_id": "asset_...",
      "verified": true
    }
  ],
  "build": "passed",
  "gaps": []
}
```

This is evidence data only. The model decides whether the evidence is sufficient
and what to do next. Reflection may surface missing evidence, but must not start
an automatic redesign loop.

## 10. Mapping from the Original Codex Session

| Original capability | code-agent equivalent |
| --- | --- |
| `visualize` skill | `ui-adaptation` Skill |
| HTML/CSS interactive preview | Generated workspace prototype plus `render_preview` |
| Built-in `view_image` | New built-in `view_image` Tool |
| Artifact card in chat | `ToolResult.Assets` and client asset preview |
| `cua_repl` desktop automation | Strict-client tool or optional computer-use MCP |
| Apple documentation browsing | Existing `web_search` and `web_fetch` |
| `xcodebuild` / simulator CLI | Existing `run_command` |
| Screenshot review loop | `render_preview` / capture, then `view_image` |
| Iterative SwiftUI implementation | Existing `read_file`, `grep`, `apply_patch`, and build tools |

## 11. Delivery Plan

### Phase 1: workflow validation

- Add `skills/ui-adaptation/SKILL.md`.
- Validate that the model researches platform guidance, inspects code, and
  designs before editing.
- Use existing tools only where possible.

### Phase 2: visual feedback loop

- Add `view_image`.
- Add focused tests for workspace paths, protected paths, MIME validation, asset
  references, and vision staging.
- Verify that image tool results reach the model as visual inputs.

### Phase 3: deterministic preview generation

- Add `render_preview` for local HTML/CSS prototypes.
- Support explicit viewport/theme variants and artifact output.
- Keep network access disabled by default.

### Phase 3.5: safe visual host

- Add a dedicated preview artifact directory and path allowlist.
- Materialize generated HTML through a trusted viewer rather than opening the
  model-authored file directly.
- Apply iframe sandboxing, CSP, size limits, canonical-path checks, and symlink
  rejection.
- If an interactive WebView is unavailable, render screenshots and use
  `view_image` as the minimum visual feedback loop.

### Phase 4: capability assembly

- Add the `ui-adaptation` preset or plugin-compatible capability manifest.
- Model the manifest after plugins with skills, tools, extensions, optional MCP
  servers, hooks, and client capability declarations.
- Keep tool registration out of the agent loop.
- Make activation explicit in settings or project configuration.

### Phase 5: external devices

- Add optional computer-use or device-farm MCP integration only where local
  execution is insufficient.
- Add structured UI evidence if asset cards alone are not enough for review.

## 12. Acceptance Criteria

- A UI task can retrieve authoritative platform guidance with citations.
- The model reads and understands the existing UI implementation before editing.
- Three target forms can be previewed and inspected in one session.
- Preview and native screenshots are visible assets, not base64 blobs in prose.
- The approved design is implemented without maintaining separate layout trees.
- Build and viewport-matrix checks are recorded.
- The turn distinguishes mockup evidence, native layout evidence, and complete
  end-to-end product verification.
- The agent loop contains no `ui-adaptation`, `view_image`, or `render_preview`
  special cases.

## 13. Resolved Scope Decisions

- Both CLI/TUI and the Talkify desktop client are first-release targets.
- Interactive HTML preview is required in v1 for both clients.
- Static screenshots remain required as evidence and fallback.
- Initial verification support is limited to Go and DreamLog/iOS.
- The Codex source and DreamLog Duo trajectory are reference baselines, not
  runtime dependencies.

### Remaining engineering questions

- Should the capability be user-enabled, project-enabled, or both?
- Is a local headless browser an acceptable runtime dependency?
- Which screenshots count as mandatory evidence for a release-ready UI change?
- Should preview rendering be a built-in tool or an optional MCP integration for
  desktop versus mobile hosts?

## 14. Codex Reference Implementation

This section records how the public Codex repository and the installed Codex
plugins implement the same class of capability. It distinguishes core runtime
support from bundled plugin content.

### 14.1 Source map

The in-repository implementation is concentrated in:

- [view_image handler](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/view_image.rs)
- [view_image schema](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/view_image_spec.rs)
- [tool planning and feature registration](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/spec_plan.rs)
- [inline visualization parser](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/tui/src/inline_visualization.rs)
- [visualization viewer](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/tui/src/inline_visualization/viewer.rs)
- [plugin manifest model](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/plugin/src/manifest.rs)
- [tool exposure model](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/tools/src/tool_executor.rs)
- [deferred tool search](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/tools/src/tool_search.rs)
- [image generation extension](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/ext/image-generation/src/extension.rs)
- [image generation tool](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/ext/image-generation/src/tool.rs)

The `visualize`, `computer-use`, and `unified-computer-use` packages are
installed plugins rather than core source in this repository. Their manifests
live under the local Codex plugin cache.

### 14.2 The four Codex layers

| Layer | Codex implementation | Consequence |
| --- | --- | --- |
| Plugin/Skill | Plugin manifest plus `SKILL.md`, references, scripts, assets | Task-specific knowledge can be distributed without changing code-agent core |
| Tool | Built-in handlers such as `view_image`; extension tools such as `imagegen`; MCP tools | Atomic capabilities remain typed and observable |
| Extension | `ToolContributor`, config and thread lifecycle contributors | Host-owned tools can be enabled conditionally without loop special cases |
| MCP | Plugin-declared servers such as `cua_repl` | Desktop and external-device control stays outside the core runtime |
| UI host | Inline visualization parser, materialized viewer, image items | Generated HTML and screenshots become safe, visible conversation artifacts |

This confirms that the proposed capability should not be reduced to one MCP
server or one Skill. The value comes from composing the layers.

### 14.3 `view_image`

Codex implements `view_image` as a core built-in tool, gated by the stable
`features.view_image` feature. The handler:

1. Checks that the selected model accepts image input.
2. Resolves the path through the selected execution environment.
3. Reads the file through the environment filesystem and sandbox context.
4. Verifies that the bytes decode as an image before exposing them.
5. Emits an `ImageViewItem` turn item for the client.
6. Returns an inline image content item to the model.
7. Supports `high` and `original` detail through model capability checks.
8. Omits image bytes from logs.

Codex uses an inline data URL because its model wire format consumes image
content items directly. Code-agent already has `ToolResult.Assets` and local
asset staging, so it should adapt the behavior to its own asset model rather
than copying the wire representation.

### 14.4 Interactive visualization pipeline

The Visualize plugin contains a Skill and associated scripts/assets. It does not
declare an MCP server. The Skill requires the model to write an HTML fragment,
not a complete document, and the host supplies the surrounding runtime.

The in-repository TUI fallback then:

1. Accepts a directive such as `::codex-inline-vis{file="chart.html"}`.
2. Restricts references to a thread-scoped visualization directory.
3. Allows only a single `.html` file component under that directory.
4. Canonicalizes both roots and rejects path escape and symbolic links.
5. Applies a size limit. The Skill recommends under 1 MB; the runtime hard cap
   is 2 MB in the inspected revision.
6. Materializes a trusted viewer document under `CODEX_HOME`.
7. Renders the fragment in an iframe with `sandbox="allow-scripts"`.
8. Applies a restrictive Content Security Policy with an explicit CDN/resource
   allowlist.
9. Rewrites the directive into a trusted local visualization link.

The important design decision is separation: the model authors a fragment, but
the runtime controls path resolution, viewer construction, and browser policy.

### 14.5 Plugin and Skill packaging

Codex plugin manifests support:

- Skills.
- MCP server declarations.
- Apps.
- Hooks.
- Interface metadata and discovery assets.

The `visualize` plugin demonstrates a skill-only capability. The Computer Use
plugins demonstrate a skill plus MCP and hook composition. This is the strongest
reference for the code-agent capability manifest: capabilities should be
packaged, not hard-coded into the loop.

Codex also supports progressive skill disclosure in `ext/skills`: a compact
catalog is exposed to the model, selected skill instructions are loaded for the
turn, and explicit skill mentions are resolved from the user input. The
experimental selector code is not required for the code-agent MVP.

### 14.6 Tool exposure and deferred discovery

Codex does not model registration as a binary visible/hidden decision. Tool
exposure includes:

- `Direct`: present in the initial model tool list.
- `Deferred`: discoverable through `tool_search`.
- `CodeModeOnly`: callable only from nested code.
- `DirectModelOnly`: normal model tool, excluded from code mode.
- `Hidden`: registered for dispatch, not visible to the model.

Deferred tool search uses indexed tool metadata and BM25 ranking. This is the
long-term answer to tool sprawl if code-agent accumulates many optional design,
device, image, and browser tools.

For the first code-agent implementation, a capability manifest plus ordinary
tool registration is sufficient. Add deferred discovery only after the optional
tool set becomes large enough to burden the prompt.

### 14.7 Extension tools

Image generation is implemented as a Codex extension, not as a special case in
the agent loop. The extension:

- Implements `ToolContributor`.
- Tracks thread configuration and provider availability.
- Uses lifecycle contributors to refresh state.
- Saves generated images under `generated_images/<session>/`.
- Emits image-generation turn items and events.

This is the reference pattern for a future code-agent preview renderer or image
service adapter that needs host-specific dependencies.

### 14.8 Computer Use via MCP

The installed Unified Computer Use plugin bundles an MCP server named
`cua_repl` and hooks for turn termination. The MCP server wraps a persistent
Node REPL with browser and native-app control APIs.

The layer boundary is clear:

- Skill: how to use Computer Use safely.
- MCP: access the host's computer-use runtime.
- Hook: notify the external runtime when the turn ends.
- Core runtime: permissions, tool calls, and event delivery.

## 15. Revised code-agent Architecture

The design should use the following dependency direction:

```text
Capability manifest / Plugin
        ├── declares Skills
        ├── requests Tools
        ├── requests Extensions
        ├── enables optional MCP servers
        └── declares client capabilities
                    ↓
Runtime registry and permission boundary
                    ↓
Agent loop (tool-agnostic)
                    ↓
Tool results + assets + visual evidence
                    ↓
Client visual host
```

### 15.1 Copied directly

- Skill as procedural guidance, not execution.
- Tool results carrying visible visual assets.
- A dedicated `view_image` capability.
- Explicit host capability and model-modality checks.
- Safe path handling, size limits, and image validation.
- Optional MCP for desktop and external-device control.
- A tool-agnostic loop.

### 15.2 Adapted to code-agent

- Codex inline image data URLs become code-agent `ToolResult.Assets` and
  `LocalAssets`.
- Codex extension contributors become a small capability manifest and tool
  construction layer.
- Codex plugin marketplace packaging can begin as a repository-local capability
  manifest rather than a full remote marketplace.
- Codex tool exposure modes can begin with ordinary visible/hidden registration.
- Codex TUI visualization directives can be implemented as a client-side asset
  preview contract; a static screenshot fallback is required first.

### 15.3 Deferred until justified

- BM25 deferred tool search.
- Remote capability marketplace and signed plugin installation.
- Rich host-provided `Tweak` controls and carousel APIs.
- Arbitrary WebView navigation or network access.
- Full plugin hooks parity.

## 16. Visual Host Security Contract

If code-agent adds interactive previews, the host must enforce the following
minimum contract:

- Model output is an HTML fragment, not a complete document.
- Artifacts live in a dedicated, task-owned preview directory.
- Resolve canonical paths and reject traversal and symlinks.
- Apply a hard fragment size limit.
- Render only in a sandboxed iframe without same-origin access.
- Apply a restrictive CSP; deny network access by default.
- Do not expose local filesystem paths or credentials to page scripts.
- Do not treat preview interaction as an approval or state mutation.
- Route screenshot assets through `view_image` and the existing vision budget.
- Preserve the preview artifacts for replay, but never execute model-authored
  JavaScript outside the sandbox.

## 17. Updated Delivery Decision

The recommended build order is now:

1. `ui-adaptation` Skill.
2. Built-in `view_image` using existing asset side channels.
3. Static preview rendering and screenshot review.
4. Plugin-compatible capability manifest in code-agent.
5. Sandboxed interactive HTML preview host.
6. Optional Computer Use or device-farm MCP.
7. Deferred tool discovery only if the optional tool catalog becomes large.

This keeps the core loop simple while preserving the complete Codex-style
feedback loop:

```text
research -> preview -> approve -> implement -> verify visually -> correct
```
