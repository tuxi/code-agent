<!-- BEAUTIFIED -->
<!-- AUTO-GENERATED -->
<h1 align="center">CodeAgent</h1>

<p align="center">
  <strong>An AI-native coding agent runtime — the model decides, the runtime enforces.</strong>
  <br />
  <em>CLI · TUI · macOS GUI · iOS · Server — one runtime, every surface</em>
</p>

<p align="center">
  <a href="#quick-start"><img src="https://img.shields.io/badge/Quick_Start-00ADD8?style=for-the-badge" alt="Quick Start" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow?style=for-the-badge" alt="License" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-00ADD8?style=flat&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/SQLite-003B57?style=flat&logo=sqlite&logoColor=white" alt="SQLite" />
  <img src="https://img.shields.io/badge/OpenAI-412991?style=flat&logo=openai&logoColor=white" alt="OpenAI Compatible" />
  <img src="https://img.shields.io/badge/Ollama-000000?style=flat&logo=ollama&logoColor=white" alt="Ollama" />
  <img src="https://img.shields.io/badge/Swift-FA7343?style=flat&logo=swift&logoColor=white" alt="Swift" />
</p>

---

## Features

| Feature | Description                                                                                                                                                                                                                                               |
|---|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Uniform agent loop** | Model → tools → feedback. No workflow state machine. Adding a tool requires only registration, not loop edits.                                                                                                                                            |
| **Three-layer tool system** | Text (`list_files`, `read_file`, `grep`), Structure (`edit_file`, `apply_patch`, `git_diff`), Semantics (`project_graph`) plus policy-gated shell (`run_command`).                                                                                        |
| **Policy-gated execution** | Every command classified as allow / confirm / block. Quoted arguments are data, not syntax. No shell interpreter smuggling.                                                                                                                               |
| **Context engineering** | `AGENTS.md` project memory at session start. SQLite persistence with LLM-driven compaction. Token-aware budget per model.                                                                                                                                 |
| **Progressive disclosure via Skills** | `load_skill` tool pulls guidance on demand. Only the L1 index lives in the system prompt. Model loads what it needs, never auto-injected.                                                                                                                 |
| **Cross-session control plane** | Sessions are first-class, addressable resources. `list_sessions` discovers any session across workspaces, `read_session` inspects one without opening it, `send_to_session` dispatches a turn, `wait_sessions` blocks on its terminal outcome.            |
| **Manifest-driven workflows** | `plan_workflow` executes deterministic DAG templates (`cross_workspace_collaboration_v1/v2`) instead of an LLM-generated graph. A typed manifest of agents is validated, fanned out across worker sessions, and converged at fan-in.                      |
| **Generator-Critic pipeline** | The v2 template runs each implementation through an independent reviewer session that must reply with a strict `VERDICT: PASS` / `VERDICT: REQUEST_CHANGES`, wired to acceptance criteria per agent.                                                      |
| **Typed tool contracts** | Session tools expose structured output schemas (`send_to_session` → `{accepted, delivery, session_id, turn_id, cursor}`), so workflow expressions reference real fields and a contract test enforces schema/output agreement.                             |
| **Multi-surface** | TUI workspace, interactive REPL, one-shot `run`/`ask`/`goal`, runtime server with WebSocket agent-wire protocol, and [AgentKit](https://github.com/tuxi/AgentKit) — a native SwiftUI GUI for macOS and iOS that embeds CodeAgent as an on-device runtime. |

## Quick Start

### Prerequisites

- Go 1.25+
- An API key for a model that supports function calling

### Install

```bash
go install ./cmd/codeagent
```
配置（可选）：首次启动会自动生成 `~/.codeagent/settings.json` 模板；要迁移旧的
`config.yaml`，运行 `codeagent migrate`。也可从 `config.example.yaml` 参考字段。

### Configure

```bash
export DEEPSEEK_API_KEY="..."
```

### Run

```bash
# TUI workspace (default)
codeagent

# REPL with a specific model
codeagent --model deepseek repl

# One-shot task
codeagent run "explain this project"
```

Sessions persist per-project to `.codeagent/sessions.db`. List them with `codeagent sessions`, resume with `codeagent resume <id>`, or switch inside the REPL with `/resume`.

## Usage

### Interactive REPL

```bash
codeagent repl
> explain how RunTurn works
> /models
  deepseek
* deepseek-pro
  glm
  qwen
> /use glm
switched to glm (glm-5.1)
> /resume
  [1] 20260616-101500-a1b2c3d4  model=glm-5.1  msgs=42
Select a number to resume:
```

### Goal mode (headless, CI-compatible)

```bash
codeagent --auto goal "fix the failing test in internal/agent/loop_test.go"
# exit code 0 = achieved, others distinct by outcome (blocked, errored, budget)
```

### Serve mode

在用户级 `~/.codeagent/settings.json` 的 `server` 段配置一次固定的本地开发 Token
（或设置 `CODEAGENT_SERVER_ACCESS_TOKEN` 环境变量，优先）：

```json
{
  "server": {
    "authentication": "bearer",
    "access_token": "<openssl rand -base64 32 的输出>"
  }
}
```

```bash
codeagent serve 127.0.0.1:8797
# authenticated HTTP + Agent Wire WebSocket protocol for client integrations
```

生产环境可以设置 `CODEAGENT_SERVER_ACCESS_TOKEN` 覆盖 YAML Token。

See [Runtime Server deployment and security](docs/runtime-server-deployment.md)
before exposing a Local or Remote Server.

### Subagent trace

```bash
codeagent tasks                  # list delegations
codeagent task-trace <id>        # replay what the subagent did
```

### Cross-workspace collaboration

The `plan_workflow` tool executes deterministic workflow templates across independent agent sessions. A manifest describes each worker; the runtime validates it, fans it out, and converges the results.

```go
// v1: parallel dispatch + fan-in
plan_workflow(template: "cross_workspace_collaboration_v1",
  agents: [
    {role: "frontend", session_id: "<sid>", workspace_path: "/repo/frontend",
     message: "Build the frontend shell.", correlation_id: "todo/frontend/1"}
  ])

// v2: implement → independent review with VERDICT
plan_workflow(template: "cross_workspace_collaboration_v2",
  agents: [
    {role: "backend", session_id: "<sid>", reviewer_session_id: "<rev>",
     message: "Implement the API.", acceptance: "tests pass, no regressions",
     correlation_id: "todo/backend/1"}
  ])
```

Workers are resolved `create_session` child sessions owned by this runtime (guaranteed online, with a lease claimed synchronously at creation). A supervisor can drive the whole lifecycle from one conversation — see the `cross-workspace-orchestrator` skill.

## Architecture

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px'}}}%%
graph TD
    A[CLI / TUI<br/>cmd/codeagent] --> B[Agent Loop<br/>internal/agent]
    B --> C[Session<br/>internal/session]
    B --> D[Model Provider<br/>internal/model]
    B --> E[Tool Registry<br/>internal/tools]
    B --> F[Skills<br/>internal/skills]
    B --> G[Reflection<br/>internal/reflection]
    B --> H[Observation<br/>internal/observation]
    E --> I[Sandbox Policy<br/>internal/sandbox]
    E --> J[MCP Adapter<br/>internal/mcp]
    E --> K[Task / Subagent<br/>internal/tools/task]
    C --> L[(SQLite<br/>sessions.db)]
    B --> M[Cross-Session Control Plane<br/>internal/controlplane]
    E --> N[Session Tools<br/>list / read / send / wait]
    M --> N
    M --> O[Workflow Engine<br/>internal/runtime + flux]
    N --> O
    O --> P[plan_workflow templates<br/>v1 / v2 Generator-Critic]

    classDef client fill:#3B82F6,stroke:#2563EB,color:#fff,stroke-width:2px
    classDef loop fill:#10B981,stroke:#059669,color:#fff,stroke-width:2px
    classDef service fill:#10B981,stroke:#059669,color:#fff,stroke-width:2px
    classDef data fill:#8B5CF6,stroke:#7C3AED,color:#fff,stroke-width:2px
    classDef ext fill:#F59E0B,stroke:#D97706,color:#fff,stroke-width:2px

    class A client
    class B loop
    class C,D,E,F,G,H,I service
    class J,K,N,O,P ext
    class L data
    class M service
```

The loop (`internal/agent`) is business-agnostic. It assembles context, calls the model with tool schemas from the Registry, gates each call through the policy layer, feeds results back. Skills, observation, reflection, and subagents plug into nil-safe interfaces — the loop never changes.

## Configuration

Configuration lives in `settings.json` — layered user (`~/.codeagent/settings.json`)
over project (`<cwd>/.codeagent/settings.json` + `<cwd>/.codeagent/settings.local.json`)
— and in Claude-compatible `.mcp.json` files for MCP servers. Models, credentials
(references only — values stay in env/keychain), agent/provider/web knobs, and
permissions/verify/hooks all live here. `codeagent migrate` converts legacy
`config.yaml` files. First start auto-generates a template.

### Models

| Field | Description | Default |
|---|---|---|
| `default_model` | Model name used when `--model` is not set | deepseek-pro |
| `models.<name>.provider` | `openai` or `ollama` | — |
| `models.<name>.base_url` | API base URL | — |
| `models.<name>.model` | Wire model name | — |
| `models.<name>.api_key_env` | Environment variable holding the API key — **deprecated**; prefer `credential: {namespace, name}` referencing a `credentials:` entry (connection-flattening) | — |
| `models.<name>.credential` | `{namespace, name}` ref to a `credentials:` entry (e.g. `{llm, deepseek}`) | — |
| `models.<name>.context_window` | Max context in tokens; sizes compaction threshold | 128000 |
| `models.<name>.input_price_per_million` | Cost per 1M input tokens (for `stats`) | — |
| `models.<name>.cache_input_price_per_million` | Cost per 1M cached input tokens | — |
| `models.<name>.output_price_per_million` | Cost per 1M output tokens (for `stats`) | — |

### Agent

| Field | Description | Default |
|---|---|---|
| `agent.max_steps` | Hard step limit per turn | 32 |
| `agent.compact_ratio` | Fraction of context_window at which compaction fires | 0.75 |
| `agent.subagent_model` | Model for read-only `task` subagents (cheaper fallback) | — |

### Supported models

OpenAI-compatible providers: DeepSeek, Qwen (DashScope), GLM. Local models via Ollama native protocol or OpenAI-compatible endpoint (vLLM, llama.cpp, LM Studio).

## Project Structure

```
cmd/codeagent/         CLI entry point (repl, tui, serve, goal, stats, trace)
internal/
├── agent/             Loop driver (thin) — turn execution, plan mode, hooks
├── session/           Context assembly, token accounting, compaction, SQLite store
├── model/             Provider abstraction (OpenAI, Ollama, DeepSeek) + resilient retry
├── tools/             Tool registry + implementations (filesystem, git, shell, search, web, task)
│   ├── sessions/      Cross-session tools (list/read/send/wait, create/fork) + typed schemas
│   └── task/          Subagent delegation
├── sandbox/           Command policy classification (allow / confirm / block)
├── skills/            Skill registry + load_skill tool + plugin marketplace
├── mcp/               MCP client — stdio/HTTP/SSE transport, tool wrapping
├── observation/       Tool result classification (ok, compile/test/lint failure, salient lines)
├── reflection/        Post-turn self-check (unverified mutations, paper-over detection)
├── hooks/             Pre/post-tool shell hooks (deterministic, config-driven)
├── controlplane/      Cross-session control plane — leases, ownership, spawn, target resolution
├── conversation/      Agent-wire protocol, runtime server, WebSocket transport
├── server/            HTTP mux, wire encoding, control messages, job streaming
├── runtime/           Runner, workspace, registry builder, subagent spawning, flux workflow bridge
├── goal/              Goal-mode engine (admit → execute → verify → evidence)
├── credential/        Credential chain resolver (env, static, cached)
├── managedworktree/   Isolated git worktree provisioning for sandboxed child sessions
├── jobs/              Background job tracking
└── ui/                Console renderer, diff formatting, terminal helpers
pkg/agentapi/          Public API types
skills/                Built-in skills (code-review, verify-change, git-commit, cross-workspace-orchestrator, etc.)
mobile/                iOS embedding support
docs/                  Design docs and protocol specifications
```

## Tech Stack

| Layer | Technology | Purpose |
|---|---|---|
| Language | Go 1.25 | Entire runtime |
| Storage | SQLite (modernc.org/sqlite) | Session persistence, event store, request log |
| TUI | BubbleTea + Lipgloss | Terminal workspace |
| Version control | go-git | Git operations, diff parsing |
| MCP | modelcontextprotocol/go-sdk | External tool server integration |
| WebSocket | coder/websocket | Agent-wire protocol transport |
| Workflow | [Flux engine](https://github.com/tuxi/flux-workflow) — goal-based multi-step DAG execution, Map fan-out/fan-in, `plan_workflow` manifest templates (v1 / v2 Generator-Critic) |
| Cross-session | Control-plane lease/ownership — heartbeat TTL, spawn edges, target resolution, session tools with typed output schemas |
| LLM providers | OpenAI-compatible + Ollama native | DeepSeek, Qwen, GLM, local models |

## Contributing

1. Fork the repository.
2. Create a feature branch (`git checkout -b feat/amazing`).
3. Commit your changes following [Conventional Commits](https://www.conventionalcommits.org/).
4. Push and open a Pull Request.

Built-in skills provide guidance for contributing code: `verify-change` for safe edits, `code-review` for structured review, and `git-commit` for commit conventions.

## License

[MIT](LICENSE)
