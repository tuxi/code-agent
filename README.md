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
- [x] **(3.9.f)** Agent proactively schedules jobs — **emergent, validated**: on
  real runs the agent autonomously backgrounds a slow test, fixes the root cause
  (not the test), and re-verifies in the background before finishing. True
  concurrency (work-while-waiting across multiple jobs) is a *narrow* case — Go
  already parallelizes test packages and readable bugs make "read-all → fix-all →
  verify-once" optimal — so it is **not pursued** until a real workload rewards it
  (an unavoidably-long command running alongside independent work).

Dormant (deferred until a real signal — both are largely designed away by 3.9.b/e):
- [ ] **(3.9.c)** Incremental logs — `job_logs` offset/cursor so re-polling a
  high-output job does not re-flood the context. **Latent**: 3.9.e gives the
  agent a `failure=test` summary from `job_status`, so it reads `job_logs` once,
  not in a loop — the flood does not happen yet. Cheap to add. *Trigger:*
  transcripts show repeated `job_logs` polls on a chatty long command.
- [ ] **(3.9.d)** Streaming console output — live output for a long *foreground*
  command. **Shrinking niche**: background is now the path for long commands, so
  foreground commands are short; and the model can't consume a stream mid-call,
  so this is pure human-UX at a real architectural cost (coupling the tool to the
  emitter). *Trigger:* users report long foreground commands feel hung, or a
  "follow a background job's output live" console feature is wanted.

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

### Phase 6 — Skills (progressive disclosure)  *(shipped + validated)*

The third leg of the Claude-Code triad — **Tool** (capability) + **Observation /
Reflection** (correction) + **Skill** (experience). Design + applied methodology:
[docs/p6-skills.md](docs/p6-skills.md).

- [x] **(P6.a)** `internal/skills` registry — `SKILL.md` (frontmatter + body),
  `Meta{name, description, version}`, `Index()` / `Get()`. `PromptIndex` renders
  *only* name+description (a test guards against any body leaking into the base
  prompt — the progressive-disclosure north star).
- [x] **(P6.b)** `load_skill` tool (model-pull) + the L1 index injected into the
  system prompt + `EventSkillLoaded{name, version}` telemetry. A skill is loaded
  because the *model* chose to, never auto-injected.
- [x] **(P6.c)** Seed skills, chosen to change behavior not restate common sense:
  `codeagent-conventions`, `verify-change`, `review-change`.
- [x] **(P6 nudge)** A first-call ephemeral reminder makes skill-loading
  *consistent across models* (deepseek self-loads; glm needed the nudge) without
  forcing it — the model still pulls.

**Validated on real runs:** the agent self-loads a matching skill (and composes
several), but only on tasks that match a description — investigation tasks load
nothing. Proactive *and* discriminating.

**Done when:** the base system prompt stays small as capabilities grow. ✅ — only
the tiny index is ever in the base prompt; bodies load on demand.

> Telemetry-driven finding worth keeping: proactive skill use is model-dependent
> (agentic models self-load; weaker ones treat the index as passive), and a low
> trigger rate is a *description* problem first — fix the trigger, not the body.

### Phase 7 — Terminal UX Runtime (TUI workspace)  *(design, signed off)*

Every phase so far raised the agent's *capability*. P7 is the first to work on the
**Human ↔ Agent interface** — turning a command executor into a **Workspace** you'd
open every morning. It adds **no agent capability**: it is a *third renderer* of the
event stream the runtime already emits (the same seam that made `liveProgress` a
pure decorator — P3.7), plus a second `Approver`. Shipped as a new `codeagent tui`
(BubbleTea); `codeagent repl` stays as the headless/CI path. Design PRD:
[docs/p7-tui.md](docs/p7-tui.md).

North star: **Timeline First, Chat Second** — the user watches *what the agent
did*, not reads *what it said*; tools, skills, reflections, and the reply are all
timeline events, rendered uniformly. Architecture guardrail: if P7 ends with a
diff inside `internal/agent`, it was done wrong.

