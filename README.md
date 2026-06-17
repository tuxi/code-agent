# CodeAgent

CodeAgent is a lightweight Claude Code–style coding agent written in Go.

It is built around a simple idea:

> The model owns control flow.
> The runtime owns boundaries.

The runtime stays thin.
Tools are explicit.
Context is assembled into a session.
Every action is observable and reviewable.

Current capabilities:

- Tool Calling agent loop
- Interactive REPL with persistent session memory
- Project memory via CODEAGENT.md
- Multi-model support
- Human approval gates for side-effecting tools
- File editing, git diff, shell execution

## Philosophy

```
An AI-native agent is not a fixed workflow.

The model decides what to do next.
The runtime decides what is allowed to actually happen.
Tools are explicit, typed, observable capabilities.
Context is engineered, not assumed.
Every step is traceable.
No hidden automation. No uncontrolled file modification.
```

A practical consequence drives the whole design: **control flow belongs to the
model, not to a runtime state machine.** The runtime stays thin and uniform; it
does not encode task-specific sequences.

---

## Features

### Agent Runtime

- Native tool-calling loop
- Thin runtime
- Tool registry as the single source of truth
- Human approval for side-effecting tools

### Session

- Persistent conversation state
- Multi-turn REPL
- Project memory via CODEAGENT.md

### Models

- Multiple configured models
- Runtime model switching
- OpenAI-compatible providers

### Built-in Tools

- list_files
- read_file
- edit_file
- grep
- git_diff
- apply_patch
- run_command

## Target architecture

The runtime is decomposed into clear layers, each owning one concern. The loop
itself stays small and **business-agnostic** — it must not know about patches,
plans, or git.

```
cmd/codeagent            CLI entrypoint
  ↓
internal/agent           Loop driver (thin) + run state
internal/session         Session state + context assembly: messages, prompt
                         assembly, token accounting, compaction, project memory
                         (CODEAGENT.md), and SQLite persistence
internal/model           Provider abstraction: tool-calling protocol, resilient
                         retries/backoff, (later) streaming
internal/tools           Tool registry (single source of truth) + tool impls
                         (filesystem / search / git / shell)
internal/sandbox         Policy & permission layer: what is allowed, what needs
                         confirmation, allowlists
internal/skills          Skill registry: progressive disclosure of guidance
internal/trace           Structured event tracing
internal/prompt          Base identity + prompt assembly (NOT the tool catalog)
internal/ui              CLI permission implementation
pkg/agentapi             Public API types
```

### The agent loop (target)

The loop becomes a single uniform cycle, identical regardless of how many tools
exist:

```
1. Assemble context (system identity + project memory + relevant skills + history)
2. Call the model with the tool schemas from the Registry
3. Model returns reasoning text and/or tool calls
4. For each tool call: the policy layer gates it; the runtime executes it
5. Tool results are fed back as tool messages
6. Repeat until the model returns a final text response with no tool calls
```

There are no `plan` / `patch_proposal` branches. Planning, asking the user, and
applying patches are ordinary tools. Confirmation lives in the policy layer, and
the sequencing of validate → apply → test is decided by the model, not the loop.

---

## Roadmap

The phases are ordered by dependency. Each phase is independently runnable and
testable.

### Phase 1 — Native tool calling & a thin loop  *(keystone)*

Unlocks all three structural limits at once.

- [x] Confirm the configured model supports function calling (and reasoning);
  swap the model if it does not.
- [x] Extend `model.Request` with `Tools`; extend `model.Response` / `Message`
  to carry `tool_calls`, the `tool` role, and `tool_call_id`.
- [x] Update the OpenAI-compatible provider to send `tools` and parse
  `tool_calls`.
- [x] Change `Tool.InputSchema()` to return structured JSON Schema; the Registry
  emits the `tools` array.
- [x] Rewrite the loop as a uniform model → tools → feedback cycle; stop on a
  text-only response.
- [x] Dissolve decision types: `plan` → a todo/plan tool, `patch_proposal` →
  the `apply_patch` tool, `ask_user` → a tool or plain text.
- [x] Remove all tool descriptions from the system prompt; remove the
  "JSON only / no explanations" constraint so the model can think.

**Done when:** adding a new tool requires only registering it (no prompt or loop
edits), and the model produces reasoning text alongside tool calls.

### Phase 2 — Harness layering

- [x] Extract the context manager (owns messages and prompt assembly).
- [x] Extract the policy/permission layer behind an interface; move every
  `ui.Confirm` gate into it. The CLI prompt is one implementation.
- [x] Make the loop driver thin and business-agnostic (no patch/plan/git
  branches).
