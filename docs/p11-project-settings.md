# P11 — Project settings layer (`.codeagent/settings.json`)

> Status: **✅ fully shipped (P11.a–d).** Aligns with Claude Code's
> `.claude/settings.json` + `settings.local.json` model. See §12 for what each
> phase landed.

## 1. Background

Two facts forced this:

1. **`config.yaml` is being used as a dumping ground for project *behavior*.**
   It should be **infrastructure**: models, API keys, endpoints, compaction,
   parallelism — machine/user-level knobs. But it currently also holds
   project-scoped *behavior*: `permissions.allow/deny` ([config.go](../internal/app/config.go)
   `PermissionsConfig`), `hooks` (`cfg.Hooks`), and — newly, from P4.3-R Move 2 —
   `agent.verify_command`. These are per-project decisions and do not belong in
   the same file as your API key.

2. **A single global `verify_command` cannot be right across projects.** Go wants
   `go test ./...`, Rust `cargo test`, Swift `swift build`, Node `npm test`. The
   command is inherently per-project, so its home must be per-project too.

**The Claude Code mirror.** Claude Code has **no** `verify_command` knob at all —
verification is model behavior (system prompt + `CLAUDE.md`) plus an *optional*
user-authored `Stop` hook. What it *does* have is a clean **settings layer**:
`.claude/settings.json` (shared, committed), `.claude/settings.local.json`
(personal, git-ignored), `~/.claude/settings.json` (user-global), holding
`permissions`, `hooks`, `env`, `model`, … We should adopt that layer.

> **Not a regression of Move 2.** Pure model-behavior verification is exactly what
> failed in the SwiftUI paper-over case — so our *deterministic* finalize verify
> is a justified addition over Claude Code's default, not a deviation to undo.
> P11 does not remove it; it **relocates** it from `config.yaml` into the settings
> layer and makes it per-project, auto-detectable, and agent-writable.

## 2. What already exists (do NOT rebuild)

Half of this is already shipped for permissions — the design **extends** it:

- [internal/approve/rules.go](../internal/approve/rules.go) already:
  - reads `<root>/.codeagent/settings.local.json` and `~/.codeagent/settings.json`
    as a **Claude-style `settings.json`** (`{"permissions":{"allow":[],"deny":[]}}`);
  - loads best-effort (missing → skip; malformed → log + ignore, never brick);
  - **persists grants atomically (temp + rename) while preserving unknown keys**
    (it merges into a generic `map[string]any`), so new blocks we add later are
    never clobbered by a permission grant;
  - defines `Scope` (`ScopeProjectLocal` → `settings.local.json`, `ScopeUser` →
    `~/.codeagent/settings.json`), mirroring Claude's scopes.
- [internal/mcp/config.go](../internal/mcp/config.go) already layers a **project
  shared** `.mcp.json` with a **project local** override (`LoadProject` +
  `LoadLocal`, local takes precedence) — the exact shared/local precedent P11
  generalizes.

## 3. Goal

One coherent **project settings layer** — Claude-compatible on disk — that owns
project-scoped *behavior* (`permissions`, `verify`, `hooks`), leaving `config.yaml`
for *infrastructure*. Read by a single loader, merged by a defined precedence, and
**writable by the agent itself** so common projects need zero manual setup.

## 4. Non-goals / guardrail

- ❌ **Secrets never live here.** API keys stay in `config.yaml` / env / host
  keychain (iOS). `settings.json` is committable; a leaked key must be impossible
  by construction. The loader ignores any `apiKey`-shaped field.
- ❌ Not a new permission engine, hook engine, or verify engine — those exist.
  P11 is a **loader + schema + precedence + migration**, feeding the existing
  `RuleStore`, `hooks.Runner`, and `Runner.VerifyCommand`.
- ❌ No breaking change: `config.yaml`'s `permissions` / `hooks` keep working as
  the lowest-priority layer (§7 back-compat).
- ❌ The agent may write `verify`/`permissions` grants but **never** edits
  `config.yaml` and never writes secrets.

## 5. The layer model

Four layers, lowest → highest priority:

| # | Source | Scope | Committed? | Owns |
|---|--------|-------|-----------|------|
| 0 | `config.yaml` | machine/user | n/a | **infra** (models, keys, endpoints) + legacy `permissions`/`hooks` (back-compat) |
| 1 | `~/.codeagent/settings.json` | user (all projects) | no (home) | your personal defaults across projects |
| 2 | `<root>/.codeagent/settings.json` | project, shared | **yes** (commit) | team-shared project behavior |
| 3 | `<root>/.codeagent/settings.local.json` | project, this machine | **no** (gitignore) | personal + agent-written overrides |

This mirrors Claude Code (local project > shared project > user), with
`config.yaml` slotted underneath as the infra + legacy layer.

