# TUI/CLI Impact Report — Connection Flattening Refactor

> Status: investigation (read-only), based on `design-connection-flattening.md` v0.1 and `design-runtime-models-wire-v2.md`
> Date: 2026-08-03
> Scope: cmd/codeagent (TUI/CLI), cmd/codeagentd, internal/app, internal/runtime, internal/server, internal/mcp

---

## 1. Investigation findings per area

### 1.1 Config loading — confirmed pain point

- `cmd/codeagent/main.go:48` — `app.LoadConfig("config.yaml")` is a pure CWD-relative load. `LoadConfig` (`internal/app/config.go:390-401`) reads the file relative to the process CWD, and on `os.ErrNotExist` silently falls back to `LoadConfigBytes(nil)`.
- `cmd/codeagentd/main.go:60` — identical `app.LoadConfig("config.yaml")`.
- `internal/app/config.go:422-430` — with no config file, `cfg.Models` defaults to the single builtin `deepseek` (openai-compatible, `https://api.deepseek.com`, `deepseek-v4-flash`, `DEEPSEEK_API_KEY`).
- `internal/app/config.go:433-440` — `default_model` falls back to `deepseek` (or first model) when unset.
- `cmd/codeagent/main.go:57` — after config load, `cfg.MCP = mcp.ResolveDesktop(root, ...)` layers `.mcp.json` per scope; `~/.codeagent` today only carries DataDir, `mcp.json`, `skills/`, `prompts/`, `settings.json` (`main.go:62-72`). Confirmed: **every new CWD needs a config.yaml copy or only the builtin deepseek is available** (no home-dir lookup, no upward search, no layer merge).

Line references to the design doc's own claims hold: `main.go:48` / `codeagentd/main.go:60` hardcode `LoadConfig("config.yaml")`, builtin fallback `config.go:422-430`.

### 1.2 Model selection and switching in TUI/CLI

- `internal/runtime/flags.go:7-22` — `ExtractModelFlag` pulls `--model NAME` / `--model=NAME` from any position; the name is a **friendly model name** resolved later via `Config.SelectModel`.
- `cmd/codeagent/main.go:44,139-147` — startup: `mc, err := cfg.SelectModel(modelName)` then `runtime.BuildProvider(mc, cfg.Provider, nil)`.
- `cmd/codeagent/main.go:118-136` — `codeagent serve` path: `SelectModel` + `BuildProvider` before `runServe`.
- `cmd/codeagentd/main.go:86-99` — daemon: same `SelectModel`/`BuildProvider`, plus `ModelNotConfiguredError` when `modelName != ""` and models empty.
- `cmd/codeagent/main.go:741-767` — TUI `/use` swap (`modelSwap`): `cfg.SelectModel(name)` → `runtime.BuildProvider(newMC, cfg.Provider, nil)` → `runner.Model/ModelName/Temperature/Compactor/CompactKeepTokens`, re-budget `sess`.
- `cmd/codeagent/tui/model.go:860-908` — `/use` picker uses `m.src.modelNames` (`[]modelInfo`), populated from `cfg.ModelNames()` (see `run.go:109,200-203`, `main.go:769`).
- `cmd/codeagent/tui/commands.go:37-38,120-134` — `/use`, `/model` commands; `/model` prints `m.header.Model` which is `mc.Name` (friendly name, `main.go:728`).
- `cmd/codeagent/tui/backend.go:30-37,64-67,121-130` — `modelSwap`/`modelSwapResult` channels; `modelSwappedMsg` carries new `HeaderInfo`.
- `internal/app/config.go:559-575` — `SelectModel` resolves friendly name → `ModelConfig`; empty name → `DefaultModel`; strict: unknown model → error; missing key → error (unless local base URL).

**Key fact**: TUI model switching resolves names against `cfg.Models` (map key = friendly name) at all times. `--model` semantics and `/use` both go through `SelectModel`.

### 1.3 Does the TUI consume `/v1/runtime/models`? — No

