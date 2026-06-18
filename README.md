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

The tools form three layers of capability:

- **Text** — `list_files`, `read_file`, `grep`: find and read by string.
- **Structure** — `edit_file`, `apply_patch`, `git_diff`: modify code and inspect changes.
- **Semantics** — `project_graph`: understand symbols, references, and rename safety.
- **System** — `run_command`: a policy-gated shell layer for git / build / test.

Full list:

- list_files
- read_file
- edit_file
- grep
- project_graph
- git_diff
- apply_patch
- run_command

### File modification philosophy

CodeAgent provides two complementary file-modification tools.

- `edit_file` is the primary editing tool.
  It performs direct file modifications and is optimized for routine code and
  documentation changes.

- `apply_patch` is a patch-oriented tool.
  It is useful when a precise diff must be reviewed, validated, or applied
  across multiple files.

The model chooses the appropriate tool.
The runtime does not enforce a workflow.

### Project Graph Principle

grep answers: "where is this text"

ProjectGraph answers: "what is this symbol and how does it relate"

The model should prefer ProjectGraph over grep whenever structural understanding is needed.

The `project_graph` tool exposes three actions, all returning JSON:

- `find_symbol` — locate a symbol's definition by name.
- `find_references` — find the use-sites of a symbol.
- `rename_check` — a safety report before a rename: how many files it touches,
  whether the target name already exists (collision), and any warnings.

> ProjectGraphTool does not implement language parsing.
> It delegates semantic understanding to language toolchains:
> gopls, sourcekitten, rust-analyzer, pyright.
> The system does not attempt to reimplement IDE-grade analysis.
> It composes existing compilers and language servers into a unified interface.

Each language has an adapter behind one `LanguageAdapter` interface; results are
normalized into a single `Symbol` / `Reference` schema. Go (gopls) is
implemented; Swift / Rust / Python are stubs that detect their toolchain and are
filled in behind the same interface. A backend whose toolchain is not installed
is skipped, never fatal — install e.g. `gopls` to enable Go semantics:

```
go install golang.org/x/tools/gopls@latest
```

### run_command permission model

`run_command` is a controlled system-shell layer, not an open one. Every command
is classified by a `sandbox.CommandPolicy` into one of three decisions:

- **allow** — read-only and build commands run directly, with no prompt
  (`ls`, `cat`, `grep`, `git status/diff/log`, `go build/test/vet`, `cargo check`).
- **confirm** — commands that mutate the tree, discard work, or reach the network
  require user confirmation (`rm`, `mv`, `curl`, `git checkout/commit/push`).
- **block** — a small set of catastrophic commands is refused outright
  (`rm -rf /`, fork bombs, `dd` to a disk, force-push to `main`), as are
  interpreter forms (`bash -c`, `sh -c`, `zsh -c`) that would smuggle an
  arbitrary nested script past per-command classification.

Classification ignores the contents of quoted arguments, so a commit message
that merely *mentions* `rm -rf /` or embeds a newline is treated as data, not
syntax — only an actual unquoted invocation is matched. The full command is
still shown at the confirmation prompt.

The confirmation gate is *command-aware*: a safe `git status` no longer prompts,
while a destructive `rm` always does. Output is structured
(`stdout`, `stderr`, `exit_code`, `duration_ms`, `command`, `decision`) so the
model can act on the result rather than parse prose. One command per call —
pipes, redirection, and chaining are a deliberate non-goal (no shell is spawned).

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

## Tool design principles

Tools represent capabilities, not workflows.

A tool should answer:

"What can the model do?"

not

"What sequence should the model follow?"

Good examples:

- read_file
- edit_file
- grep
- run_command

Bad examples:

- analyze_project
- create_plan_then_patch
- fix_build_errors

The latter encode workflows into tools and reduce model autonomy.

## Tooling Philosophy (Claude Code-inspired)
CodeAgent’s tool system is designed to evolve from a minimal CLI toolset into a developer-environment-grade agent runtime.

The long-term direction is inspired by Claude Code–style systems:

> Tools are not workflows.
Tools are atomic capabilities that compose into workflows implicitly through the model.

---

#### 1.Tools are atomic, not procedural

