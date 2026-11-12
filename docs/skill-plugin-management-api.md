# Skill and Plugin Management API v1

Status: P1-A backend implemented. This contract is the stable boundary for
AgentKit/Talkify management surfaces. It does not define UI behavior, remote
marketplaces, hooks/apps execution, or plugin hot reload.

All endpoints use the existing Runtime bearer authentication and unified
response envelope:

```json
{
  "trace_id": "request trace id",
  "code": 0,
  "msg": "success",
  "data": {}
}
```

Management routes fail closed with HTTP 401 unless Runtime bearer authentication
is enabled. The same rule applies to loopback and non-loopback listeners.

## Skill inventory

`GET /v1/runtime/skills?session_id=<conversation-id>`

Project scope is resolved exclusively from the conversation's persisted
`WorkspacePath`. Arbitrary `workspace_path` query parameters are rejected with
`workspace_scope_unsupported`; callers cannot enumerate another workspace by
supplying a path. Omitting `session_id` returns system/user/plugin scopes without
project skills.

Response schema: `runtime-skill-inventory/v1`.

```json
{
  "schema": "runtime-skill-inventory/v1",
  "scope": {
    "session_id": "sess_123",
    "workspace_id": "ws_ab12cd34ef56",
    "display_name": "project",
    "resolved": true
  },
  "items": [
    {
      "name": "verify-change",
      "display_name": "Verify Change",
      "description": "Verify a change before delivery.",
      "scope": "system",
      "source": "system",
      "source_group": "system",
      "path": "skills/.system/verify-change/SKILL.md",
      "version": "1",
      "sha256": "64 lowercase hex characters",
      "enabled": true,
      "status": "active",
      "plugin_name": "docs",
      "interface": {
        "icon": "shield-check"
      },
      "resources": {
        "count": 1,
        "names": ["references/checklist.md"]
      },
      "warnings": [],
      "parse_errors": [],
      "shadowed_by": {
        "name": "verify-change",
        "scope": "project",
        "source": "project",
        "path": ".agents/skills/verify-change/SKILL.md"
      },
      "shadows": [
        {
          "name": "verify-change",
          "scope": "user",
          "source": "user",
          "path": "skills/verify-change/SKILL.md"
        }
      ]
    }
  ],
  "warnings": [],
  "reload_required": false
}
```

`status` is `active`, `shadowed`, or `invalid`. `enabled` is true only for the
effective active candidate. Invalid entries retain parse diagnostics; shadowed
entries retain `shadowed_by`, while active winners list all lower-precedence
candidates in `shadows`.

`description` and `display_name` are parsed from `SKILL.md`, so management
clients do not need to read skill files. `display_name` falls back to the skill
name. `source_group` is one of `system`, `user`, `project`, or `plugin`; the exact
plugin identity remains in `plugin_name`. `interface.icon` is optional and is
omitted when absent or unsafe.

Paths are workspace-relative for project skills and data-root-relative for
system/user/plugin content. Absolute internal paths, file bodies, command lines,
environment values, and credentials are never returned. If a safe relative path
cannot be derived, the API returns an opaque path identifier.

Disabled plugins are absent from active skill inventory. Plugin skills use the
stable `<plugin>:<skill>` namespace. A tampered plugin is skipped independently:
healthy roots remain visible and top-level `warnings` reports
`plugin_integrity_error`. Only an unreadable registry fails the whole request.

## Plugin inventory

`GET /v1/runtime/plugins`

Response schema: `runtime-plugin-inventory/v1`.

```json
{
  "schema": "runtime-plugin-inventory/v1",
  "items": [
    {
      "name": "docs",
      "version": "1.0.0",
      "description": "Docs plugin",
      "display_name": "Docs Center",
      "category": "documentation",
      "capabilities": ["search", "summarize"],
      "icon": "https://cdn.example.com/icon.svg",
      "logo": "assets/logo.png",
      "brand_color": "#112233",
      "website_url": "https://example.com/docs",
      "enabled": true,
      "source": "local",
      "source_kind": "local_directory",
      "cache_id": "plg_ab12cd34ef56",
      "cache_path": "plugins/cache/docs/1.0.0",
      "manifest_sha256": "64 lowercase hex characters",
      "bundle_sha256": "64 lowercase hex characters",
      "installed_skills": [
        {
          "name": "docs:search",
          "path": "plugins/cache/docs/1.0.0/skills/search/SKILL.md",
          "valid": true
        }
      ],
      "mcp_servers": ["docs__lookup"],
      "has_apps": true,
      "has_hooks": false,
      "integrity_status": "verified",
      "reload_required": false
    },
    {
      "name": "broken",
      "version": "1.0.0",
      "description": "Broken plugin",
      "enabled": true,
      "source": "local",
      "source_kind": "local_directory",
      "cache_id": "plg_broken",
      "cache_path": "plugins/cache/broken/1.0.0",
      "manifest_sha256": "64 lowercase hex characters",
      "bundle_sha256": "64 lowercase hex characters",
      "installed_skills": [],
      "mcp_servers": [],
      "has_apps": false,
      "has_hooks": false,
      "integrity_status": "failed",
      "error_code": "plugin_integrity_error",
      "reload_required": false
    }
  ],
  "reload_required": false
}
```

