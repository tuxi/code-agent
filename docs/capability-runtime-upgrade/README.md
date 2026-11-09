# CodeAgent Capability Runtime Upgrade

Status: R0 frozen; R1/A1 approved
Decision gate: commit the frozen baseline before implementation starts.

This directory defines the large upgrade that turns code-agent from a
hard-coded coding tool collection into a composable capability runtime.

## Documents

- [GOALS.md](./GOALS.md): outcomes, success metrics, non-goals, and approval
  criteria.
- [PRD.md](./PRD.md): functional requirements, architecture boundaries,
  milestones, security requirements, and acceptance criteria.
- [DECISIONS.md](./DECISIONS.md): accepted scope decisions and remaining
  engineering decisions.
- [REFERENCES.md](./REFERENCES.md): Codex source and DreamLog trajectory
  baselines.
- [AGENTKIT_INTEGRATION.md](./AGENTKIT_INTEGRATION.md): ownership boundary between
  code-agent runtime, AgentKit, and Talkify.
- [DELIVERY_PLAN.md](./DELIVERY_PLAN.md): two-track implementation ownership,
  contract freeze, review gates, and release criteria.
- [R0_FREEZE.md](./R0_FREEZE.md): frozen contract/fixture baseline, review
  result, and implementation gate.
- [PREVIEW_CONTRACT_V1_6.md](./PREVIEW_CONTRACT_V1_6.md): canonical R0 preview
  artifact, producer, event, capability, replay, and security contract.
- [PREVIEW_CONTRACT_V1_5.md](./PREVIEW_CONTRACT_V1_5.md): penultimate review draft,
  retained for review traceability.
- [PREVIEW_CONTRACT_V1_4.md](./PREVIEW_CONTRACT_V1_4.md): final-review draft,
  retained for review traceability.
- [PREVIEW_CONTRACT_V1_3.md](./PREVIEW_CONTRACT_V1_3.md): fourth-round draft,
  retained for review traceability.
- [PREVIEW_CONTRACT_V1_2.md](./PREVIEW_CONTRACT_V1_2.md): third-round draft,
  retained for review traceability.
- [PREVIEW_CONTRACT_V1_1.md](./PREVIEW_CONTRACT_V1_1.md): second-round draft,
  retained for review traceability.
- [PREVIEW_CONTRACT.md](./PREVIEW_CONTRACT.md): historical v1 draft, retained for
  review traceability.
- [UI Adaptation Capability Design](../ui-adaptation-capability.md): the first
  end-to-end validation capability for this upgrade.

## Decision

The upgrade is organized around four capability layers plus a client-side
rendering boundary:

1. Plugin/Skill
2. Tool
3. Extension
4. MCP
5. UI rendering and security boundary

The Agent Loop remains tool-agnostic. Capabilities are composed through explicit
manifests and runtime registration, not through new design-specific branches.

## Current Gate

Preview Contract v1.6 and its fixture manifest are frozen. Backend R1 and
AgentKit A1 may start in parallel after the frozen baseline is committed.
