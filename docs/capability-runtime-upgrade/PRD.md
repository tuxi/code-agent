# Capability Runtime Upgrade Requirements

Status: scope accepted; engineering plan pending
Date: 2026-11-09
Related: [GOALS.md](./GOALS.md), [UI Adaptation Capability Design](../ui-adaptation-capability.md)

## 1. Problem

code-agent already has strong runtime primitives:

- Typed tools with inspection, validation, side-effect, client-execution, and
  structured-output interfaces.
- Approval and path-boundary enforcement.
- Progressive-disclosure skills.
- MCP tool adaptation.
- Tool-result assets and vision-model image injection.
- Observation, reflection, background jobs, and subagents.

These primitives are still assembled mainly for built-in coding tools. There is
no general layer that lets a capability package declare and activate a coherent
set of skills, tools, extensions, MCP servers, client capabilities, and evidence
types.

The DreamLog Duo task exposes the gap: platform research, interactive UI
previews, screenshot feedback, native implementation, simulator control, and
visual verification cannot currently be composed as one explicit capability.

## 2. Current State

| Area | Current implementation | Gap |
| --- | --- | --- |
| Tool interface | `internal/tools/tool.go` supports typed tools, inspector, side effects, validation, client tools, output schemas | No general exposure model or capability-scoped registration |
| Tool assembly | `internal/runtime/registry.go` registers a central built-in list | New host tools require central edits |
| Skills | `internal/skills` supports progressive disclosure and `load_skill` | Skills cannot declare or activate paired tools |
| Plugins | `internal/plugins` installs skill-only marketplace bundles | No tools, extensions, MCP, hooks, or client capability declarations |
| MCP | `internal/mcp` adapts remote tools into the tool registry | Remote tools are conservatively side-effecting; no capability packaging layer |
| Assets and vision | `ToolResult.Assets` and local image staging already exist | No built-in `view_image`, capture contract, or rich preview host |
| Observation | `internal/observation` classifies command failures | Classification and verification are Go-first |
| Reflection | `internal/reflection` detects mutations and verify commands | No visual, device, or generic evidence signals |
| Client protocol | Tool, asset, and structured output events exist | No generic client capability negotiation or sandboxed HTML preview contract |

## 3. Target Architecture

```text
Plugin/Skill
  declares methods and package resources
        ↓
Capability Manifest
  declares tools, extensions, MCP, client capabilities, evidence
        ↓
Runtime Assembly
  validates, registers, gates, and emits lifecycle events
        ↓
Tool / Extension / MCP
  execute atomic operations and return output, assets, and evidence
        ↓
Agent Loop
  remains tool-agnostic and model-driven
        ↓
Client Visual Host
  safely renders assets and preserves replayable evidence
```

### 3.1 Layer responsibilities

| Layer | Responsibilities | Explicit exclusions |
| --- | --- | --- |
| Plugin/Skill | Task playbooks, references, scripts, resources, capability metadata | Execution and approval bypass |
| Tool | One typed atomic operation | Multi-step workflow ownership |
| Extension | Host integration, tool contribution, lifecycle/config hooks | Agent decisions or loop branching |
| MCP | External service and device integration | Core capability policy |
| UI host | Safe rendering, asset presentation, preview lifecycle | Tool selection or automatic correction |
| Runtime | Registration, validation, permissions, events, assets, evidence | Hard-coded capability behavior |

### 3.2 Client support matrix

Interactive HTML is a v1 requirement for both first-party clients. The artifact
format and security boundary must be shared, while each client provides its own
safe host implementation.

| Client | Interactive HTML host | Required behavior |
| --- | --- | --- |
| CLI/TUI | Materialized trusted viewer opened in the system browser or a dedicated local preview surface | The runtime creates the viewer under a controlled artifact root; the client must not open raw model-authored HTML directly |
| Talkify desktop | AgentKit-owned interactive preview block directly inside ConversationWorkbench | Use a non-persistent, restricted WebKit configuration; JavaScript is enabled only for the isolated preview; remote navigation and arbitrary local-file access are denied |

Talkify already contains a WebKit evidence surface in
`/Users/xiaoyuan/Documents/work/git/chater/Talkify/DesktopControlEvidenceTimeline.swift`,
but it currently disables JavaScript. The interactive preview must be a separate
controlled surface rather than weakening that existing evidence viewer.

Talkify uses AgentKit as its Agent GUI. AgentKit already has a macOS
`ConversationWebWorkbenchView` with a non-persistent WebKit store, custom scheme
handler, JavaScript enabled for the trusted conversation shell, and a typed
action bridge. Its Inspector HTML viewer is intentionally script-disabled and
must remain that way.

