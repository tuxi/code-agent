# Phase 6 — Skills (progressive disclosure) — design PRD

> Status: design. No implementation yet. The spec to hand a developer. Like the
> P4 docs it defines a **context-engineering layer** that the *model* drives —
> not a runtime that auto-injects guidance. Companion to
> [docs/p4.1-observation.md](./p4.1-observation.md) and
> [docs/p4.3-reflection.md](./p4.3-reflection.md).

## 1. Background

We have been hand-stuffing the system prompt: grounding rules, the stopping
bias, the long-running-commands section. That works for a *few universal*
behaviors. It does not scale. You cannot also pack "how to do a Go refactor in
this repo", "how to write a commit message here", "how to debug a flaky test",
and fifty more into the base prompt — every one of them would ride along on every
request (including "what is 2+2"), burning tokens and diluting the model's
attention with irrelevant instructions.

A Skill is a named instruction document loaded into context **only when the task
calls for it**. The base prompt stays small; a large library of task-specific
guidance becomes available on demand.

## 2. Goal

Let the project carry many task-specific playbooks without bloating the base
context, by loading a skill's full instructions only when the model judges it
relevant:

```
system prompt holds only the INDEX (tiny): name + one-line description per skill
        ↓  model sees the menu, matches the task
model: load_skill("go-conventions")        ← the model decides, like grabbing a manual
        ↓
the skill's full SKILL.md body returns as a tool result; the model follows it
```

## 3. North star / guardrail (the whole point)

**Progressive disclosure, not static injection.** This is the line that makes or
breaks the feature:

| | Static injection (what most "skills" actually are) | Progressive disclosure (this PRD) |
|---|---|---|
| In base context | the full guidance text, always | only `name` + one-line `description` |
| Adding 50 skills | base prompt 50× heavier; a Python task still carries the Go skill | base unchanged; irrelevant skills never enter |
| Trigger | none — it is always on | the model reads the description and decides |

And, consistent with this project's spine: **the model owns control flow.** A
skill is loaded because the *model* called `load_skill`, never because a runtime
matched keywords and pushed text in. Skills are advisory guidance the model
chooses to consult — not rules the runtime enforces.

