# Capability Runtime Upgrade Delivery Plan

Status: R0 frozen; R1/A1 approved to start in parallel
Date: 2026-11-09
Related: [PRD.md](./PRD.md), [PREVIEW_CONTRACT_V1_6.md](./PREVIEW_CONTRACT_V1_6.md), [AGENTKIT_INTEGRATION.md](./AGENTKIT_INTEGRATION.md)

## 1. Delivery Model

Implementation is split into two independent workstreams:

1. **code-agent backend/runtime**
2. **AgentKit GUI**

Talkify remains a thin integration consumer. The reviewer role is independent of
both implementation tracks and is responsible for contract, architecture,
security, integration, and evidence review.

```text
Shared preview contract
        ├── code-agent backend implementation
        └── AgentKit GUI implementation
                    ↓
              Integration
                    ↓
          Independent review gate
                    ↓
        CLI/TUI + Talkify acceptance
```

## 2. Contract-First Rule

No parallel implementation begins until the shared preview contract is frozen.

The contract must define:

- Preview artifact identity and version.
- Capability and preview capability flags.
- Viewer/asset URLs or references.
- Conversation, turn, tool-call, and artifact correlation IDs.
- Interactive/static/degraded states.
- CSP and navigation requirements.
- Host-owned control semantics; v1 has no preview action bridge.
- Replay semantics and immutable evidence snapshots.
- Fixture payloads used by both implementations.

Contract changes after implementation begins require:

- A version bump or explicit compatible extension.
- Re-running both test suites.
- Reviewer approval.

## 3. Workstream A: code-agent Backend/Runtime

### Scope

- Capability manifest and explicit activation.
- Extension/tool contributor boundary.
- Tool exposure: `model_visibility=direct|hidden` and
  `executor=server|client|flex`.
- Preview artifact creation and validation.
- Runtime asset endpoints and metadata.
- Trusted viewer materialization.
- Runtime capability negotiation.
- Capability lifecycle and evidence events.
- CLI/TUI preview entry and trusted local browser/preview flow.
- Backend contract fixtures and tests.

### Explicit boundaries

- No AgentKit-specific or Talkify-specific branches.
- No UI design tool names in `internal/agent/loop.go`.
- No direct dependency on SwiftUI, WebKit, or AppKit.
- No raw model-authored HTML execution outside the trusted viewer contract.
- Deferred tool discovery and Code Mode are post-v1.

### Required handoff artifacts

- Contract version and schema.
- Example preview artifact.
- Example asset/preview response.
- Security metadata and threat model notes.
- Backend test command and results.
- Fixture server or test harness usable by AgentKit tests.

## 4. Workstream B: AgentKit GUI

### Scope

- Canonical `PreviewArtifact` decode and `PreviewArtifactStore`.
- Client capability flags and negotiation.
- First-class preview block in macOS ConversationWorkbench.
- Sandboxed iframe as the primary interactive host; tested child WebView fallback.
- Generic timeline card and expansion behavior.
- Host-owned open/collapse/expand/retry controls; no v1 preview action bridge.
- Replay, restore, and static fallback.
- Static asset and Inspector integration where useful.
- AgentKit contract fixtures and tests.

### Explicit boundaries

- Generic preview must not be implemented as a new Talkify
  `TimelineExtension`.
- Model-authored preview scripts must not access the main ConversationWorkbench
  DOM, existing native message handler, or arbitrary native APIs.
- Existing script-disabled `TimelineDocumentInspectorView` must remain
  script-disabled.
- Preview logic must not parse specific tool names such as `view_image` or
  `ui_preview`.

### Required handoff artifacts

- AgentKit feature branch and changed-file summary.
- Contract fixture test results.
- Security tests for iframe/WebKit navigation and CSP.
- Screenshots or recordings of preview in ConversationWorkbench.
- Replay/degraded-state demonstration.

## 5. Talkify Integration

Talkify's implementation should be limited to:

- Updating the AgentKit dependency.
- Wiring any host-level WebKit/runtime prerequisites.
- Updating app shell navigation or window layout if required.
- Removing or avoiding duplicate preview-specific product extensions.

Talkify must not own:

- Preview artifact parsing.
- Generic preview rendering.
- Generic preview security policy.
- Tool-name-to-preview routing.

## 6. Review Ownership

The reviewer is responsible for:

- Approving or rejecting the shared contract.
- Checking backend and AgentKit are independently implementing the same contract.
- Rejecting hidden cross-repository assumptions.
- Verifying that generic preview is in AgentKit ConversationWorkbench.
- Verifying that Talkify remains a thin integration consumer.
- Reviewing the security boundary and threat model.
- Reviewing tests, fixtures, screenshots, replay behavior, and failure states.
- Running or independently checking the required validation commands.
- Approving the final integration result before release.

The reviewer does not implement either workstream, avoiding self-review of the
implemented code.

## 7. Review Gates

### R0. Contract freeze

Inputs:

- [PREVIEW_CONTRACT_V1_6.md](./PREVIEW_CONTRACT_V1_6.md).
- Fixture artifact and asset payloads.
- Capability flags.
- Security requirements.

Exit:

- Both workstreams can implement without inventing missing fields.
- Reviewer approves contract version.

### R1. Backend readiness

Inputs:

- Backend implementation and tests.
- Capability activation/events.
- Preview materialization and asset serving.
- CLI/TUI preview entry.

Exit:

- Contract tests pass.
- No loop special cases.
- Reviewer approves backend surface.

### R2. AgentKit readiness

Inputs:

- AgentKit preview module and tests.
- ConversationWorkbench inline rendering.
- Isolated preview security host.
- Replay and fallback behavior.

Exit:

- Contract fixture tests pass.
- Security tests pass.
- Reviewer approves GUI surface.

### R3. Integration readiness

Inputs:

- Backend + AgentKit combined run.
- Talkify dependency integration.
- CLI/TUI flow.
- DreamLog Duo validation scenario.

Exit:

- Same preview artifact works in CLI/TUI and Talkify.
- No tool-specific preview extension is required.
- Reviewer approves cross-repository integration.

### R4. Release acceptance

Inputs:

- Full build/test evidence.
- Screenshots/recordings for outer screen, inner portrait, and inner landscape.
- Replay evidence.
- Security/degraded-state evidence.

Exit:

- All PRD acceptance criteria pass.
- Existing coding workflows remain green.
- Reviewer records final approval.

## 8. Required Validation Evidence

Backend:

- `go test ./internal/...`
- `go build ./...`
- capability activation and preview contract tests
- asset endpoint and path-boundary tests

AgentKit:

- package build/tests for the affected targets
- preview contract fixture tests
- WebKit/iframe isolation tests
- ConversationWorkbench inline rendering tests
- replay and fallback tests

Integration:

- CLI/TUI interactive preview demonstration
- Talkify/AgentKit interactive preview demonstration
- DreamLog three-form preview and native screenshot review
- explicit degraded behavior when WebKit or preview capability is unavailable

## 9. Release Blockers

- Contract mismatch between backend and AgentKit.
- Any generic preview implemented as a tool-specific Talkify extension.
- Model-authored JavaScript sharing the main ConversationWorkbench DOM or bridge.
- Missing replay or static fallback.
- Missing capability/degraded-state reporting.
- Backend branch special-casing AgentKit or specific tools in the agent loop.
- Reviewer cannot reproduce the reported evidence.