- TUI/CLI model lists come from `cfg.ModelNames()` / `cfg.Models` (`main.go:769`, `tui/run.go:200-203`), **not** from the HTTP catalog.
- `internal/server/mux.go:445-447` — `GET /v1/runtime/models` serves `opts.RuntimeModels` built by `BuildRuntimeContract`.
- `internal/server/runtime_contract.go:65-93` — `BuildRuntimeContract` builds the catalog from `cfg` (friendly name keys) with `Available` hardcoded `true` (`:142`).
- Consumers of `/v1/runtime/models`: `cmd/codeagentd/main.go:194-196` (daemon startup), `embed` hosts; **not** the TUI. Wire v2 changes are contract-facing (AgentKit/gateway), not TUI code.

### 1.4 Subagent / goal model resolution

- `internal/runtime/subagent.go:219-258` — `ResolveSubAgentModel` / `ResolveSubAgentModelWithCredential`: `cfg.Agent.SubagentModel` empty → inherit parent; else `cfg.SelectModel(name)` (strict: unknown → error, no fallback), plus credential pre-resolution and `BuildProvider(subMC, cfg.Provider, cred)`.
- `cmd/codeagent/goal.go:112-128` — `admitObjective` calls `runtime.ResolveSubAgentModel` for the LLMAdmitter.
- `cmd/codeagent/goal.go:134-146` — `newGoalEngine` calls `ResolveSubAgentModel` for the LLMChecker (independent judge).
- `cmd/codeagent/goal.go:190-199` — `goalOps`/`buildGoalOps` injected into the TUI as `tui.GoalOps`; TUI `/goal` pursues via `goalOps.Pursue` (`tui/run.go:156-180`), which uses `admitObjective` + `newGoalEngine` (goal.go:204-244).
- `internal/goal/checker.go:28` — LLMChecker comment: judge model built like resolveSubAgentModel; `internal/goal/admitter.go` similar.
- Subagent model is a **name-based** lookup through `cfg.SelectModel`; `agent.subagent_model` flows from `Config.Agent.SubagentModel` (`internal/app/config.go:257-261`).

### 1.5 MCP

- `cmd/codeagent/main.go:57` and `cmd/codeagentd/main.go:145` — `mcp.ResolveDesktop` / `wsReg.EnableMCP` per workspace.
- `internal/mcp/config.go` — MCP config entirely from `.mcp.json` (env var expansion `${VAR}` in `expandServer`, `:312-378`). MCP servers don't use `Config.Credentials`; grep for `credential.` in internal/mcp returns nothing. MCP credential lookup is via env expansion + injected secrets, not the `credential` package.

---

## 2. Impact list (concrete code locations)

### A. Config loading
| Location | Impact |
|---|---|
| `cmd/codeagent/main.go:48` | `LoadConfig("config.yaml")` → layered load (builtin → `~/.codeagent/config.yaml` → `<cwd>/config.yaml` → injected). TUI/CLI startup entry point. |
| `cmd/codeagentd/main.go:60` | Same layered load for daemon. |
| `internal/app/config.go:390-401` (`LoadConfig`) | Must merge layers; keep `LoadConfigBytes` for embedded hosts. |
| `internal/app/config.go:422-430` | Builtin deepseek fallback moves to registry layer; when registry is richer, the "only deepseek" fallback should go away (F2.5). |
| `internal/app/config.go:433-440,501-508` | `default_model` fallback rules and "default_model not defined under models" validation change with layered merge (project > user > registry default). |
| `internal/app/config.go:163-196` (`ModelConfig`) | `APIKey`/`APIKeyEnv` removed (F3.1); fields sourced from registry/connection. |
| `internal/app/config.go:210-224` (`CredentialRef`) | `{Namespace,Name}` → flat connection id (F1.1), compat parse retained. |
| `internal/app/config.go:257-261` (`Agent.SubagentModel`) | Becomes user-level preference (F2.2). |
| `internal/app/config.go:599-631` (`CredentialResolver`) | Chain → two-level (session override + connection single source); `EnvResolver` dual personality eliminated. |
| `internal/app/config.go:466-471` | Auto-derivation `api_key_env` → `CredentialRef{llm, name}` bridge goes away; warning for legacy `api_key_env` (F3.3). |