V1 therefore adds a first-class interactive preview block in AgentKit. The block
is rendered directly inside the ConversationWorkbench but runs its preview
document in an isolated iframe or child WebView. Model-authored preview
JavaScript must not share the main conversation DOM, message handler, or native
bridge.

Talkify's existing `WebTimelineExtension` and `tool_finished` projection points
may still host product-specific evidence cards, but generic interactive preview
must be supplied by AgentKit. The preview feature must not depend on the
desktop-control plugin or its private render models.

## 4. Functional Requirements

### P0: Core platform requirements

#### FR-1. Capability manifest

The runtime must support a versioned capability manifest containing:

- Name, description, and version.
- Skill references.
- Tool names.
- Extension names.
- Optional MCP server references.
- Required client capabilities.
- Supported evidence kinds.

Manifest parsing must fail closed on unknown required fields or missing
dependencies.

#### FR-2. Explicit activation

Capabilities must be activated by explicit user, project, or session
configuration. The runtime must not infer activation through keyword matching.

Activation must emit a traceable event containing:

- Capability name and version.
- Activation source.
- Resolved tools and extensions.
- Optional MCP servers.
- Missing or degraded client capabilities.

#### FR-3. Extension/tool contribution boundary

The runtime must provide a host-owned extension boundary that can:

- Contribute tools to the normal registry.
- Inspect host/model/client prerequisites.
- Participate in configuration or session lifecycle.
- Degrade cleanly when dependencies are unavailable.

Adding an extension must not require edits to `internal/agent/loop.go`.

#### FR-4. Tool exposure

Visibility and execution are separate dimensions in v1:

- `model_visibility`: `direct` or `hidden`.
- `executor`: `server`, `client`, or `flex`.

`client` describes who executes the tool, not whether the model can see it.

The design must leave room for later `deferred` and `code_mode` exposure without
breaking the tool contract.

#### FR-5. Skill integration

Activated capabilities must be able to contribute skills to the existing
progressive-disclosure index. Skill resources must remain on-demand and must not
be injected in full into the base prompt.

#### FR-6. Client capability negotiation

The runtime must represent required client abilities, including:

- Vision input.
- Image/asset preview.
- Interactive WebView.
- Screen capture.
- Device or simulator control.

Unsupported capabilities must cause an explicit degraded or blocked result, not
silent failure.

#### FR-7. `view_image`

The runtime must provide a read-only image-viewing tool that:

- Respects workspace and external-path approval rules.
- Validates image data.
- Returns an image asset or vision-compatible content item.
- Enforces image size and request budgets.
- Does not log image bytes.
- Is registered only when the selected model supports image input or explicitly
  returns a structured unsupported-capability result.

#### FR-8. Asset preview contract

Tool results must be able to expose visual assets with stable IDs, MIME type,
display name, source call, and a client-safe preview reference.

Binary data must not be embedded in ordinary model text when the asset channel is
available.

#### FR-9. Structured evidence

The runtime must support structured evidence records for at least:

- Command execution.
- Build and test result.
- Visual preview screenshot.
- Native screenshot or device matrix.
- Missing/unavailable verification.

Evidence is data. The runtime must not use it to start automatic retries or
design changes.

#### FR-10. Configurable verification

Projects must be able to declare verification commands independent of a
hard-coded Go-first list. Existing Go defaults must remain unchanged when no
override is present. V1 explicitly supports Go and DreamLog/iOS verification;
the contract must remain extensible for later toolchains.

### P0: V1 client requirements

#### FR-11. Safe HTML preview host

The client/runtime boundary must:

- Accept only HTML fragments from a dedicated preview root.
- Canonicalize paths and reject traversal and symlinks.
- Enforce a hard size limit.
- Generate a trusted viewer document.
- Use iframe sandboxing without same-origin access.
- Apply a restrictive CSP and deny arbitrary network access by default.
- Keep generated preview artifacts for replay.

#### FR-12. Interactive host support for both clients

Both CLI/TUI and Talkify desktop must implement the same preview capability
contract:

- Discover that `interactive_html` is supported.
- Request or receive a trusted preview artifact.
- Render it under the common security boundary.
- Preserve stable artifact identifiers for replay.
- Fall back to static screenshots when interactive rendering is unavailable.

#### FR-13. Screenshot matrix

The runtime must support a viewport/theme/device matrix and associate each result
with stable evidence IDs and visual assets.

### P1/P2: Explicitly post-v1

- Deferred tool search and BM25 discovery.
- Full Code Mode execution.
- Remote plugin marketplace.
- Plugin signing and supply-chain verification.
- Rich host-provided widget controls.
- Automatic visual regression correction.

Deferred Tool Discovery, tool search, and Code Mode are explicitly post-v1.

## 5. Non-functional Requirements

### NFR-1. Backward compatibility

Existing tools, skills, sessions, MCP servers, approvals, verification contracts,
background jobs, and subagents must keep working.