**Per-block merge semantics** (different blocks merge differently — define each):

- `permissions` — **union** for `allow`/`deny` across all layers (deny always
  wins over allow). This is already how `RuleStore` behaves; P11 just adds
  layer 2 to the files it unions.
- `verify` — **override**: a scalar; the highest layer that sets it wins
  (3 > 2 > 1 > 0-legacy). Absent everywhere ⇒ `auto` (§8).
- `hooks` — **concatenate**: hooks from all layers all run (each is an
  independent command); ordering is layer 0→3 then file order.

## 6. Schema (`settings.json`, Claude-compatible)

```jsonc
{
  // Already read today (permissions-only). Extended, not changed.
  "permissions": {
    "allow": ["run_command", "mcp__github__*"],
    "deny":  ["run_command(rm *)"]
  },

  // NEW — the P4.3-R Move 2 finalize verify, relocated here.
  "verify": {
    // "auto" (default when the block/field is absent) → detect from the
    // workspace (§8). A literal string is used verbatim. "" / false disables.
    "command": "go test ./...",
    "enabled": true
  },

  // Migrated from config.yaml `hooks:`. Same shape as internal/hooks.Hook.
  "hooks": [
    { "event": "post_tool_use", "match": "edit_file", "command": "gofmt -w \"$CODEAGENT_FILE\"" }
  ]
}
```

`settings.local.json` has the identical schema; it just overrides/extends.
Unknown top-level keys are preserved on write (already true — §2) and ignored on
read, so a file authored for a newer version never breaks an older binary.

## 7. Migration & back-compat

- **`verify`**: `agent.verify_command` in `config.yaml` becomes the **layer-0
  legacy** value, overridden by any `verify` in the settings layer. New guidance:
  put it in `settings.local.json` (or let the agent auto-write it, §9). The
  `config.example.yaml` note points here. (Do not delete the field yet — it is the
  documented back-compat path and how embedded/iOS hosts inject it, §10.)
- **`permissions`**: `config.yaml` `permissions` stays as layer 0, unioned with
  the files exactly as `NewRuleStore` already does. No change for existing users.
- **`hooks`**: `config.yaml` `hooks` stays as layer 0; settings-layer hooks
  concatenate on top.
- Net: **every existing `config.yaml` keeps working unchanged**; the settings
  layer is purely additive and higher-priority.

## 8. `verify: auto` — detection

When `verify.command` is `auto`/absent, detect from the workspace root (first
match wins), preferring a **cheap** check so auto-running at every finish is safe:

| Marker | Auto command |
|--------|--------------|
| `go.mod` | `go build ./...` |
| `Cargo.toml` | `cargo build` |
| `package.json` with a `build`/`test` script | that script (`npm run <s>`) |
| `*.xcodeproj` / `Package.swift` | `swift build` |
| none matched | disabled (no verify) |

Rationale: `build`-class over full `test` — a full suite auto-run every turn is
expensive and may have side effects. A user who wants stricter `test`-level
verification sets `verify.command` explicitly (or the agent writes it, §9). The
existing `HasSideEffectsFor` guard still refuses to auto-run a mutating command.

## 9. Agent self-write

The agent already writes `.codeagent/plans/` and (via `RuleStore.Grant`)
`settings.local.json` permission rules. Extend that:

- A small internal capability (a tool or a one-shot startup step) lets the agent
  **detect the project type and write `verify.command` into
  `settings.local.json`** the first time it is about to need it — the "agent
  configures the project for itself" behavior. It writes through the **same
  atomic, unknown-key-preserving** path `persistAllow` uses (generalized to
  `persist(path, block, value)`).
- Guardrails: writes only to `settings.local.json` (never shared, never
  `config.yaml`), only `verify`/`permissions` blocks, never secrets; the value is
  a detected build/test command, surfaced to the user (an event) so it is visible,
  not silent.

## 10. Loading architecture

- New `internal/settings` package: `Load(root string) Settings` returns the merged
  view across layers 1–3 (layer 0 values are passed in from `app.Config` and
  merged per §5). Best-effort loading, same discipline as `RuleStore.loadFile`.
- Consumers read their block from the merged `Settings`:
  - **permissions** → fold into `NewRuleStore` (add layer 2; today it reads 1 + 3).
  - **hooks** → `hooks.New(settings.Hooks + cfg.Hooks, root)`.
  - **verify** → `Runner.VerifyCommand = settings.ResolveVerify(root)` (auto-detect
    lives here).
- **Embedded / iOS hosts** have no fixed disk path (sandbox), exactly like
  `.mcp.json` today ([config.go](../internal/app/config.go) notes embedded hosts
  inject MCP in-memory). So `settings` must accept an **in-memory injection** path
  for `internal/embed`, parallel to how config and MCP are injected.
