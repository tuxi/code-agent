# Reference Baselines

Status: accepted inputs
Date: 2026-11-09

## 1. Codex Source Baseline

Repository:

```text
/Users/xiaoyuan/Documents/work/git/codex
```

The following patterns are authoritative references for the upgrade:

- Built-in media tooling and model-modality gating.
  - `codex-rs/core/src/tools/handlers/view_image.rs`
  - `codex-rs/core/src/tools/handlers/view_image_spec.rs`
- Tool planning, feature gating, and exposure.
  - `codex-rs/core/src/tools/spec_plan.rs`
  - `codex-rs/tools/src/tool_executor.rs`
  - `codex-rs/tools/src/tool_search.rs`
- Safe inline HTML materialization and rendering.
  - `codex-rs/tui/src/inline_visualization.rs`
  - `codex-rs/tui/src/inline_visualization/viewer.rs`
- Plugin packaging.
  - `codex-rs/plugin/src/manifest.rs`
- Extension-owned tools.
  - `codex-rs/ext/image-generation/src/extension.rs`
  - `codex-rs/ext/image-generation/src/tool.rs`

The installed Visualize, Computer Use, and Unified Computer Use packages are
plugin references. They demonstrate that a capability can be skill-only,
skill-plus-MCP, or skill-plus-MCP-plus-hooks.

### Reuse rule

Reuse principles and interfaces, not implementation details that do not fit
code-agent:

- Codex inline image data URLs map to code-agent `ToolResult.Assets` and
  `LocalAssets`.
- Codex extension contributors map to a smaller code-agent capability/extension
  registry.
- Codex TUI-specific viewer code is a security reference, not a cross-platform
  client implementation.
- Codex deferred tool search and Code Mode remain post-v1 options.

## 2. DreamLog Duo Baseline

Source conversation:

```text
01a1b6c7-9f1b-7730-b944-eaec720aaaa5
```

The observed trajectory includes the capabilities the upgrade must make
first-class and reproducible:

- Official platform documentation research.
- Reading the existing SwiftUI implementation and assets.
- Creating an interactive HTML/CSS preview.
- Switching between outer screen, inner portrait, and inner landscape forms.
- Implementing the approved design with shared SwiftUI components and state.
- Building with `xcodebuild`.
- Using simulator/device tooling to capture screenshots.
- Inspecting screenshots and correcting visual issues.
- Separating design evidence, native implementation evidence, and incomplete
  end-to-end verification.

### Reuse rule

The DreamLog conversation is a validation fixture, not a runtime rule:

- Do not hard-code its session ID, file paths, device names, tool names, or
  specific layout thresholds.
- Use it to define acceptance scenarios and regression tests.
- Re-run the scenario after each milestone that affects capabilities, tools,
  assets, or visual rendering.

## 3. AgentKit GUI Baseline

Repository:

```text
/Users/xiaoyuan/Documents/work/git/AgentKit
```

AgentKit is the Agent GUI layer used by Talkify. The following interfaces are
the integration baseline:

- `Sources/AgentKit/Features/Conversation/TimelineExtensions/TimelineExtension.swift`
  - Product-specific Timeline extensions.
  - Semantic `WebTimelineNode` and `TimelineWebDocument` models.
  - AgentKit ownership of HTML generation, action tokens, navigation, and
    security policy.
- `Sources/AgentKit/Features/Inspector/TimelineDocumentInspectorView.swift`
  - Current script-disabled HTML Inspector shell.
  - Navigation delegate and CSP behavior.
- `Sources/AgentKit/Features/Inspector/AssetInspectorView.swift`
  - Generic asset list and asset preview surfaces.
- `Sources/AgentKit/Core/AgentCapabilityFlags.swift`
  - Runtime/client capability negotiation.
- `Sources/AgentKit/Core/RuntimeHTTPClient.swift`
  - Runtime asset preview/content APIs.

The current HTML document Inspector is a security baseline, not the interactive
preview implementation. Interactive preview must be a first-class AgentKit
surface with explicit JavaScript and navigation policy isolated from the
script-disabled evidence viewer.
