# Runtime Workspace Git Branch Management v1

The Runtime advertises `workspace_git_branch_v1` in both
`GET /v1/runtime/capabilities` (`capabilities.workspace_git_branch_v1`) and the
WebSocket hello capability list. AgentKit must hide branch controls when the
capability is absent.

All endpoints use the existing `{trace_id, code, msg, data}` Runtime envelope.
`data` on success is the payload below; `data` on failure is an object with a
stable `code` and human-readable `message`.

## Endpoints

```http
POST /v1/workspaces/git/branches/list
Content-Type: application/json

{"workspace_path":"/repo/AgentKit"}
```

The response data is:

```json
{
  "workspace_path": "/repo/AgentKit",
  "checkout": {
    "workspace_path": "/repo/AgentKit",
    "is_git_repository": true,
    "head": {"kind":"branch", "name":"main", "commit":"abc123"},
    "is_dirty": false,
    "modified_files": 0,
    "untracked_files": 0,
    "active_worktree": false
  },
  "branches": [{
    "name":"main", "commit":"abc123", "is_current":true,
    "is_checked_out_elsewhere":false, "worktree_path":null
  }]
}
```

Create a local branch with `POST /v1/workspaces/git/branches/create`:

```json
{
  "workspace_path":"/repo/AgentKit",
  "name":"feature/branch-ui",
  "start_point":null,
  "checkout":true,
  "client_request_id":"create-branch-42"
}
```

`start_point` may be a local branch or commit. It defaults to the current
HEAD. `checkout:false` creates the branch without changing HEAD and is allowed
on a dirty workspace. A successful response always contains a fresh complete
branch list and checkout state.

Checkout uses `POST /v1/workspaces/git/branches/checkout`:

```json
{"workspace_path":"/repo/AgentKit", "name":"main", "allow_dirty":false}
```

v1 always rejects dirty or conflict-state checkout, including when
`allow_dirty:true` is supplied. Runtime never stashes, resets, cleans, commits,
or discards files.

## Safety and errors

The Runtime canonicalizes the path, requires an existing directory, and only
accepts workspaces below the Runtime-authorized workspace root. `.git` paths and
external worktree paths are not accepted as substitutes. Managed worktrees are
rejected with `workspace_git_managed_worktree` and include
`base_workspace_path`.

Branch mutations are serialized in the Runtime. An active turn using the same
workspace returns `workspace_git_busy`; the operation does not bypass the
conversation/workspace lease. Git merge, rebase, cherry-pick, or revert
conflict state returns `workspace_git_conflict_state`.

The stable error codes are:

`workspace_not_found`, `workspace_not_authorized`, `workspace_not_git_repository`,
`workspace_git_unsupported`, `workspace_git_invalid_ref`,
`workspace_git_branch_exists`, `workspace_git_branch_not_found`,
`workspace_git_dirty`, `workspace_git_conflict_state`, `workspace_git_busy`,
`workspace_git_session_conflict`, `workspace_git_managed_worktree`,
`workspace_git_checkout_failed`, and `workspace_git_create_failed`.

`client_request_id` makes create retries idempotent for the lifetime of the
Runtime process. Concurrent creates are serialized, so only one same-name
branch can succeed; Git remains the final conflict authority.

AgentKit client models need `CheckoutState`, `Branch`, and the result envelope
fields above. Branch selection must use `is_current`, not list ordering.
