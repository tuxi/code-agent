# Capability Runtime Upgrade Goals

Status: scope accepted; engineering plan pending
Date: 2026-11-09

## 1. Upgrade Question

Can code-agent expose, compose, observe, and safely render capabilities that are
not built into the core coding loop, while preserving its current guarantees:
model-owned control flow, explicit tools, approvals, traceability, and
tool-loop independence?

The DreamLog Duo UI adaptation task is the validation scenario, not the full
scope. The upgrade succeeds only if the resulting runtime also supports future
media, browser, device, design, data, and external-service capabilities.

## 2. Primary Outcome

Code-agent should move from:

```text
fixed coding tools + skills + MCP adapters
```

to:

```text
capability manifests
    -> explicit tool, extension, skill, and MCP composition
    -> safe client rendering and evidence
    -> tool-agnostic agent loop
```

## 3. Goals

### G1. Capability composition without loop changes

A new capability must be activatable without adding tool-name branches,
keyword routing, or capability-specific state machines to `internal/agent/loop.go`.

### G2. Extensible tool registration

Host-owned tools must be registrable through an extension/contributor boundary
instead of requiring all tools to be added to one central built-in list.

### G3. Explicit tool exposure

The runtime must distinguish at least:

- tools visible in the initial model tool list;
- tools registered for runtime/client dispatch but hidden from the model;
- capability-required client tools.

Deferred discovery and Code Mode may follow after the initial substrate is
stable.

### G4. Plugin-compatible packaging

A capability package must be able to declare:

- Skills.
- Tools.
- Extensions.
- Optional MCP servers.
- Required client capabilities.
- Evidence expectations.

The first implementation may be repository-local; a remote marketplace is not
required.

### G5. Multimodal output and evidence

Tool results must support text, structured output, local assets, remote assets,
screenshots, and evidence references without embedding large binary payloads in
model prose.

### G6. Safe visual host

Both first-party clients must support interactive HTML in v1. Each host must
enforce a bounded rendering contract: dedicated artifact roots, canonical path
checks, symlink rejection, size limits, iframe or WebKit sandboxing, CSP,
remote-navigation denial, and no arbitrary network access by default.

### G7. General verification evidence

Verification must stop being defined only in terms of Go commands and command
failure markers. A project must be able to describe language- or product-specific
verification commands and evidence kinds, including visual and device evidence.

### G8. Observable capability lifecycle

Capability activation, tool exposure, extension registration, MCP connection,
asset creation, visual verification, and approval decisions must be replayable
and traceable through runtime events.

### G9. Backward compatibility

Existing coding workflows, tool approvals, skills, sessions, MCP servers,
background jobs, and subagents must continue to work during and after the
upgrade.

## 4. Success Metrics

The upgrade is not considered successful based only on feature presence. It must
meet these outcomes:

1. Adding a sample capability requires zero changes to the agent loop.
2. A capability can be enabled and disabled through explicit configuration.
3. Capability activation and tool exposure are visible in session events.
4. Tool result images can be safely read and injected into a vision-capable model.
5. A preview asset can be displayed by a client without exposing arbitrary local
   files or executing unsandboxed model-authored JavaScript.
6. A turn can record a structured verification result for build, tests, and a
   visual/device matrix.
7. Existing `go build`, `go test`, and current MCP/tool tests remain green.
8. The DreamLog UI adaptation scenario completes without any
   `ui-adaptation`-specific branch in the agent loop.

## 5. Non-goals for the first release

- Full Codex plugin-marketplace parity.
- Signed remote plugin distribution.
- BM25 deferred tool discovery in the first milestone.
- Full Code Mode execution.
- Automatic design generation or automatic visual correction loops.
- Runtime keyword matching that injects skills or capabilities implicitly.
- Treating mockups as proof of production implementation.
- Allowing model-authored HTML to open arbitrary URLs, read local files, or gain
  same-origin browser access.

## 6. Milestones

### M0. Requirements approval

- Goals and PRD approved.
- Priority and scope confirmed.
- No production implementation starts before this milestone closes.

### M1. Capability substrate

- Capability manifest.
- Extension/tool contributor boundary.
- Explicit tool exposure.
- Capability activation events.

### M2. Media and visual feedback

- `view_image`.
- Asset preview contract.
- Vision-safe image handling.
- Static screenshot evidence.

### M3. General verification

- Configurable verification commands.
- Structured evidence envelope.
- Language-neutral result classification where feasible.

### M4. Safe visual host

- Interactive HTML preview host shared by CLI/TUI and Talkify desktop.
- Sandbox and CSP tests.
- Replayable preview artifacts.

### M5. Validation capability

- `ui-adaptation` skill and capability pack.
- DreamLog Duo scenario as the end-to-end release gate.

### M6. Advanced discovery

- Deferred tool search, Code Mode, or remote plugin distribution only if the
  earlier milestones show a concrete need.

## 7. Approval Criteria

This document set is approved when:

- The four-layer boundary plus UI safety layer is accepted.
- P0 requirements are accepted.
- Non-goals are accepted.
- Milestone order is accepted.
- The DreamLog scenario is accepted as validation, not the product boundary.
- Explicit approval is recorded before implementation begins.

## 8. Accepted Scope Decisions

1. The first client targets are both CLI/TUI and the Talkify desktop client in
   `/Users/xiaoyuan/Documents/work/git/chater`.
2. Interactive HTML preview is required in v1. A static screenshot fallback is
   still required for degraded or replay-only contexts, but it does not replace
   the interactive path.
3. Initial verification support is limited to Go and DreamLog/iOS. Additional
   toolchains follow after the first end-to-end validation.
4. Generic Agent GUI and interactive preview behavior belong in AgentKit; Talkify
   should not duplicate it in a tool-specific Timeline extension.
5. Deferred Tool Discovery, tool search, and Code Mode remain post-v1.
6. AgentKit renders generic interactive previews directly in the conversation
   workbench, with expansion and replay semantics owned by AgentKit.
7. Backend and AgentKit are implemented as two coordinated workstreams; Talkify
   only wires the shared AgentKit surface into the app.

## 9. Engineering Decisions Still Required

1. Confirm that capability manifests are project-local only in v1.
2. Approve the CLI/TUI and AgentKit HTML host designs and their shared capability
   contract.
3. Approve the implementation milestones and test strategy before coding.