### B. Model selection / provider build
| Location | Impact |
|---|---|
| `internal/runtime/flags.go:7-22` (`ExtractModelFlag`) | `--model` value semantics: friendly name today; must accept connection/wire names post-flattening (see blockers). |
| `cmd/codeagent/main.go:118-147` | `SelectModel` + `BuildProvider` call sites. |
| `cmd/codeagent/main.go:741-767` (TUI `modelSwap`) | `/use` must resolve through the new profile/connection model. |
| `cmd/codeagent/tui/model.go:860-908` (`/use` picker) | Picker list comes from `cfg.ModelNames()` — must become profile list (connection+wire), display alias. |
| `cmd/codeagent/tui/commands.go:31-38` | `/model` prints friendly name; with display names vs aliases, decide which to show. |
| `cmd/codeagent/tui/run.go:109,200-203` | `modelNames []string` from `cfg.ModelNames()` feeds picker. |
| `internal/app/config.go:559-575` (`SelectModel`) | Resolution path changes: friendly name → (connection, wire_model) profile; keep strict unknown-model error. |
| `internal/runtime/provider.go:22-59` (`BuildProvider`) | Signature `BuildProvider(conn, timeout)`; chain-building removed (§10 step 4/7). |
| `internal/runtime/serve_builder.go:160-188` (`resolveTurnModel`) | `provider.*` alias strict path; bare wire model fallback for gateway. |
| `internal/runtime/serve_builder.go:190-203` (`effectiveCredentialResolver`) | Chain → two-level; removed per design. |
| `cmd/codeagentd/main.go:86-99,139,167,194-196` | Daemon startup model select + `BuildBaseRegistry` + `BuildRuntimeContract`. |

### C. Subagent resolution
| Location | Impact |
|---|---|
| `internal/runtime/subagent.go:219-258` | `ResolveSubAgentModel*` — `subagent_model` lookup must resolve against profile/connection model space, strict behavior kept. |
| `cmd/codeagent/goal.go:112-146` | Admitter + checker construction uses `ResolveSubAgentModel`. |
| `cmd/codeagent/goal.go:197-244` | `goalOps.Pursue` (TUI `/goal`) — inherits model resolution. |
| `internal/goal/checker.go:28` / `internal/goal/admitter.go` | Judge/admitter models — resolve through same path. |

### D. Catalog / wire
| Location | Impact |
|---|---|
| `internal/server/runtime_contract.go:37-61` | DTOs already connection+profile two-layer; v2 adds `credential.status/source`, `available` real, `unavailable_reason`. |
| `internal/server/runtime_contract.go:95-168` | `buildRuntimeModelCatalog` iterates `cfg.ModelNames()` friendly keys; must iterate profiles; `Available` hardcoded true → real auth state. |
| `internal/server/runtime_contract.go:192-218` | `runtimeAliasComponents` — alias format `provider.<b64>.model.<b64>` unchanged by flattening; rename migration needed. |
| `internal/server/mux.go:445-447` | Endpoint unchanged; payload evolves. |
| `internal/runtime/serve_builder.go:164-188` (`resolveTurnModel`) | Session model references (alias or friendly name) resolution. |

### E. MCP
| Location | Impact |
|---|---|
| `cmd/codeagent/main.go:57` | `mcp.ResolveDesktop` — no change to loading; shares only credential lookup. |
| `internal/mcp/config.go:312-378` | `${VAR}` expansion is MCP's own credential mechanism; two-level lookup should not replace it (MCP stays independent, §11). |
| `internal/runtime/serve_builder.go:293-305` | Per-workspace MCP tool registry; credential scope if MCP servers adopt two-level lookup (open question 5: name collision with connection ids). |
| `cmd/codeagentd/main.go:145` | `wsReg.EnableMCP` — per-conversation workspace; unaffected by config layering. |

