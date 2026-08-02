# P14 Acceptance Report — v2 Generator-Critic

> Status: verified. Three systematic defects discovered and fixed across
> 9 test iterations (test1–test9). The v2 pipeline is now production-grade.

## Test Timeline

| Test | Result | Root cause |
|---|---|---|
| test1–5 | workflow stuck, wait_review correlation null | Expression syntax error: `input.agent.correlation_id + '/review'` and `'request'` literal not supported (`df00dc8`) |
| test6 | dispatch_review blocked 44s, then succeeded via retry | `create_session` didn't claim ownership → heartbeat deleted cross-workspace lease → offline window (`512a84d` + `8d3beea`) |
| test7 | same symptoms as test6 | Same lease window, plus model override `deepseek-v4-flash` unresolvable on target (`d2291cd`) |
| test8 | first attempt offline (44s) → retry success. Error NOW visible in node error column | `f7b974c` made errors visible; previously swallowed as `success + {}` |
| test9 | **5s, one pass, no errors, no retries** | All fixes combined close the window |

## Defects Found and Fixed

### 1. Error propagation (critical)
**File**: `internal/runtime/flux_tool.go:746`
**Before**: `return fluxtool.Fail(err), nil` — Go error was nil, flux engine saw success, node output was `{}`
**After**: `return nil, err` — Go error propagated, node marked failed, error persisted
**Impact**: All flux tool node failures were invisible. `send_to_session` timeout, model unavailable, owner offline — all swallowed.

### 2. Cross-workspace lease deletion (root cause of 44s delay)
**File**: `internal/controlplane/owner.go:384`
**Before**: `DELETE FROM session_owners WHERE instance_id=? AND session_id NOT IN (seen)` — deleted valid leases for sessions in other workspaces
**After**: Added `AND expires_at_ms <= ?` — only delete genuinely expired leases
**Impact**: Every heartbeat cycle (5s) deleted cross-workspace session leases, creating a recurring offline window.

### 3. Spawned session ownership
**File**: `internal/controlplane/spawn.go:60-72, 116-130`
**Before**: `create_session`/`fork_session` relied on next heartbeat to claim ownership
**After**: Synchronously write `session_owners` at creation time
**Impact**: New sessions were offline for up to 5s until heartbeat picked them up.

### 4. Model fallback for cross-session turns
**File**: `internal/runtime/serve_builder.go:176-181`
**Before**: `resolveTurnModel` returned hard error for unresolvable model overrides on direct providers
**After**: Fall back to server default model
**Impact**: Cross-session turns passing `model="deepseek-v4-flash"` to a target with a different model catalog failed.

### 5. Unsupported expression syntax
**File**: `internal/runtime/flux_cross_workspace.go` (v2 template)
**Before**: `"input.agent.correlation_id + '/review'"` and `"'request'"` — string concat and literal not supported
**After**: `"input.agent.correlation_id"` and `"input.agent.intent"`
**Impact**: `dispatch_review` tool input failed to resolve, producing empty output.

## Verified Pipeline

```
v2 child workflow per agent:
  start → dispatch (send_to_session)
       → wait_impl (NodeAwait: external_task + check_turn)
       → read_impl (read_session)
       → dispatch_review (send_to_session)
       → wait_review (NodeAwait: external_task + check_turn)
       → read_review (read_session)
       → end

Parent: Map(parallel=N) → fan-in → end
```

All 7 nodes verified across 3 full runs (test7–test9). Both implementer and reviewer turns complete, VERDICT: PASS carried into Map fan-in output. Session ownership, model resolution, error propagation, and expression syntax all verified.
