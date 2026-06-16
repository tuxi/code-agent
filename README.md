# Code Agent

Code Agent is an experimental, AI-native coding agent runtime written in Go.

It is not a chatbot wrapper. The goal is to build a real coding agent from first
principles, where the model owns the control flow and the runtime owns the
boundaries, execution, and observation:

```
Goal → Model reasons & calls tools → Runtime executes under policy → Results fed back → … → Done
```

The long-term aim is an on-device agent that migrates cleanly to a server-side
runtime, with coding as the entry point.

---

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

## Where the project is now

A working foundation already exists, built on an early "JSON decision protocol"
(the model emits a typed JSON decision each turn; the runtime parses and acts).
The following is implemented and runnable:

- **Read-only foundation** — CLI entrypoint, OpenAI-compatible provider, agent
  loop, tool registry, and the tools `list_files`, `read_file`, `grep`.
- **Code editing** — `git_diff`, plan/patch-proposal flow, `apply_patch`
  validation via `git apply --check`, user-confirmed apply, post-apply diff,
  and a first rollback strategy.
- **Command execution** — `run_command` with an allowlist, timeout control, and
  a basic sandbox policy.

This foundation works, but it has three structural limits that the roadmap below
addresses directly:

1. **Tool metadata is duplicated in three places** (the `Tool` interface, the
   system prompt, and the loop's `switch`). Adding a tool requires editing the
   prompt and the loop. The Registry should be the single source of truth.
2. **Control flow is baked into the runtime** as decision types
   (`plan`, `patch_proposal`, …) and a hardcoded patch orchestration sequence.
   This makes the agent feel like a workflow and makes the loop grow with every
   capability.
3. **No context engineering** — runs are bounded by `max_steps` rather than a
   token budget, there is no compaction, and the project memory file
   (`CODEAGENT.md`) is not injected.

The project is now entering a deliberate refactor toward a native tool-calling
agent with a layered harness.

---

## Target architecture

The runtime is decomposed into clear layers, each owning one concern. The loop
itself stays small and **business-agnostic** — it must not know about patches,
plans, or git.

```
cmd/codeagent            CLI entrypoint
  ↓
internal/agent           Loop driver (thin) + run state
internal/context         Conversation/context manager: messages, prompt
                         assembly, token accounting, compaction
internal/model           Provider abstraction: tool-calling protocol, retries,
                         (later) streaming
internal/tools           Tool registry (single source of truth) + tool impls
                         (filesystem / search / git / shell)
internal/sandbox         Policy & permission layer: what is allowed, what needs
                         confirmation, allowlists
internal/memory          Project memory (CODEAGENT.md) + session persistence
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

- [ ] Extract the context manager (owns messages and prompt assembly).
- [ ] Extract the policy/permission layer behind an interface; move every
  `ui.Confirm` gate into it. The CLI prompt is one implementation.
- [ ] Make the loop driver thin and business-agnostic (no patch/plan/git
  branches).
- [ ] Add retry/backoff in the provider so a transient API error does not kill
  the run.
- [ ] Move patch validate → apply → diff orchestration out of the loop:
  `apply_patch` self-validates, the policy layer gates the apply, and the
  model decides whether to run tests.
- [ ] Emit structured trace events from the harness.

**Done when:** the loop driver contains no tool-specific logic, the permission
layer is swappable, and runs survive transient API errors.

### Phase 3 — Context engineering

- [ ] Inject `CODEAGENT.md` as project memory at session start.
- [ ] Add token accounting; budget on tokens (keep `max_steps` as a safety cap).
- [ ] Add compaction near the context-window limit (summarize old turns, keep
  recent ones verbatim).
- [ ] Add session persistence (SQLite) for resume and trace.

**Done when:** long runs do not overflow the context window, and project
conventions persist across runs.

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

- Go 1.22+
- An API key for a model that supports function calling

## Setup

```bash
go mod tidy
cp config.example.yaml config.yaml
export DEEPSEEK_API_KEY="your_api_key"
```

Example `config.example.yaml`:

```yaml
model:
  provider: deepseek
  base_url: "https://api.deepseek.com"
  model: "deepseek-v4-flash"   # must support function calling; verify in Phase 1
  temperature: 0.2

agent:
  max_steps: 16                # safety cap; the real budget becomes tokens (Phase 3)

workspace:
  root: "."
```

## Usage

```bash
# Ask a normal question
go run ./cmd/codeagent ask "你是谁"

# Run the agent loop
go run ./cmd/codeagent run "解释下这个项目结构"
```

---

## Project belief

```
An AI-native agent is a runtime where the model decides the next step,
while the system provides tools, boundaries, state, memory, and observation.
The runtime stays thin. The intelligence lives in the model.
```