---

## 3. Blockers (concrete)

1. **`--model` and `/use` semantics change with friendly-name → profile mapping.**
   `ExtractModelFlag` (`flags.go:7-22`), `SelectModel` (`config.go:559-575`), TUI `modelSwap` (`main.go:741-767`) all resolve a single friendly name string. Post-flattening a model is `(connection, wire_model)` and the friendly name may no longer be the map key. Without a stable mapping (friendly name → profile, or a CLI accepting `connection/wire_model`), `--model deepseek` and `/use` break.
2. **Alias change in persisted sessions (design open question 8).**
   `runtimeAliasComponents` (`runtime_contract.go:192-218`) produces `provider.<b64(connectionID)>.model.<b64(wireModelID)>`. Today `connectionID` derives from the **friendly name** (alias) or `Catalog.ConnectionID` (`runtime_contract.go:106-112`). After flattening, connection ids are flat (e.g. `deepseek`) while wire models are new names (`deepseek-v4-pro`) — existing persisted session model references (friendly-name-based aliases) become unresolvable. `resolveTurnModel` (`serve_builder.go:164-188`) already handles `provider.` strict + bare-string fallback, so a migration table (design v2 §6 option b) or rename-map is required; else `/resume` of old sessions silently drops the model.
3. **`default_model` fallback chain.**
   Today: `config.go:433-440` picks `deepseek` or first model; `config.go:501-508` errors if `default_model` is not under `models`. With layered config (project > user > registry), the fallback must go registry → user → project (§8.3), and the "not defined" validation must run against the **merged** model space, not the project file's. A project that only sets `default_model: qwen/qwen3` with no local `models:` must keep working.
4. **`SelectModel`'s strict error for missing keys.**
   `config.go:568-573` errors when `APIKey == "" && !IsLocalBaseURL && Credential.IsZero()`. After `APIKey`/`APIKeyEnv` removal, the "set the %s environment variable" branch disappears; the missing-credential error must instead consult the two-level resolver (session override + connection source). If this check is dropped or made non-strict at startup, TUI/CLI will start with a model that fails on first request (worse UX than today's early error).
5. **`subagent_model` becomes user-level — strict resolution must not break goal admission.**
   `ResolveSubAgentModelWithCredential` (`subagent.go:229-258`) is strict: unknown name → error, and `admitObjective` degrades to "fail open" on resolve error (`goal.go:113-116`). Moving `subagent_model` to `~/.codeagent/config.yaml` means a user-level value that names a model not present in the merged space (e.g. after registry changes) silently degrades judge independence — the design must keep the strict-error + fail-open contract and make the resolution operate on the merged space.
6. **`cfg.Models`-as-identity is baked into many call sites.**
   `printCostReport` (`main.go:322-341`) maps wire model string → `ModelConfig` by `cfg.Models` key; `ModelNames()` feeds the TUI picker (`run.go:200-203`) and `SelectModel` error text (`config.go:565-567`); `BuildRuntimeContract` iterates `cfg.ModelNames()` (`runtime_contract.go:101`). Any code that iterates `cfg.Models` by friendly key breaks if the merged space is `[]Profile` + connections instead of `map[string]ModelConfig`. Minimal-risk path: keep a merged `map[string]ModelConfig` view (friendly name → profile) in `Config`, so these call sites survive.
7. **TUI does not consume `/v1/runtime/models` — wire v2 must not be the only source of truth for the TUI.**
   If the flattening design routes model-list UI through the wire v2 catalog, the TUI would need to fetch from `GET /v1/runtime/models` (or embed the same builder). Today the TUI reads `cfg.Models` directly. Recommendation: keep `Config` as the single source for the TUI's picker (F4.4's "same-source" requirement is satisfied by sharing the builder, not by adding a network hop).
