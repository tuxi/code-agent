# Capability Runtime Upgrade Decisions

Status: accepted scope; engineering decisions pending
Date: 2026-11-09

## Accepted

### D-001. Four layers plus UI boundary

The upgrade uses four capability layers plus a client-side rendering and safety
boundary:

1. Plugin/Skill
2. Tool
3. Extension
4. MCP
5. UI rendering and security boundary

The agent loop remains tool-agnostic.

### D-002. First clients

The first release must support:

- CLI/TUI.
- The Talkify desktop client in `/Users/xiaoyuan/Documents/work/git/chater`.

Both clients consume the same preview artifact and capability contract. They may
use different rendering surfaces, but must implement the same security policy
and degraded behavior.

### D-003. Interactive HTML is v1

Interactive HTML preview is required in the first release. Static screenshots
remain mandatory as evidence and fallback, but are not a substitute for the
interactive path.

### D-004. Initial verification toolchains

V1 verification support is limited to:

- Go.
- DreamLog/iOS.

Additional languages and frameworks are post-v1 extensions.

### D-005. Reference designs are inputs, not dependencies

The Codex implementation and the DreamLog Duo trajectory are reference
baselines:

- Codex supplies architectural patterns.
- DreamLog supplies a real behavior and tool-use fixture.
- Neither may become a hard-coded runtime dependency or implicit capability
  activation rule.

### D-006. Generic preview GUI belongs in AgentKit

AgentKit owns the generic Agent GUI surface for Talkify. Generic interactive
preview, asset preview, timeline presentation, navigation policy, and related
client capability negotiation belong in AgentKit, not in a Talkify-specific
`TimelineExtension`.

Talkify may keep product-specific extensions for domain-specific visualization,
such as Desktop Control evidence. It must not duplicate the generic interactive
HTML host.

### D-007. Deferred Tool Discovery remains post-v1

V1 uses direct, hidden, and client tool exposure. Deferred tool discovery,
BM25 tool search, and Code Mode remain post-v1.

### D-008. Interactive preview is first-class in ConversationWorkbench

AgentKit must provide a first-class generic preview module that appears directly
inside the macOS ConversationWorkbench. Preview is not limited to the Inspector
and is not implemented as a tool-specific Talkify Timeline extension.

The preview may use an isolated child WebView or sandboxed iframe, but its
presentation, artifact loading, selection, expansion, replay, and degraded state
are owned by AgentKit.

### D-009. Backend and AgentKit are separate implementation tracks

Implementation is split into:

1. code-agent runtime/backend track.
2. AgentKit GUI track.

Talkify remains a thin integration consumer. The review role is independent of
both implementation tracks, and cross-repository acceptance is gated on shared
contracts and evidence.

### D-010. Preview Contract v1.6 is frozen

Both code-agent backend and AgentKit returned R0 GO on v1.6. The contract and
fixture manifest are frozen in [R0_FREEZE.md](./R0_FREEZE.md). Backend R1 and
AgentKit A1 may start in parallel after the frozen baseline is committed.

## Pending Engineering Approval

### D-101. Manifest scope

Recommendation: capability manifests are project-local only in v1. A remote
marketplace and signed distribution remain post-v1.

### D-103. Host contract

Approve one shared preview manifest and one security policy implemented by:

- CLI/TUI: trusted materialized viewer plus system-browser or local preview host.
- Talkify desktop: isolated embedded WebKit preview surface.

### D-104. Implementation plan

Approve milestones, interface boundaries, test strategy, and release gates
before production code changes.