- [x] Add retry/backoff in the provider so a transient API error does not kill
  the run. `ResilientProvider` wraps every provider with a per-attempt timeout,
  bounded retries, exponential backoff + jitter, and error classification
  (408/429/5xx and transport errors retry; 4xx do not). Replaying is
  context-safe — the request is read-only, so retries never duplicate messages.
- [x] Move patch validate → apply → diff orchestration out of the loop:
  `apply_patch` self-validates, the policy layer gates the apply, and the
  model decides whether to run tests.
- [ ] Emit structured trace events from the harness.

**Done when:** the loop driver contains no tool-specific logic, the permission
layer is swappable, and runs survive transient API errors.

### Phase 3 — Context engineering

- [x] Inject `CODEAGENT.md` as project memory at session start.
- [x] Add token accounting; budget on tokens (keep `max_steps` as a safety cap).
- [x] Add session persistence (SQLite) for resume and trace. Sessions are saved
  per-project to `.codeagent/sessions.db` after every turn (full history +
  summary + compaction trace); `codeagent sessions` lists them and
  `codeagent resume <id>` continues one. Pure-Go driver (`modernc.org/sqlite`),
  no cgo.
- [x] Add compaction near the context-window limit (summarize old turns, keep
  recent ones verbatim). `LLMCompactor` folds dropped turns into a cumulative
  `Session.Summary`; history is rebuilt as system → summary → recent.
- [x] Make the budget model-aware: per-model `context_window` and a
  `compact_ratio` derive the compaction threshold; `/use` re-budgets the live
  session.
- [x] Make compaction observable: each compaction records a `CompactionStats`
  (before/after tokens, saved, ratio, summary size) on the session, finalized
  from the next call's measured prompt size — no fabricated post-compaction
  token count.
- [x] Aggregate compaction telemetry: `codeagent stats` / `/stats` report global
  and per-session compression ratio, saved tokens, and summary size — the
  evidence base for sizing retention, computed from the persisted compactions.
- [ ] **(P3.5)** Replace the fixed `KeepRecentMessages` retention (the last
  hardcoded magic number, `50` in `buildCompactor`) with a token-based recent
  window, sized from real telemetry rather than a guess. Deferred on purpose:
  collect `stats` over real runs first — the data may well show `50` is already
  enough, downgrading this from an architecture task to a config knob.

**Done when:** long runs do not overflow the context window, and project
conventions persist across runs.

### Phase 3.6 — Transport observability

The bottleneck has moved from "will the context overflow" to "why is this
request slow / failing". A bare `context deadline exceeded` is a black box.

- [x] Provider metrics: `ResilientProvider` emits a `RequestStat` per call
  (attempts, retries, timeouts, latency, error class) through an `Observer`; the
  CLI persists them to a `requests` table, and `codeagent stats` / `/stats` add a
  `=== Provider ===` section (requests, successes, failures, timeouts, retries,
  avg/max latency). Each retry also prints a one-line notice so a slow request is
  visible live.
- [x] Request trace: each request persists per-attempt detail (latency + result
  per attempt) as a JSON `trace`; `codeagent trace [N]` / `/trace [N]` show the
  last N requests broken down attempt by attempt.
- [ ] Latency histogram / P95.
- [ ] Cost metrics (token-based spend per model).

**Done when:** a slow or failing run can be diagnosed from `stats` and the retry
log instead of a bare timeout error.

### Phase 3.7 — Agent event stream

The runtime emits a typed event stream instead of writing to stdout, so one turn
can drive a plain terminal, a live progress UI, or a remote event bus unchanged.
This is reusable Agent-Runtime infrastructure, not CLI glue.

- [x] EventEmitter: `Runner` emits `Event`s (turn start/finish, model
  start/finish + latency, thinking, tool start/finish, compaction) through an
  `Emitter` interface; the loop no longer prints. The CLI wires a
  `consoleEmitter` that renders them — identical output, fully decoupled.
- [ ] **(P3.8)** Live progress: a renderer that shows an elapsed "Thinking… Ns"
  and the current step in place (built on `EventModelStarted` + `Elapsed`), so a
  long wait reads as progress, not a hang.

**Done when:** swapping the renderer changes the UX without touching the loop.

### Phase 4 — Thinking & reflection

- [ ] Adopt a reasoning-capable model / interleaved reasoning.
- [ ] Confirm the verify-fix loop closes through real tool feedback (tests,
  compiler, lint) — this replaces the old "self-validation loop" idea, which
  should emerge from the uniform loop rather than be a hardcoded state
  machine.
- [ ] Optional: a lightweight self-check before the final answer.

**Non-goal:** a heavyweight, separate reflection engine. Reflection is grounded
in real tool results, not a bolt-on subsystem.

**Done when:** the model autonomously runs tests, reads failures, and fixes them
without any hardcoded sequencing.