- [x] **(M1 — "use it")** Workspace skeleton (`cmd/codeagent/tui`, run with
  `codeagent tui`): the async seam (channel `Emitter` + `Approver`, `RunTurn` on a
  background goroutine — *no interface signature changes*), an edit-first
  multi-line composer, the unified event **timeline** with Event Collapse, and a
  minimal y/n approval footer. Reducer + model unit-tested; live-terminal
  validation pending.
- [x] **(EventStore — the durable seed)** A `session_events` table persists the
  raw `agent.Event` stream (`RecordEvent` / `SessionEvents`), written by a
  composable `eventStoreEmitter` decorator that `run` / `repl` / `tui` all wrap in.
  The domain event (`agent.Event`) is the replayable truth; the timeline is a
  lossy projection of it. Foundation for replay / search / analytics / export.
  See [docs/p7-tui.md §7.1](docs/p7-tui.md).
- [x] **(M2 — "understand it")** Structured event cards: tool cards (✓/✗,
  duration; a failure prints its body, a success prints just its header), skill,
  reflection, and assistant cards, and a live status line (context gauge +
  skills). The card formatters + reducer are unit-tested.
- [x] **(Inline-mode pivot)** The headline architecture change from real use:
  **dropped alt-screen for inline mode** — finalized events print to the
  terminal's own **scrollback**, so native select/copy, scroll, and Ctrl+R search
  all just work. The program only owns a small live region (status + composer).
  This is "an enhanced terminal," not a full-screen TUI — the comfort behind
  Claude Code. (Cost: M2's *in-place* cursor-nav / expand-collapse and *live*
  Event-Collapse don't apply to immutable scrollback; the collapse library is
  retained for a future turn-end batch.)
- [x] **(Composer)** Auto-growing 1→8 rows (one line keeps the cursor on the
  bottom row — the *root* IME fix, not a mitigation), Enter sends / Alt+Enter
  newline, `ctrl+z` suspends.
- [x] **(Command registry)** `/` opens a filtered palette backed by a command
  registry (data + run-func + aliases, not a switch): `/help /sessions /model
  /clear /resume /exit`; commands are intercepted, never sent as chat.
- [x] **(`/resume` with history replay)** A session **picker** in the live
  region: ↑/↓ select, Enter resumes. Each row shows the session's first message +
  relative time · model · msgs (a new `Meta.Title` from the first user message).
  Selecting hands the loaded+re-budgeted session to the run loop over a `swap`
  channel — applied at the next turn boundary (no hot-swap), reusing the REPL's
  `loadAndRebudget`. Resumed sessions **replay their history** to scrollback:
  persisted `agent.Event` records are re-rendered through the same `transcript`
  renderer as live output, so they read exactly as they did when they ran.
  Sessions older than the EventStore resume with context intact but no visible
  back-scroll. The renderer is extracted into a shared `transcript` type
  (`transcript.go`) used by both live rendering and replay.
- [x] **(`/use`)** A model **picker** in the live region: ↑/↓ select, Enter
  switches. The model swap is a `modelSwap` channel (the same between-turns safety
  as `/resume`): the run-loop goroutine calls the `ModelSwapFunc` callback, which
  rebuilds the provider/compactor and re-budgets the session — the exact `/use`
  logic from the REPL, run inside the goroutine.
- [x] **(M3 — "trust it")** Approval UX: the approval card (↑/↓ select y/n,
  argument preview) + `[v]` **diff preview**: pressing 'v' toggles a diff-like
  view below the card — `edit_file` shows `- old` / `+ new`, `apply_patch` shows
  the patch with +/- coloring, `create_file` shows the file content,
  `run_command` shows the command. Each format live-renders; dismissed on Esc/
  Enter.
  approval prompt with a diff **preview**; fills the placeholder
  `internal/ui/diff.go`. (Paused behind the inline-workspace P0s.)
- [x] **(M4 — "feel it's an IDE")** Workspace awareness: a git status summary
  (branch + modified/untracked files) printed on startup, after each turn, and
  after `/resume`. Runs `git branch --show-current` and `git status --short`;
  formatted as a compact dim line like `── main · M loop.go · ?? new.go ──`.
  Capped at 10 files so a noisy workspace stays readable.

**Done when:** swapping into `tui` changes the UX without a single diff inside
`internal/agent` — the same north star as P3.7, one layer up.