Plugin interface metadata is projected from the manifest's allowlisted
`interface` fields. Unknown fields are discarded, and unsafe URL schemes,
absolute asset paths, path traversal, non-http(s) website URLs, and invalid
color values are omitted. Items are sorted by plugin name.

The manifest and bundle digests are verified per plugin. One tampered plugin does
not fail the inventory: it is returned with `integrity_status: "failed"` and a
stable `error_code`, while healthy plugins remain listed. Only an unreadable or
malformed installed-plugin registry fails the whole request.

`mcp_servers` contains names only; server configuration, commands, environment
values, and credentials are never serialized.

## Plugin lifecycle

All mutations return `reload_required` and, when true, `reload_hint`. Hot reload
is not implemented in P1-A; `reload_required: true` means the caller must restart
the runtime before the active skill/MCP registry reflects the persisted change.
`false` means the persisted plugin state matches the process-start snapshot used
by this manager (including no-op enable/disable or install-then-uninstall before
restart).

### Install local

`POST /v1/runtime/plugins/install-local`

```json
{
  "source": "/absolute/path/to/plugin-or-plugin.zip",
  "confirmed": true
}
```

`confirmed: true` is mandatory. Only an absolute local directory or `.zip` is
accepted. URL schemes, relative paths, source symlinks, and sources inside managed
skills/plugin-cache roots are rejected. Installation writes only to the immutable
versioned plugin cache and the installed-plugin registry; it never writes into
the system bundle or user skill siblings.

Success is HTTP 201 with `runtime-plugin-mutation/v1`:

```json
{
  "schema": "runtime-plugin-mutation/v1",
  "action": "installed",
  "plugin": {},
  "reload_required": true,
  "reload_hint": "Restart the CodeAgent runtime to apply plugin changes."
}
```

### Enable / disable

- `POST /v1/runtime/plugins/{name}/enable`
- `POST /v1/runtime/plugins/{name}/disable`

The response contains the updated plugin item and reload semantics. Disabled
plugins contribute neither namespaced skills nor MCP servers. Mutation endpoints
remain fail-closed: enable/disable/install/uninstall reject a tampered plugin.

### Uninstall

`DELETE /v1/runtime/plugins/{name}`

```json
{
  "schema": "runtime-plugin-mutation/v1",
  "action": "uninstalled",
  "name": "docs",
  "version": "1.0.0",
  "reload_required": true,
  "reload_hint": "Restart the CodeAgent runtime to apply plugin changes."
}
```

The cache path must resolve inside the plugin cache root. Symlink escapes and
paths outside that root are refused. Registry state is updated after cache
cleanup.

## Error codes

Errors remain in the unified envelope. `data.code` is the stable machine code:

| HTTP | `data.code` | Meaning |
| --- | --- | --- |
| 401 | `runtime_auth_required` | Bearer authentication is required. |
| 401 | `runtime_auth_invalid` | Bearer token is invalid. |
| 400 | `workspace_scope_unsupported` | Caller supplied an arbitrary workspace path. |
| 400 | `plugin_source_invalid` | Local source is missing, relative, or symlinked. |
| 400 | `plugin_source_unsupported` | URL or non-directory/non-zip source. |
| 404 | `conversation_not_found` | Session workspace cannot be resolved. |
| 404 | `plugin_not_found` | Plugin is not installed. |
| 409 | `workspace_scope_unresolved` | Session exists but requires workspace rebind. |
| 409 | `plugin_integrity_error` | A mutation was rejected because manifest/bundle or cache integrity failed. |
| 422 | `plugin_manifest_invalid` | Local plugin manifest is malformed or invalid. |
| 428 | `plugin_confirmation_required` | `confirmed: true` was not supplied. |
| 500 | `skill_inventory_failed` | Skill roots could not be read. |
| 500 | `plugin_registry_invalid` | Installed-plugin registry could not be read. |
| 500 | `plugin_operation_failed` | Local plugin lifecycle operation failed. |

Response fixtures live in `internal/server/testdata/`.

## Capability

When the service is wired, Runtime advertises both:

- REST/WebSocket feature: `skill_plugin_management_v1`
- `/v1/runtime/capabilities` field: `skill_plugin_management_v1: true`