### Phase 5 — Skills (progressive disclosure)

- [ ] A skill = a named instruction document (+ optionally scoped tools) loaded
  into context only when relevant.
- [ ] Task-specific guidance lives in skills, not the base system prompt.

**Done when:** the base system prompt stays small as capabilities grow.

### Later / parallel

- [ ] MCP adapter — consume and expose tools through a standard protocol,
  registering them into the same Registry.
- [ ] Streaming output.
- [ ] Local/cloud runtime split — remote tool runtime, workspace adapter,
  server-side sandbox experiment.
- [ ] GUI.

---

## Design rules

```
1.  An AI-native agent is not a fixed workflow.
2.  The model owns control flow; the runtime owns boundaries, execution, observation.
3.  The Registry is the single source of truth for tools.
    Adding a tool must not require editing the loop or the prompt.
4.  Permission and confirmation live in a policy layer, never inline in the loop.
5.  Patches are never applied silently. Gates are policy; sequencing is the model's.
6.  Reflection emerges from real tool feedback, not a separate engine.
7.  Context is engineered: project memory is injected, history is compacted.
8.  Skills add guidance via progressive disclosure, only when the prompt would
    otherwise grow unbounded.
9.  Every step is traceable.
10. No hidden automation. No uncontrolled file modification.
```

---

## Requirements

- Go 1.25+ (the pure-Go SQLite driver requires a recent toolchain)
- An API key for a model that supports function calling

## Setup

## Quick Start

### Install
```bash
go mod tidy
cp config.example.yaml config.yaml
```

### Configure API Keys
```bash
export DEEPSEEK_API_KEY="..."
export DASHSCOPE_API_KEY="..."
export GLM_API_KEY="..."
```

### Install CLI
```bash
go install ./cmd/codeagent
```

### Start Interactive Mode
```bash
codeagent
codeagent --model qwen
```

### Example Session
```bash
> 解释这个项目结构
> 顺便看看 RunTurn 是怎么工作的
> /models             
  deepseek
* deepseek-pro
  glm
  qwen

> /use glm
switched to glm (glm-5.1)

> /model
current model: glm (glm-5.1)

> /resume
  [1] 20260616-101500-a1b2c3d4  model=glm-5.1  msgs=42  updated=2026-06-16 10:15
* [2] 20260616-093012-deadbeef  model=glm-5.1  msgs=8   updated=2026-06-16 09:31
Select a number to resume (enter to cancel): 1
resumed session 20260616-101500-a1b2c3d4 (42 messages)

> /exit
```

Sessions persist per-project to `.codeagent/sessions.db`. List them with
`codeagent sessions`, resume from the shell with `codeagent resume <id>`, or
switch between them inside the REPL with `/resume`.

### One-shot Mode
```bash
codeagent run "解释这个项目结构"
codeagent --model qwen run "解释这个项目结构"
```

### Ask Mode
```bash
codeagent ask "什么是 interface"
codeagent --model qwen ask "什么是 interface"
```

## Configuration
Example `config.example.yaml`:

```yaml
default_model: deepseek

models:
  deepseek:
    provider: openai
    base_url: "https://api.deepseek.com"
    # model must support function calling
    model: "deepseek-v4-flash"
    api_key_env: DEEPSEEK_API_KEY
    temperature: 0.2
    # max context in tokens; sizes the compaction threshold (optional, default 128000)
    context_window: 128000

  deepseek-pro:
    provider: openai
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-pro"
    api_key_env: DEEPSEEK_API_KEY
    context_window: 128000

  qwen:
    provider: openai
    base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
    model: "qwen3-coder-plus"
    api_key_env: DASHSCOPE_API_KEY
    context_window: 256000
  glm:
    provider: openai
    base_url: "https://open.bigmodel.cn/api/paas/v4"
    model: "glm-5.1"
    api_key_env: GLM_API_KEY
    context_window: 128000

agent:
  max_steps: 16
  # compact at this fraction of the model's context_window (optional, default 0.7)
  compact_ratio: 0.7

# transport resilience: per-attempt timeout + retry/backoff (all optional)
provider:
  request_timeout_seconds: 120
  max_retries: 2
  backoff_millis: 500
  max_backoff_seconds: 8

workspace:
  root: "."
```

Compaction is **model-aware**: the threshold is `context_window * compact_ratio`, so
a 256k model compacts later than a 128k one. The recent-window retention policy
(`KeepRecentMessages`) is separate — it decides *what* survives a compaction,
independent of *when* compaction fires.

## Project belief

```
An AI-native agent is a runtime where the model decides the next step,
while the system provides tools, boundaries, state, memory, and observation.
The runtime stays thin. The intelligence lives in the model.
```