### Phase 8 — Agent platform: interop, delegation, extensibility

Every phase so far made CodeAgent a more capable *single* agent. Phase 8 opens it
up — to external tools (MCP), to sub-agents (delegation + context isolation), to
user customization (hooks, task structure) — plus the two model-layer gaps
(streaming UX, cache-accurate cost). This is the real distance between CodeAgent
and a Claude-Code-class platform.

The payoff of the harness discipline shows here: most of these plug into existing
nil-safe seams (the `Registry`; the `Approver` / `Observer` / `Reflector` /
`Emitter` interfaces; `jobs`) **without touching the loop**. Only streaming
reaches into the `Provider` interface. Items are ordered by value × fit × effort.

- [x] **(8.1) Cache-accurate cost** *(easy — quick win)* — **shipped.** Cached-input
  usage (`prompt_cache_hit_tokens` / `prompt_tokens_details.cached_tokens`,
  per-provider) is parsed into `model.Usage.CachedPromptTokens`, threaded through
  the request telemetry into the `requests` table (additive `cached_prompt_tokens`
  column), and `cache_input_price_per_million` splits cost into cached/uncached in
  the `stats` cost report. When the cache price is unset, cached tokens fall back to
  the full input price — so the estimate never silently under-counts. Honest caveat
  that still holds: this *measures* spend accurately, it does not *reduce* it — and
  compaction churns the prompt prefix, which busts the provider cache.
- **(8.2) MCP adapter** *(medium — highest strategic value)* — consume
  external MCP servers via the official Go SDK: `tools/list`, then wrap each
  remote tool as an ordinary `tools.Tool` (`Execute` → `tools/call`) in the same
  Registry — so MCP tools are gated by the same policy layer and enriched by the
  same Observation. The Registry being the single source of truth is what makes
  this drop-in. Risk: MCP server subprocess lifecycle, schema-translation edges,
  and treating remote tools as side-effecting (approval) by default. Shipped in
  iterations:
  - [x] **First slice** — stdio transport; `tools/list` → wrap each remote tool in
    the same Registry; `tools/call` with raw-JSON arg passthrough; text content
    (non-text → placeholder); every remote tool side-effecting; wire name
    `mcp__server__tool` vs. display label `mcp.server.tool` (function names must
    match `^[a-zA-Z0-9_-]+$`, so the dotted form is display-only); three error
    classes (protocol / tool / invalid-args); per-server connect timeout +
    skip-and-summarize. Lives in `internal/mcp`, **zero changes to the loop**.
  - [ ] **Async / lazy connect (next)** — today `buildRegistry` blocks startup on
    the handshake (a cold `npx` is ~12s of frozen UI; bounded by a 30s timeout but
    still synchronous). Launch the UI immediately and connect servers in the
    background, registering each server's tools when it comes up; surface
    connect progress/failure as timeline events instead of a stderr line. Design
    questions: a Registry that accepts late tool additions concurrency-safely (the
    loop snapshots `toolDefinitions` per turn, so additions must be safe and ideally
    land before the first turn), and what the model is told if it reaches for a tool
    whose server is not ready yet.
  - [ ] **Later follow-ons** — SSE / streamable-HTTP transport; real multimodal
    passthrough (blocked on `model.Message` carrying content parts, not a plain
    string); expose our *own* tools as an MCP server; show the label rather than the
    wire name in the approval prompt.
