# R0 Freeze Record

Status: frozen
Date: 2026-11-09
Contract ID: `preview_contract_v1`
Contract version: v1.6

## Frozen Baseline

- Contract: [PREVIEW_CONTRACT_V1_6.md](./PREVIEW_CONTRACT_V1_6.md)
- Fixtures: [fixtures/preview/v1/manifest.json](./fixtures/preview/v1/manifest.json)
- Contract SHA-256: `e61de0a8de24749f14d63e57c1211f161700fd5ad0dd9bc18a4c5fb453a221f3`
- Fixture manifest SHA-256: `f06b5bccfc736e8a620fac9994de2edbf5941900944f3cb51d7ae5ebba9d1caa`

## Review Result

- code-agent backend R0: GO on v1.6.
- AgentKit GUI R0: GO on v1.6.
- No remaining R0 blocking issues.
- Backend R1 and AgentKit A1 are approved to start in parallel.

## Implementation Rules

- R1/A1 must use the frozen contract and fixture bytes as the shared baseline.
- Any contract change requires a new versioned contract, updated fixtures,
  regenerated digests, and a new R0 review.
- Implementation must not add tool-name-specific preview branches.
- Talkify remains a thin integration consumer through AgentKit.
- Preview-to-runtime actions, deferred tool discovery, and Code Mode remain
  post-v1.

## Non-blocking Implementation Tests

- Pending revision N to ready revision N+1 event sequence.
- Ready interactive without required fallback maps to `unavailable` corruption
  state.
- iframe navigation and main-frame message rejection.
- CLI/TUI in-process resolver byte consistency.
- Runtime flex executor reports the resolved `tool_started.executor`.

## Git Baseline

The documentation and fixture baseline is committed in:

```text
84d9eac733205cc53686f9c6914216732010574d
docs: freeze capability runtime R0 baseline
```

The unrelated existing modification to `internal/app/registry.go` is intentionally
excluded from this baseline.