8. **MCP credential naming collision (open question 5).**
   If MCP servers adopt the two-level credential lookup keyed by flat connection id, an MCP server named `github` collides with a `github` connection. MCP config today is `.mcp.json`-only with `${VAR}` expansion (`config.go:312-378`); the design must either keep MCP credential resolution fully separate (recommended, §11) or namespace MCP keys.

---

## 4. Recommendations (actionable)

1. **Keep `app.SelectModel(friendlyName, cfg)` as the stable public resolution API.** Internally back it with a merged profile space; never make TUI/CLI call sites resolve `(connection, wire_model)` directly. All of: `--model` (`flags.go`), startup (`main.go:139`), `/use` (`main.go:742`), subagent (`subagent.go:234`), `resolveTurnModel` (`serve_builder.go:171`) already route through it — preserving its signature minimizes blast radius.
2. **Provide a friendly-name → profile mapping in the merged config.** Flat connection ids and new wire names (`deepseek-v4-pro`) must not force users to retype model references. Map old friendly names (`deepseek`) to `(connection: deepseek, wire_model: ...)` so `--model deepseek`, `/use deepseek`, `subagent_model: deepseek`, and persisted session aliases keep resolving (design wire-v2 §6 option (a) registry keeps old-name aliases is the cheapest).
3. **Implement the layered load in `LoadConfig` itself, keeping `LoadSettingsBytes` for embedded hosts.** The daemon (`codeagentd/main.go:60`) and CLI (`main.go:48`) both call `LoadConfig("config.yaml")`; changing that one entry point upgrades both. `default_model` validation (`config.go:501-508`) must run against the merged model space.
4. **Preserve the early missing-credential error in `SelectModel`.** When `APIKey`/`APIKeyEnv` are removed, replace the check with a two-level existence probe (session override + connection source, per design v2 §5: declaration-existence only, no network). Do not silently start with an unusable model.
5. **Treat `subagent_model` as user-level but resolve it through the same merged space with the same strict/fail-open contract** (`subagent.go:229-258`, `goal.go:112-116`). Emit the degraded-judge warning when a user-level subagent_model is unresolvable.
6. **Keep `cfg.Models` as a merged view for read call sites.** `printCostReport` (`main.go:322-341`), `ModelNames()` (TUI picker, `run.go:200-203`), and `buildRuntimeModelCatalog` (`runtime_contract.go:101`) all iterate `cfg.Models`; give them a merged `map[string]ModelConfig` (or equivalent) rather than rewriting every iteration site.
7. **Do not make the TUI depend on `/v1/runtime/models`.** The TUI and the wire catalog must share the same builder output (F4.4) but the TUI should keep reading `Config` — wire v2 changes (available tri-state, credential status) are for AgentKit/gateway UIs, not the desktop TUI.
8. **Keep MCP credential resolution independent.** Do not route MCP servers through the flat-connection two-level lookup without explicit namespacing (open question 5). `.mcp.json` + `${VAR}` expansion (`internal/mcp/config.go:312-378`) is a separate, working mechanism; "shares only credential lookup" should mean sharing the injected-secrets map, keyed with an MCP-specific prefix if at all.
9. **Ship the alias migration before or with the registry change** (design wire-v2 §6): old session references to friendly-name aliases must map to new flat aliases, with `/resume` fallback already demonstrated by `resolveTurnModel`'s bare-string path (`serve_builder.go:176-187`).

---

## 5. Open items for the design owner

- Friendly-name alias policy: keep old names as aliases in the registry (recommended) vs a startup migration table vs session-time re-resolution — needs a decision before `SelectModel`/alias code can be touched.
- Whether `--model` should additionally accept `connection/wire_model` syntax (CLI surface change; `flags.go` + usage text).
- TUI `/use` picker display: friendly names, display names, or aliases (`tui/model.go:860-908`, `HeaderInfo.Model` = friendly name at `main.go:728`).
- Whether `Config.Models` remains `map[string]ModelConfig` (merged) or becomes `[]Profile` — affects `main.go:322-341`, `runtime_contract.go:95-168`, and the daemon.