We explicitly avoid encoding workflows such as:

* plan → patch → test → fix
* read → modify → validate

Instead, we provide primitive operations:

* file I/O
* code search
* patch application
* test execution
* git inspection
* shell execution

The model is responsible for sequencing.

---

#### 2. The system evolves toward an “AI coding environment”, not a chatbot

Future tool design is guided by three missing primitives:

(1) Patch-first mutation model (not string replacement)

Current:

* edit_file(old, new)

Target:

* apply_patch (multi-hunk, git-compatible diffs)

Reason:

* enables refactors, batch edits, and model-driven code evolution

---

(2) Project structure intelligence layer

Current:

* grep-based search

Target:

* symbol graph + reference index

Examples:

* find_symbol
* find_references
* list_dependencies

Reason:

* enables semantic navigation instead of lexical search

---

(3) Execution environment (not command whitelist)

Current:

* allowlisted run_command

Target:

* sandboxed execution environment with:
* background jobs
* streaming logs
* long-running processes
* structured output capture

Reason:

* debugging is an iterative process, not a single command

---

#### 3. Workflow is emergent, not encoded

We intentionally do NOT implement:

* workflow engine
* task DAG system
* planning state machine

Instead:

workflows emerge from tool composition + model reasoning + feedback loops

Typical emergent pattern:

read_file → grep → apply_patch → run_tests → inspect diff → retry

This is not hardcoded — it is a natural result of tool design.

---

#### 4. Tool outputs must be machine-actionable

All tool outputs should evolve toward:

* structured results (not only text)
* parseable failure modes
* stable identifiers (file paths, symbols, diff hunks)
* retryable error semantics

This enables the model to:

* recover from failure
* self-correct
* iterate safely

---

#### 5. The system is converging toward an AI-native IDE kernel

Long-term vision:

CodeAgent is not a CLI tool.

It is an agent-native development kernel providing:

* code understanding primitives
* mutation primitives
* execution primitives
* traceable state transitions

The model provides intelligence.
The runtime provides guarantees.

---

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
- [x] Latency histogram / P95: `stats` reports P50/P95/P99 latency and an ASCII
  distribution histogram, computed in Go from the request log (the average hides
  the slow tail; the percentiles and shape show it).
- [x] Cost metrics: requests log completion tokens too; per-model prices
  (`input_price_per_million` / `output_price_per_million`) drive a `=== Cost ===`
  section in `stats` showing per-model token spend and the total.

**Done when:** a slow or failing run can be diagnosed from `stats` and the retry
log instead of a bare timeout error.

### Phase 3.7 — Agent event stream

The runtime emits a typed event stream instead of writing to stdout, so one turn
can drive a plain terminal, a live progress UI, or a remote event bus unchanged.
This is reusable Agent-Runtime infrastructure, not CLI glue.

- [x] EventEmitter: `Runner` emits `Event`s (turn start/finish, model
  start/finish + latency, thinking, tool start/finish, compaction) through an
  `Emitter` interface; the loop no longer prints. Each event carries
  `SessionID` + `TurnID` correlation ids so a multiplexed bus (concurrent runs,
  a web UI) never crosses streams.
- [x] **(P3.8)** Live progress: `liveProgress` decorates the console renderer
  with a "Thinking… Ns" ticker between `EventModelStarted` and
  `EventModelFinished` (TTY only), so a long wait reads as progress, not a hang.
  Added as a pure renderer — zero changes to the loop, agent, or session.

**Done when:** swapping the renderer changes the UX without touching the loop.

### Phase 3.9 — Tooling primitives upgrade (Claude Code parity layer)

- [x] Add apply_patch (multi-hunk diff model)
- [x] Add edit_file (small targeted edits)
- [x] Add policy-gated shell layer (run_command)
- [x] Add machine-readable tool outputs
Background-jobs arc (single-thread agent → multi-task agent):
- [x] **(3.9.a)** Background jobs — `run_command "background": true` → `job_id`;
  `job_status` / `job_logs` / `job_cancel`. Long build/test no longer blocks the
  loop or hits the 120s timeout. Jobs use a detached context (real background).
- [x] **(3.9.b)** Teach the agent to use it — system prompt: prefer background
  for long commands, keep working, poll sparingly. (The engine existed; the
  agent now knows the car can drive.)