- **(8.3) Subagent / Task** *(medium — highest architectural fit)* — a `task`
  tool that runs a nested `RunTurn` on an *isolated* session and returns its final
  answer as the tool result. Not just Claude-Code parity: it is the root fix for
  context hygiene — push dirty/exploratory work into a sub-agent whose verbose
  investigation never pollutes the parent's context; only the conclusion comes
  back. Full design (verified against Claude Code's current subagent behavior):
  [docs/p8.3-subagent.md](docs/p8.3-subagent.md).
  - [x] **First slice** — a read-only, Explore-class subagent on an isolated,
    ephemeral session: a name-based, fail-closed allow-list toolset (no writes, no
    `task` ⇒ depth-1), a dedicated terse system prompt, an optional cheaper
    `subagent_model`, a step budget with a non-convergence return, and default-quiet
    output. `internal/tools/task` + `cmd/codeagent/subagent.go`; one additive field
    on the loop's `TurnResult`, otherwise the loop is untouched.
  - [x] **Observability** — two views of the subagent, neither of which floods the
    parent (default-quiet holds, because the parent's live renderer never sees the
    raw sub-stream): a **live condensed heartbeat** (`⟳ subagent · step N · tool`)
    on run/repl so a `task` call isn't a black box while it runs, and the **full
    transcript** persisted under the sub-session's id — `codeagent tasks` lists
    delegations, `codeagent task-trace <id>` replays exactly what the subagent did.
    Both are fanned out from the same sub-stream (store + heartbeat), bracketed by
    `task_started/finished`. (A TUI heartbeat is a follow-on.)
  - [ ] **Later follow-ons** — a writable subagent with approval propagation;
    parallel subagents (`jobs`); resumable sub-sessions; lifting the depth-1 cap
    (Claude Code allows depth-5); telemetry for a distinct `subagent_model` so its
    tokens land in the cost report.
- [x] **(8.4) Todo** *(easy–medium — pairs with the TUI)* — **shipped.** A
  `todo_write` tool the model maintains for a multi-step task: whole-list rewrite,
  items `{content, status, activeForm}` — the simpler, more robust model for a
  weak-delegation model than Claude Code's newer id-keyed Task tools (no cross-call
  ids to track). The loop emits `EventTodoUpdated` via a `TodoAnnouncer` interface —
  the same loop-stays-tool-agnostic pattern as `SkillAnnouncer` — rendered as a live
  checklist panel in the TUI and inline in the console. A "soft" tool, like skills:
  its value depends on the model using it.
- [x] **(8.4b) Plan mode** *(the "Plan" half — a mode, not a tool)* — **shipped.**
  A read-only research turn: `Runner.PlanMode` swaps the toolset to the read-only
  `PlanTools` allow-list (the subagent's read-only set + `todo_write`) and injects a
  one-shot plan nudge, so the model researches and produces an implementation plan
  but **cannot edit** — enforced, not advisory (a hallucinated write call is simply
  unavailable, so it's rejected). Entered via the REPL `/plan` toggle and the TUI
  `ctrl+p` (applied at the next turn boundary, with a `⏸ PLAN` status badge / `plan>`
  prompt). v1 is **plan-only** — re-run normally to execute; the approve-and-execute
  handoff (CC's `exit_plan_mode`) is a follow-on.
- [ ] **(8.5) Hooks** *(medium — extensibility)* — user-configured pre/post-tool
  commands (auto-`gofmt` after `edit_file`, guardrails, context injection). The
  loop already consults nil-safe interface hooks (Approver before, Observer after
  each tool); a `ToolHook` is the same pattern with a runner that executes
  configured commands (still under the sandbox). Decide semantics up front: can a
  hook block a call, rewrite args, or amend the result?
- [ ] **(8.6) Streaming** *(medium–high — lowest ROI, defer)* — stream model
  tokens (SSE) into token-delta events the renderer appends live. Honest caveat:
  the model cannot consume a stream mid-call, so this is *pure human UX*, and the
  `liveProgress` ticker already answers "is it hung?". It also reaches into the
  `Provider` interface and complicates `ResilientProvider`'s replay (you cannot
  replay half a stream). Do it only when "watching a long answer render" becomes a
  real complaint.

**Done when:** CodeAgent can register an external MCP tool, delegate a sub-task to
an isolated sub-agent, and report cache-accurate cost — i.e. it is a platform, not
just an agent.

### Later / parallel

- [ ] **Robustness: tool-call markup leak at the step limit** — when a turn hits
  `max_steps`, `finalAnswerAfterLimit` asks the model to answer with no tools, but
  some models (deepseek) emit their tool-call markup (`DSML`) as text instead of a
  real answer. The subagent already sanitizes this (8.3,
  [`looksLikeToolCallLeak`](cmd/codeagent/subagent.go)), but the **main** loop's
  final-answer path surfaces the garbage to the *user*. Detect and strip it (or
  re-prompt) in `finalAnswerAfterLimit` — a loop-level fix, so it stays generic
  across providers.
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