Three disclosure tiers (Anthropic's model). Crucially: **a Skill is a folder,
not a file** — the directory itself is the progressive-disclosure mechanism.

- **L1 — metadata** (`name` + `description`): always in context. Tiny.
- **L2 — body** (`SKILL.md`): loaded when the skill is triggered. It *tells the
  model what else is in the folder.*
- **L3 — resources** (`references/*.md`, `scripts/`, `assets/`): the model reads
  or runs them on demand. **L3 references are already implemented — via
  `read_file`.** `SKILL.md` says "see references/api.md for the full signature
  list", and the model loads it with the existing `read_file` tool only when it
  needs it. So L3 is *not* a separate subsystem: a Skill is just a *local
  knowledge space*, and `read_file` already gives the model on-demand access to
  it. **Only executable resources (`scripts/`/`assets/` run conventions) remain
  deferred.**

The big mental shift (from Anthropic's experience): do not cram everything into
`SKILL.md`. Keep `SKILL.md` short and have it *point at* deeper files the model
pulls only when relevant — progressive disclosure inside the skill, using
`read_file` we already have.

## 4. Non-goals

- ❌ Runtime auto-injection of skills by similarity/keyword match (that is a
  control machine making the model's decision for it).
- ❌ Putting any skill *body* in the base prompt. Only the index lives there.
- ❌ Skills as a tool/capability mechanism. A skill is *guidance*; it may point at
  tools, but it does not implement them. (Skill ≠ Tool ≠ Subagent ≠ MCP.)
- ❌ L3 executable resources, and per-skill scoped tool sets — both deferred.
- ❌ Migrating the existing universal prompt sections into skills now. Universal
  behavior stays in the base prompt; skills are for *task-specific* guidance.

## 5. The mechanism (the crux)

Two pieces, both leaning on patterns already in the codebase:

**(a) The index in the system prompt.** Session assembly (`session/builder.go`,
which already injects `prompt.AgentSystemPrompt`) appends a small Skills section
built from the registry:

```
Skills — task-specific playbooks for this project. When the task matches one,
call load_skill(name) to load its full instructions before proceeding. Do not
guess a skill's contents; load it.
- go-conventions: Go style, testing idiom, and conventions for this repo.
- commit-message: How to write a commit message for this project.
```

This is the same shape as how tools are advertised (name + description); it costs
~one line per skill and nothing more.

**(b) The `load_skill` tool.** A read-only tool: given a skill name, it returns
that skill's `SKILL.md` body as the tool result. The body then lives in the
conversation like any other observation and the model follows it. The model
decides when to call it, exactly as it decides to call `read_file` or
`run_command`.

```
model reads the index → load_skill("go-conventions")
  → tool result = the full go-conventions body
  → model proceeds with that guidance in context
```

Why a tool (model pulls) and not runtime injection (runtime pushes): it keeps the
loop tool-agnostic, keeps the decision with the model, and reuses the exact
mechanism that already works for every other capability.

## 6. Skill format & storage

A `skills/` directory; each skill is a **folder** — the directory is part of the
context engineering:

```
skills/codeagent-conventions/
  SKILL.md              # short: the playbook + what other files exist
  references/
    loop-invariants.md  # read_file'd on demand: the thin-loop rules, etc.
  Gotchas live in SKILL.md (see below)
```

`SKILL.md` is YAML frontmatter + a short markdown body:

```markdown
---
name: codeagent-conventions
version: "1"
description: Conventions and known gotchas for working in THIS repo (the thin
  loop, no-shell run_command, comment density, commit format). Load when editing
  this project's Go code.
---

# Working in this repo

- The agent loop stays tool-agnostic — never import a tool package into loop.go.
- run_command has no shell: no pipes/redirection; one command per call.
- See references/loop-invariants.md before touching internal/agent.

## Gotchas
- (real pitfalls land here as we hit them — see §9)
```

Frontmatter fields for MVP: `name` (unique id), `version` (e.g. `"1"`, for
telemetry — §7), `description` (the trigger — §9). A future `tools:` field
(scoped tools) is reserved but not read yet.

**Gotchas are the highest-signal part of a skill** (Anthropic's finding). A
`## Gotchas` section records the *real* pitfalls the model hit while using the
skill — not the ones you predicted, the ones it actually tripped on — appended
over time as new edge cases appear. A skill with a good Gotchas list is worth ten
skills of tidy theory. As the list grows it will likely outgrow `SKILL.md` and
move to its own `gotchas.md` (read on demand via `read_file`) — the body stays
short, the gotchas accumulate beside it. In Anthropic's experience a mature
skill's value ≈ its gotchas' value, not its prose.

## 7. Data model

```go
package skills

// Meta is the L1 view that lives in the index — tiny, always in context.
type Meta struct {
    Name        string
    Description string
    Version     string // e.g. "1" — carried from day one so skill_loaded telemetry
                       // can compare trigger/success rates across versions later
}

// Skill is the full L2 view, loaded on demand.
type Skill struct {
    Meta
    Body string // the SKILL.md markdown after the frontmatter
}

// Registry loads skills from disk once and serves the index + bodies. Pure and
// read-only; it does not decide anything.
type Registry struct { /* name -> Skill */ }

func Load(dir string) (*Registry, error)     // scan dir, parse frontmatter
func (r *Registry) Index() []Meta            // for the prompt section, sorted
func (r *Registry) Get(name string) (Skill, bool)
```

## 8. Boundary — where guidance belongs

A decision table, so the line stays sharp:

| Put it in… | When | Example |
|---|---|---|
| **Base prompt** | universal, every task | "ground answers in real tool output"; "background long commands" |
| **A Skill** | task-/domain-specific | "Go refactor steps in this repo"; "our commit format" |
| **Reflection (P4.3)** | a *post-hoc check* on the model's own work | "you edited a test after it failed" |
| **A Tool** | a *capability*, not guidance | `run_command`, `project_graph` |

Skills do not replace Reflection: Reflection is a runtime data-layer check;
skills are guidance the model reads up front. They compose (a skill can say "fix
the source, not the test"; reflection catches it if the model does anyway).

## 9. Authoring a good skill (Anthropic's playbook, applied)

**The `description` is the load-bearing wall.** It is the *only* thing the model
matches against to decide whether to load a skill — a trigger condition, not a
feature summary. 80% of a skill's quality lives here.

- State **when to use it**: "Load when editing this project's Go code," not
  "Go stuff."
- Specific enough to *not* fire on unrelated tasks, broad enough to fire on real
  ones. A vague description never triggers, or triggers everywhere.
- One or two sentences; the index must stay tiny.

**Don't encode common sense.** Claude already knows Go, git, testing. A skill
that mostly restates defaults is worthless — and our originally-proposed
`go-conventions` was exactly that trap. Spend the words on what pushes the model
*out* of its defaults: this repo's non-obvious rules and the things it actually
gets wrong here.

**Give information, not a script.** Skills are reused across many situations, so
over-specific step-by-step instructions backfire. Give the model what it needs to
know and leave the judgment to it — consistent with "the model owns control flow."

**Gotchas come from real use, not prediction.** Start a skill at a few lines plus
one real gotcha; append edge cases as the model trips on them. (Wiring usage
telemetry, §11, is what tells you which gotchas to add and which skills never
fire.)

**Start tiny.** Pick one thing you do repeatedly, write the minimal version, get
it loading on the right tasks, then grow it. Do not design the perfect skill up
front.

## 10. How it stays emergent

No new control flow. The loop already advertises tools and runs whatever the
model calls. The Skills index is just more advertised context; `load_skill` is
just another tool. The behavior — "recognize a Go task → load the Go skill →
follow it" — *emerges* from the model reading the index, exactly as backgrounding
a long test emerged once the prompt mentioned it (P3.9.b). The runtime adds a
registry and one tool; it makes no decisions.

## 11. Testing strategy

Offline, no model:

- `load_test.go`: a `skills/` fixture → `Load` parses frontmatter, `Index()`
  returns sorted `Meta`, `Get` returns the body; malformed frontmatter is skipped
  with a clear error, not a crash.
- `prompt_test.go`: the index section renders from a registry — names +
  descriptions, nothing else (assert no body text leaks into the base prompt).
- A `load_skill` tool test: unknown name errors; a known name returns the body.
- A loop test (fake model) that scripts `load_skill("x")` and asserts the body
  comes back as the tool result and is appended to history.

**Instrumentation (telemetry-driven, like the rest of P4).** `load_skill` is a
tool, so its calls already flow through the event stream — emit a `skill_loaded`
event (or reuse the tool-call trace) and aggregate per skill. The key signal,
straight from Anthropic's experience: **a skill that is rarely loaded usually has
a bad description, not a useless body.** Under-triggering → fix the description
first. This is the same "tune from real transcripts" loop we used for the
classifier and reflection.

## 12. Phasing

- **P6.a** — `internal/skills`: `Meta` / `Skill` / `Registry`, frontmatter
  parsing, `Index()` / `Get()`. Pure, table-tested. No loop change.
- **P6.b** — the `load_skill` tool + inject the index into the system prompt
  (`builder.go`) + register the tool. Wire it; one loop test.
- **P6.c** — author the 3 seed skills (`codeagent-conventions`, `verify-change`,
  `review-change` — §13.6) and watch the model load them on matching tasks (the
  real validation, like the background replays).
- **Future** — `list_skills`/search for when the index grows large; per-skill
  scoped tools (`tools:` frontmatter); L3 resources/scripts; skills authored from
  the agent's own learnings.

## 13. Open decisions (for sign-off)

1. **Load mechanism** — `load_skill` tool, model pulls (recommended) vs. runtime
   auto-injects by matching. Recommendation: **tool/model-pull** — keeps the
   decision with the model and the loop tool-agnostic.
2. **Index location** — a section in the base prompt (recommended for MVP, the
   model sees the menu) vs. a `list_skills` tool it must call first (saves base
   tokens, better at 100s of skills, but adds a round-trip and hurts
   discoverability). Recommendation: **index in the prompt now**, `list_skills` as
   the scale path.
3. **Scoped tools in MVP** — defer (recommended) vs. include a `tools:` field that
   restricts/grants tools while a skill is active. (Anthropic's on-demand hooks —
   e.g. a `/careful` skill that blocks `rm -rf`/force-push while active — are the
   richer version of this; they map naturally onto our `sandbox.CommandPolicy`,
   which a skill could tighten. Future.)
4. **L3 resources** — ship the **read-on-demand reference form now** (it is free:
   `SKILL.md` points at `references/*.md`, the model uses `read_file`). Defer only
   the formal `scripts/`/`assets/` execution convention. (Was "defer all".)
5. **Skill storage/format** — `skills/<name>/SKILL.md` **folder** with YAML
   frontmatter (`name`, `description`) + optional `references/`, mirroring
   Anthropic (recommended). A single file has no room for L3 and misses the point.
6. **Seed skills** — pick ones that *change behavior*, not ones that *supply
   information* (a template is not a behavior change). Signed-off set for P6.c:
   - `codeagent-conventions` — this repo's non-obvious rules + real gotchas (thin
     tool-agnostic loop, no-shell run_command, comment density, the
     `Co-Authored-By` trailer). Things Claude does *not* default to and got wrong
     here. *(Type 1.)*
   - `verify-change` — the behavior pattern we spent two weeks validating: change
     → verify (background if slow) → on failure fix the *source*, not the test →
     re-verify. This is the highest-leverage seed: a behavior, not knowledge.
     *(Type 2.)*
   - `review-change` — read the related code first; don't fix symptoms, fix the
     root cause; check side effects; check test coverage. A lightweight
     adversarial-review playbook. *(Type 6.)*
   *(Dropped `go-conventions` — common sense, §9. Dropped `commit-message` — too
   small, a template not a behavior change.)*
7. **Usage telemetry** — emit a `skill_loaded` event (carrying `name` + `version`)
   and aggregate per skill from day one (recommended), so under-triggering
   surfaces as a description problem early. Cheap; reuses the event stream.
8. **Distribution** — for this single repo, commit skills to `skills/` (or
   `./.claude/skills`). The internal-marketplace / `list_skills`-at-scale story
   (decision 2) is deferred until the index gets heavy.
9. **Skill versioning** — carry a `version` field on every skill from day one
   (recommended). It is ~free, and once `skill_loaded` telemetry exists you will
   want to compare trigger/success rates across `verify-change` v1 vs v2.

## 14. Skill taxonomy (Anthropic's 9 types), mapped to this repo

Anthropic sorts their hundreds of internal skills into nine kinds. Most assume a
big product/infra surface (data warehouses, on-call, deploys) we do not have.
The ones that fit a single coding-agent codebase, and where to start:

| Type | Fit here | What it would be for us |
|---|---|---|
| 1. Library / API reference | ✅ | This repo's non-obvious internals + gotchas (`codeagent-conventions`) |
| 2. Product validation | ✅✅ | A "verify a change" playbook — encodes the background-test + fix-source + re-verify habit we validated; highest-leverage given our P4 work |
| 5. Scaffolding / templates | ✅ | "new tool" — generate a `tools.Tool` skeleton (Name/Description/InputSchema/Execute) wired into buildRegistry |
| 6. Code quality / review | ✅ | Our review standards (we already have `/code-review`); could encode what to look for here |
| 4. Business automation | 🟡 | release notes / changelog from merged PRs — maybe later |
| 3 data, 7 CI/CD, 8 runbooks, 9 infra | ❌ | no warehouse / services / clusters to operate |

So the near-term skill space is **conventions, validation, scaffolding, review** —
and the signed-off P6.c seed set is `codeagent-conventions` (type 1),
`verify-change` (type 2), and `review-change` (type 6): one piece of
project-specific knowledge and two behavior playbooks, none of them common sense.
