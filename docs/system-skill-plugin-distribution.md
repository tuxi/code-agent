# System Skill and Plugin Distribution Contract

Status: implemented foundation. This document defines the CodeAgent distribution
boundary shared by `codeagent`, `codeagentd`, AgentKit/Talkify's embedded runtime,
and future skill management surfaces.

The P1-A backend management contract is defined in
[skill-plugin-management-api.md](skill-plugin-management-api.md). It covers
skill/plugin inventory, local plugin lifecycle, bearer-auth requirements,
workspace-scoped project skills, integrity failures, and reload semantics.

## Scopes and paths

| Scope | Path | Conflict behavior |
| --- | --- | --- |
| system | `<data-root>/skills/.system` | Lowest ordinary precedence; replaced only as a managed bundle |
| user | `<data-root>/skills` | Overrides system |
| project | `<workspace>/skills`, then `<workspace>/.agents/skills` | Highest ordinary precedence; `.agents/skills` wins if both define the same name |
| plugin | `~/.codeagent/plugins/cache/<plugin>/<version>/...` | Always exposed as `<plugin>:<skill>`; cannot override an ordinary skill |

Desktop uses `~/.codeagent` as `<data-root>`. Embedded hosts use their writable
application data directory. The user skill directory remains compatible with the
legacy `Load(globalDir, projectDir)` API. System content is never flattened into
the user directory.

Loaded skills retain `name`, `scope`, `source`, `path`, skill version,
`SHA-256(SKILL.md)`, `enabled`, resources, and parse/shadow errors.

## System bundle

Canonical manifest: `skills/system-skills.json`.

Schema: `schemas/system_skills.v1.schema.json`.

The manifest declares bundle version, name, source path, destination, scope,
aggregate hash, dependencies/references, and per-file hashes. Validation rejects
duplicate names, missing `SKILL.md`, frontmatter/source mismatches, stale hashes,
undeclared skill directories, unsafe paths, and symlinks.

The initial system set is intentionally cross-runtime: `review-change`,
`verify-change`, `skill-creator`, and `ui-adaptation`. Repository-specific seed
skills remain project skills rather than being installed globally.

### Legacy flattened bundle migration

`skills/legacy-system-skills.json` records the exact historical flattened
bundle variants for `review-change`, `verify-change`, and `skill-creator`.
Both the AgentKit initial bundle and code-agent `build-ios.sh` bundle variants
are represented.

At startup, after `.system` is current, the installer checks those three names
directly under the user skill root:

- exact file set + hashes match a known legacy variant: remove the flattened
  bundle-owned copy;
- any extra/missing file, changed hash, symlink, or non-regular file: preserve it
  and report a shadow warning because the user copy outranks `.system`;
- missing legacy directory: no action.

The legacy manifest itself is digest-protected by the installer marker.

Generate the deterministic embedded bundle and sync AgentKit resources:

```bash
go run ./cmd/system-skills --write \
  --agentkit-resources ../AgentKit/Sources/AgentKit/Resources/skills/.system
```

Validate both repositories without writing:

```bash
go run ./cmd/system-skills --check \
  --agentkit-resources ../AgentKit/Sources/AgentKit/Resources/skills/.system
```

Build scripts run `--check`. `codeagentd`, `codeagent`, and the Go embedded runtime
compile the generated bundle with `go:embed`; AgentKit copies the same generated
lock into its SwiftPM resources.

### Installer guarantees

- First launch materializes `<data-root>/skills/.system`.
- A JSON marker records schema, bundle version, fingerprint, installation source,
  and installed file hashes.
- A matching marker plus matching installed hashes skips all writes.
- A different bundle atomically replaces only `.system`; user siblings are never
  touched.
- A broken marker, missing file, or modified file triggers repair.
- Failed activation restores the previous `.system`; stale stage/backup state is
  recovered on the next launch.

## Plugin foundation

Manifest: `.codeagent-plugin/plugin.json` (with inert compatibility reads for
`plugin.json` and `.claude-plugin/plugin.json`).

Schema: `schemas/codeagent_plugin.v1.schema.json`.

The manifest supports name, version, description, skills, MCP servers, apps,
hooks, and interface/display metadata. Phase 1 installs local directories or zip
archives only:

```bash
codeagent plugin install /path/to/plugin --yes
codeagent plugin disable docs
codeagent plugin enable docs
codeagent plugin remove docs
```

External installation requires explicit confirmation. The cache is immutable and
versioned at `~/.codeagent/plugins/cache/<name>/<version>`. Installed metadata
records manifest and bundle SHA-256 digests; reads verify both before exposing a
capability. Reusing a version with different content fails instead of overwriting.

Enabled plugin skills are namespaced as `<plugin>:<skill>`. Disabled plugins
contribute neither skills nor MCP servers. Plugin MCP server names are
`<plugin>__<server>`, and they enter the existing MCP manager at the lowest layer,
so normal tool permission, approval, sandbox, and audit paths still apply.
Plugin enable/disable changes are persisted immediately and take effect when the
runtime process reloads its plugin registry; live plugin hot-reload is a P1 item.

## AgentKit

AgentKit resources live at `Sources/AgentKit/Resources/skills/.system`. On launch,
`BundledSkillsInstaller` verifies the manifest, hashes, paths, and symlinks, then
installs into `<data-directory>/skills/.system`. The same marker/fingerprint and
preservation rules apply. The Go embedded runtime also carries the generated
bundle, so a standalone app runtime is not dependent on a neighboring manual
skills directory.

## Deliberately out of scope

- Talkify skill management UI and marketplace UI.
- Automatic remote plugin download or unpinned marketplace installation.
- Executing plugin `hooks` or `apps`; they are preserved and digested but not run.
- Plugin MCP UI/approval redesign. Plugin MCP uses the existing manager and policy
  path rather than a privileged side channel.
- Already-flattened legacy system skill migration is supported for registered
  bundle variants.