### NFR-2. Tool-loop independence

`internal/agent/loop.go` must not contain capability names, capability-specific
tool names, or capability-specific branching.

### NFR-3. Traceability

Every capability activation, tool call, approval, asset creation, and evidence
record must be correlated by session, turn, call, and capability where available.

### NFR-4. Failure isolation

A missing extension, unavailable client capability, failed MCP server, or
invalid manifest must disable or degrade only the affected capability.

### NFR-5. Prompt budget

Capability activation must not inject all tool schemas or all skill bodies into
the base prompt. Only the active index and required direct tools may be present.

### NFR-6. Security

- No silent activation.
- No unsandboxed execution of model-authored HTML.
- No arbitrary filesystem access from visual previews.
- No bypass of existing approval or path protections.
- No logging of secret-bearing or binary tool content.

### NFR-7. Testability

Capability manifests, extension registration, tool exposure, asset routing,
visual host security, and evidence aggregation must have unit or integration
tests.

## 6. Data Contracts

### 6.1 Capability manifest

```go
type CapabilityManifest struct {
    Name               string
    Version            string
    Description        string
    Skills             []string
    ToolBindings       []ToolBinding
    Extensions         []string
    OptionalMCP        []string
    ClientCapabilities []string
    EvidenceKinds      []string
}

type ToolBinding struct {
    Name            string
    ModelVisibility string // direct | hidden
    Executor        string // server | client | flex
}
```

### 6.2 Client capabilities

```go
type ClientCapabilities struct {
    VisionInput      bool
    AssetPreview     bool
    InteractiveHTML  bool
    ScreenCapture    bool
    DeviceControl    bool
}
```

Preview transport negotiation uses the canonical wire strings in
[PREVIEW_CONTRACT_V1_6.md](./PREVIEW_CONTRACT_V1_6.md): `preview_contract_v1`,
`preview_interactive_html_v1`, `preview_static_asset_v1`, and
`preview_replay_v1`.

### 6.3 Evidence envelope

```go
type Evidence struct {
    ID         string
    Kind       string
    Capability string
    Status     string
    Output     json.RawMessage
    Assets     []assets.Ref
    Gaps       []string
}
```

## 7. Milestones and Exit Criteria

| Milestone | Deliverables | Exit criteria |
| --- | --- | --- |
| M0 Requirements | Goals, PRD, approval record | User approves scope and priority |
| M1 Substrate | Manifest, extension boundary, exposure, lifecycle events | Sample capability activates with no loop edits |
| M2 Media | `view_image`, asset preview, vision routing | Image tool result reaches a vision model and client |
| M3 Evidence | Verification config, evidence envelope, tests | Build/test/visual evidence can be recorded together |
| M4 Visual host | Fragment validation, sandbox, CSP, replay | Security tests pass and preview survives replay |
| M5 UI adaptation | Skill, preview renderer, DreamLog scenario | Duo design loop completes without capability-specific loop code |
| M6 Advanced | Deferred tools, Code Mode, marketplace | Only pursued after explicit product decision |

## 8. Acceptance Criteria

The upgrade is accepted when:

1. Existing tests and builds remain green.
2. A sample capability is added without changing the agent loop.
3. Capability activation and degradation are observable.
4. `view_image` and the asset channel form a complete visual feedback loop.
5. Visual previews render directly in the AgentKit ConversationWorkbench under a
   tested security boundary.
6. Verification evidence represents the initial Go and DreamLog/iOS toolchains,
   and the evidence contract remains extensible.
7. The DreamLog Duo validation task can research platform guidance, generate a
   three-form interactive preview, obtain approval, implement native UI, build,
   capture the device matrix, and inspect screenshots in both CLI/TUI and
   Talkify desktop.
8. The implementation contains no special-case UI design state machine.

## 9. Risks

| Risk | Mitigation |
| --- | --- |
| Building a premature plugin ecosystem | Start with repository-local manifests |
| Tool and prompt explosion | Explicit exposure first; deferred search later |
| Over-coupling extension code to the core loop | Enforce contributor interfaces and loop tests |
| Unsafe HTML execution | Dedicated root, canonicalization, sandbox, CSP, size limits |
| Breaking current coding workflows | Compatibility suite and opt-in capability activation |
| Code-centric verification | Project-declared verification and generic evidence |
| Client capability mismatch | Explicit negotiation and degraded mode |

## 10. Approval Gate

The accepted scope is recorded in [DECISIONS.md](./DECISIONS.md).

Implementation remains blocked until the engineering plan is approved:

- The shared interactive HTML host contract for CLI/TUI and Talkify desktop.
- The capability manifest and extension interfaces.
- The milestone order and test strategy.
- The remaining v1 defaults for project-local manifests and deferred discovery.