- The atomic writer (`persist*` in `rules.go`) is generalized so both permissions
  and verify persist through one unknown-key-preserving function.

## 11. Guardrails

- Malformed file → log + ignore (never brick), as today.
- Writes are atomic (temp + rename) and preserve unknown keys, as today.
- `settings.local.json` MUST be git-ignored: P11 adds/verifies a `.gitignore`
  entry (`.codeagent/settings.local.json`) as an implementation task; the shared
  `settings.json` is meant to be committed.
- Secrets are structurally excluded (§4).

## 12. Phasing

- **P11.a** — ✅ **shipped.** New [internal/settings](../internal/settings/settings.go)
  package: `File`/`Permissions`/`Settings` schema, `LoadFile` (missing≠error,
  malformed=error), `Load` (best-effort merge, permissions union), and the layer
  path helpers (`UserPath`/`ProjectSharedPath`/`ProjectLocalPath`). `NewRuleStore`
  now sources file permissions through `settings.Load`, which adds the
  **project-shared** `settings.json` (layer 2) to the union; grant/persist write
  paths unchanged (still local/user, never shared). Table tests: layer union
  incl. shared, dedup, missing-is-silent, malformed-logged-and-skipped. Only
  behavior change: permissions now also read `<root>/.codeagent/settings.json`.
- **P11.b** — ✅ **shipped.** `Verify` block (`command` + `enabled`) added to the
  schema, merged as an OVERRIDE (highest layer wins; pointer distinguishes
  absent). `settings.ResolveVerify(root, home, legacy)` resolves the effective
  command: settings block > `config.yaml` legacy > OFF; `"auto"`/`off`/`""`
  handled; `DetectVerify` implements §8 (go.mod→`go build ./...`, Cargo→`cargo
  build`, package.json→`npm run build`/`npm test`, Package.swift/`*.xcodeproj`→
  `swift build`), build-class only. `BuildRunner` sources `Runner.VerifyCommand`
  through it. Auto-detect is **opt-in** (only `command:"auto"`), so an unconfigured
  project stays OFF (decision 3). `agent.verify_command` marked legacy in the
  example. Tests: override precedence, legacy fallback, off-when-absent, disable
  words + `enabled:false`, and every detection marker.
- **P11.c** — ✅ **shipped.** `hooks.Hook` gained JSON tags (one type for both
  `config.yaml` yaml and settings JSON); `settings.File`/`Settings` carry a
  `Hooks []hooks.Hook` block that **concatenates** across layers (user → shared →
  local). `BuildRunner` now loads the settings layer **once** and feeds both verify
  (`ResolveVerifyFrom`) and hooks; hooks are `append(cfg.Hooks, settings.Hooks…)`.
  **Correctness fix:** hooks (config- AND settings-layer) are gated on
  `cfg.Profile.AllowsSubprocess()`, so a no-subprocess host (iOS) suppresses *all*
  hook layers, not just `cfg.Hooks`. Tests: cross-layer concatenation + order +
  JSON shape. Example config documents the settings `hooks` block.
- **P11.d** — ✅ **shipped.** Single canonical atomic writer `settings.Persist`
  (unknown-key-preserving, process-serialized) with `AddAllowRule` +
  `SetVerifyCommand` + `EnsureGitignored`; `RuleStore.persistAllow` now delegates
  to it (writer no longer duplicated). New `set_verify_command` tool
  ([internal/tools/projectcfg](../internal/tools/projectcfg/set_verify.go)) — the
  agent detects the project type (or takes an explicit command) and persists
  `verify` to `settings.local.json`, best-effort git-ignoring it; registered where
  `run_command` exists (so it is never inert). Embedded-host injection:
  `settings.ParseJSON` + `embed.Options.SettingsJSON` folded into the config layer
  (permissions/verify/hooks), before the sandboxed block so iOS still nils hooks.
  Tests: writer round-trip + preserve, idempotent grant, gitignore, ParseJSON, and
  the tool (explicit/auto/none/side-effecting).

## 13. Decisions (signed off)

1. **Shared vs local default for a NEW agent-written `verify`** — ✅ **local**
   (`settings.local.json`): a detected command is a machine guess, not a team
   decision; promote to shared by hand. Mirrors interactive permission grants.
2. **Keep `agent.verify_command` or deprecate** — ✅ **keep** as the documented
   layer-0 / embedded-injection path; mark it "legacy, prefer settings".
3. **`verify: auto` default ON or OFF** — ✅ **OFF** until P11.b proves detection;
   then a `build`-class default (never `test`) so it is cheap and safe.
4. **File format** — ✅ **JSON** (Claude-compatible); the files already exist as
   JSON (`rules.go`), keeping 1:1 compatibility so users can copy Claude blocks.