- [x] **(3.9.e)** Background Observation — `Observe` now classifies `job_status`
  and `job_logs` results through the same entry point, so a failed background job
  becomes a real failure (`failure=test`, salient lines) instead of a generic OK.
  This is really *P4 Observation extended to the async world* — without it half
  the agent's world was blind.
- [x] **(3.9.e.1)** Reflection on background — Reflection now reads the
  `failure=test` marker from any tool (incl. background `job_logs`), so the
  paper-over / verify-fix signals fire on background work too.
- [ ] **(3.9.c)** Incremental logs — `job_logs` offset/cursor so re-polling a
  large build does not re-flood the context (the token problem again).
- [ ] **(3.9.d)** Streaming console output — live output for foreground commands.
- [ ] **(3.9.f)** Agent proactively schedules jobs — Claude-Code-level: start
  tests, keep analyzing, check results, fix, re-test.

Other Phase 3.9 items:
- [ ] Add tool result attachments
- [ ] Add retryable vs fatal tool errors
- [ ] Add tool chaining through structured outputs

### Phase 4 — Thinking & Reflection Runtime

The spine here is "the model owns control flow." Observation and Reflection are
**data layers** (classify, summarize, surface), never control machines; the
verify-fix loop *emerges* from the uniform loop rather than being hardcoded.
Design docs: [docs/p4.1-observation.md](docs/p4.1-observation.md),
[docs/p4.3-reflection.md](docs/p4.3-reflection.md).

### P4.1 Tool-driven reasoning  *(shipped)*
- [x] Observation model — `internal/observation`, structured `{ok, failure_type,
  summary, salient}` enriched ahead of each tool result
- [x] Tool Result Summarizer — salient-line extraction (signal, not the dump)
- [x] Failure Classification — compile / test / lint / runtime / timeout / blocked
- [~] Retry Planning — *reframed as data, not a runtime planner*: Observation
  surfaces the failure; the **model** decides the retry (see the guardrail)

### P4.2 Verify-Fix Loop  *(emergent — validated)*
- [x] Verify → Observe → Fix loop — emerges from P4.1 + the uniform loop; seen
  end-to-end in a real run (test fails → read → edit → re-test → green)
- [x] Compiler- / test- / lint-driven repair — emergent: the model reads the
  classified Observation and fixes; no per-failure control code
- [x] Max retry budget — `max_steps` backstop + one-shot reflection

### Phase 4.3 — Reflection  *(shipped)*
- [x] Lightweight self-check — `internal/reflection`, an ephemeral nudge at the
  finalize boundary (mirror of the convergence nudge), one-shot per turn
- [x] Detect unfinished work — `UnverifiedMutation` (code changed, never verified)
- [x] Detect unverified assumptions — `TestEditedAfterFailure` (paper-over guard)
- [x] Suggest next verification step — the nudge asks; the model decides
- [ ] Broader signals (partial-verification scope, more languages) — deferred to
  telemetry (see PRD §13)

### Phase 5 — Semantic code intelligence (Project Graph)

> Shipped but **parked**: the tool and the Go backend exist; the remaining
> backends are deferred behind the Observation/Verify-Fix work, which is higher
> ROI now (see Phase 4).

- [x] ProjectGraphTool (`find_symbol` / `find_references` / `rename_check`,
  unified `Symbol` / `Reference` schema, one `LanguageAdapter` interface)
- [x] Go backend (gopls)
- [ ] Swift backend (SourceKit / sourcekitten) — stub, detects toolchain
- [ ] Rust backend (rust-analyzer) — stub, detects toolchain
- [ ] Python backend (pyright) — stub, detects toolchain

### Phase 6 — Skills (progressive disclosure)

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

## Key Insight
* As the tool system matures, the agent loop does not become more complex.
* Instead, **the tools become more composable, structured, and environment-like.**
* This shifts the system from:
> a model calling functions

to:
> a model operating a programming environment

## Project belief

```
An AI-native agent is a runtime where the model decides the next step,
while the system provides tools, boundaries, state, memory, and observation.
The runtime stays thin. The intelligence lives in the model.
```