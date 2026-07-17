# Public Git Clone v1

Status: implemented by the Go Runtime. This contract supersedes
`docs/ios_github_clone_spec.md`.

## Capability

`GET /v1/runtime/capabilities` advertises the endpoint only when its projects
root and durable idempotency store initialized successfully:

```json
{
  "capabilities": { "public_git_clone_v1": true },
  "projects_root": "/Users/me/Documents"
}
```

The WebSocket hello capability list also contains `public_git_clone_v1`.
Daemon mode uses `~/Documents`; embedded mode uses the host-provided App
Documents directory. `codeagent serve` retains its current-directory root.

## Clone request

`POST /v1/repos/clone` is synchronous and has a Runtime timeout of five minutes.

```json
{
  "request_id": "ff1d7605-38aa-4fd7-90d8-0e46376ae07c",
  "url": "https://gitlab.com/team/repo.git",
  "ref": "main",
  "name": "repo",
  "depth": 1
}
```

- `request_id` is required and at most 128 characters. Automatic retries reuse
  it. A user-initiated retry after a terminal failure uses a new id.
- `url` accepts any public HTTPS Git endpoint without credentials. The legacy
  GitHub `owner/repo` shorthand remains accepted.
- `name` is optional and otherwise derived from the last URL path component.
- `ref` is an optional branch or tag. A short name checks branches before tags.
- omitted `depth` means `1`; zero means full history; positive values request a
  shallow clone; negative values are invalid.

Success is HTTP 201 in the standard API envelope:

```json
{
  "request_id": "ff1d7605-38aa-4fd7-90d8-0e46376ae07c",
  "workspace_path": "/Users/me/Documents/repo1",
  "workspace_ref": { "root": "workspace", "rel": "repo1" }
}
```

Clone creates only the project. It does not create a Conversation or managed
Worktree. Git submodule recursion and Git LFS materialization are outside v1.

## Idempotency and publication

Request state is durable in Runtime data storage. Repeating the same request id
and payload returns the recorded success or error, including after Runtime
restart. Concurrent duplicates join the same operation. Reusing an id with a
different payload returns `destination_conflict`.

Clone runs in a hidden temporary directory on the projects-root filesystem and
publishes with an OS no-replace rename. Existing files, empty directories,
non-empty directories, and symlinks are never overwritten. Conflicts allocate
`repo`, `repo1`, `repo2`, and so on. Failed, cancelled, and interrupted requests
clean their temporary directory.

## Network boundary

Only unauthenticated HTTPS is supported. URL userinfo, query parameters,
fragments, SSH, HTTP, file, and git protocols are rejected. DNS resolution,
actual dialing, and every redirect reject loopback, private, link-local, shared,
reserved, and cloud-metadata addresses. The public-clone transport is isolated
from the normal go-git HTTPS transport used by project tools.

## Errors

| Code | HTTP | Meaning |
|---|---:|---|
| `invalid_url` | 400 | Unsupported or credential-bearing URL |
| `invalid_name` | 400 | Unsafe destination name |
| `invalid_request` | 400 | Invalid request id or depth |
| `repo_not_found` | 404 | Missing or non-public repository |
| `ref_not_found` | 404 | Missing branch or tag |
| `destination_conflict` | 409 | Idempotency payload conflict or name exhaustion |
| `cancelled` | 408 | Request was cancelled |
| `network_error` | 502 | DNS, TLS, timeout, or transport failure |
| `io_error` | 500 | Local persistence or filesystem failure |
