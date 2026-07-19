# Phase 7 — Terminal UX Runtime (TUI workspace) — design PRD

> Status: design, signed off — **revised to inline mode (see the box below)**.
> The spec to hand a developer. Unlike the P4/P6 docs — which add *runtime data
> layers* — this phase adds **no agent capability at all**. It is a second
> *renderer* on the event stream the runtime already emits
> ([internal/agent/event.go](../internal/agent/event.go)). Companion to
> [docs/p4.1-observation.md](./p4.1-observation.md),
> [docs/p4.3-reflection.md](./p4.3-reflection.md), [docs/p6-skills.md](./p6-skills.md).

> ### ⚠ Architecture revision — inline mode (supersedes the alt-screen design)
>
> Driving the alt-screen build for real surfaced that the comfort users want
> (native copy, scroll, Ctrl+R search, ctrl+z, IME) comes from *not occupying the
> terminal*. So P7 pivoted from a **full-screen TUI** to an **inline workspace**:
> finalized events print to the terminal's own **scrollback**; the program owns
> only a small live region (status line + composer). This is "an enhanced
> terminal," the model Claude Code actually uses.
>
> What still holds below: the event seam (§3.2, §5), the EventStore (§7.1), the
> reducer + card formatters (§7), the composer (§9), the command palette (§11
> M5), the non-goals (§4). **What changed:** the `viewport` and §8's *in-place*
> Event-Collapse / cursor-nav / expand-collapse are gone (scrollback is
> immutable) — expansion is a print-time decision (failures print their body,
> successes their header). The async seam still applies, but `RunTurn` events are
> *printed* (`tea.Println`) rather than rendered into a viewport. Decisions §13.9
> (mouse off) and the inline note there are now **adopted**, not deferred.

## 1. Background

Claude Code's win was never the model. It was that you can sit in it for eight
hours and not get tired. That is a UX win, and it is now our largest gap: every
phase so far — Observation, Reflection, Skills (and the DreamAI / ai-engine work
around them) — raised the agent's *capability ceiling*. P7 is the first phase that
asks a different question entirely:

> **The capability is already enough. Why does no one want to use it every day?**

Those are two different problems. The answer to the second one is a mental model,
not a feature. Today `codeagent` is still:

```
> 修一下这个测试
thinking...
tool...
tool...
done
```

— i.e. **ChatGPT + tool calls: a command executor.** The job of P7 is to turn it
into a **Workspace**:

```
Workspace
 ├─ Context     (model · workspace · session · budget)
 ├─ Agent
 ├─ Timeline    (tools · skills · reflections · the reply — all events)
 └─ Composer    (a place to draft, edit, and send)
```

The current REPL ([cmd/codeagent/repl.go](../cmd/codeagent/repl.go)) is a
line-at-a-time scroll. It is built on `chzyer/readline` (not `bufio.Scanner`), and
multi-line paste is *already* handled — a bracketed-paste filter
([cmd/codeagent/paste.go](../cmd/codeagent/paste.go)) collapses a paste into
**one** input. So "100 lines → 100 turns" is **not** a bug we have. The real pains
are about the *mental model*, not the line reader:

| Pain | Today | Root cause |
|---|---|---|
| Can't see what the agent is *doing* | a flat wall of `[result]` / `[skill] loaded x` text | a chat stream, not a timeline |
| Can't edit a prompt before sending | paste collapses + sends; one readline line | no composer to draft in |
| Pasting code/PRD loses structure | newlines → spaces ([paste.go:73](../cmd/codeagent/paste.go#L73)) | no real editor; the filter flattens to protect readline |
| No sense of "where am I" | nothing persistent | no status header |
| Approval is a bare `y/N`, no preview | [internal/ui/approver.go](../internal/ui/approver.go) | text prompt, can't show the diff |
| Long tasks drown the user | every `read_file` printed in full | no event collapse |

None of these are runtime problems. They are all *rendering and input* problems —
which is exactly why P7 touches no agent code.

## 2. Goal

A TUI **agent workspace** you would choose to write code in all day — concretely,
one where the user can compose multi-line input, **watch the agent work** as a
structured timeline, always know where they are, copy output cleanly, and approve
side-effecting tools with a preview.

Shipped as a **new** entry point, `codeagent tui`. The existing REPL stays as
`codeagent repl` (headless, scriptable, CI/debug). Lowest-risk path: the new UX is
additive, the old one remains the golden reference.

## 3. North stars

Two, in order. The first is the *product* north star and **drives every design
decision below**; the second is the *architecture* guardrail that keeps the cost
near zero.

### 3.1 Product: **Timeline First, Chat Second**

> **P7 is not a chat window. P7 lets the user watch the agent work in real time.**

The user cares about **what the agent did**, not **what the agent said**:

```
✓ read_file      loop.go
✓ grep           "Emitter"
✓ verify-change  (skill)
✓ go test        ./...   → PASS
✓ review-change  (skill)

Assistant
  已修复问题…
```

Everything is a **timeline event** — tools, skills, reflections, compaction,
*and the assistant's reply*. The reply is just one event kind
(`EventTurnFinished`), rendered inline with the rest, not the frame that
everything else hangs off. This is the opposite of how TUIs are usually built
(chat transcript first, tool cards bolted on later) and it is the single most
important decision in this doc: **build the unified timeline first; chat is one
card in it.**

### 3.2 Architecture: **Change the renderer, not the agent**

The codebase already *proves* this is possible:

> The loop EMITS events and never writes to stdout. `consoleEmitter`
> ([console.go](../cmd/codeagent/console.go)) is one renderer; `liveProgress`
> ([live.go](../cmd/codeagent/live.go)) is a second one stacked on top — a pure
> decorator that adds the "Thinking… Ns" ticker with **zero** changes to loop,
> agent, or session. P3.7's acceptance test was literally *"swapping the renderer
> changes the UX without touching the loop."*

The TUI is a **third renderer** of the same `Event` stream, plus a second
implementation of the same `Approver` interface. The spine holds:

| | Must NOT happen | Must happen |
|---|---|---|
| The loop | gain a TUI/`tea` import or any UI branch | keep emitting plain `Event`s |
| `internal/agent` | know a TUI exists | unchanged |
| `Event` / `Approver` shapes | change signatures | stay the contract |
| Control flow | move into the UI | stay with the model |

If P7 ends with a diff inside `internal/agent`, we did it wrong.

## 4. Non-goals

- ❌ A chat transcript with tool cards bolted on. The timeline is primary; the
  assistant's text is one event kind, not the frame. (See §3.1.)
- ❌ A pixel-faithful Claude Code clone. We copy the *design philosophy*
  (low-fatigue, observable, timeline-first), not the layout.
- ❌ A web/Electron GUI. That is the "Later / GUI" roadmap item; this is a
  terminal app.
- ❌ Any change to the agent loop, observation, reflection, skills, or session.
- ❌ Multiplexing concurrent runs on screen. (The `Event`'s `SessionID`/`TurnID`
  already make this *possible* later; not now.)
- ❌ Mouse-first interaction. Keyboard-driven; mouse scroll is a bonus.
- ❌ Replacing the REPL. It stays as the headless path.
- ❌ Streaming token-by-token model output. (Separate roadmap item; the TUI
  renders whole `Thinking`/final texts as they arrive.)

## 5. The core architectural change (the crux)

The one non-trivial piece: **adopting BubbleTea forces the loop to run off the
main goroutine, which makes the first TUI commit an async-plumbing change — not
"just a textarea."**

Today everything is one synchronous flow on the main goroutine, and `readline` is
the *single owner of stdin* (the REPL's own comments stress this — two readers
would steal each other's bytes):

```
main goroutine:
  rl.Readline()            ← readline owns stdin
  runner.RunTurn(...)      ← BLOCKS the goroutine for the whole turn
    └─ Approver.Approve()  ← re-enters the SAME readline for y/N
  print final
```

BubbleTea must own stdin and the render loop on the main goroutine. So the turn
moves to a background goroutine and the two existing seams become channel-backed:

```
main goroutine (tea.Program):                  background goroutine:
  textarea / viewport / spinner                  runner.RunTurn(ctx, sess, input)
        ▲  events as tea.Msg                            │ emits Event ───────────┐
        └──────────── event channel ◄───────────────────┘                        │
        │  approval decision                       Approver.Approve() blocks      │
        └──────────── reply channel ─────────────► on a reply channel ◄───────────┘
```

Crucially, **neither interface signature changes.** `Emitter.Emit(Event)` and
`Approver.Approve(name, input) bool` stay exactly as they are; only their
*implementations* become channel-backed. The agent never learns any of this
happened.

`liveProgress` is **retired in TUI mode** — it writes raw ANSI (`\r…\033[K`)
straight to stdout, which fights BubbleTea's render loop. Its job is done by a
`bubbles/spinner` driven by `EventModelStarted` / `EventModelFinished`.
`consoleEmitter` + `liveProgress` stay for `repl` mode.

### 5.1 The channel Emitter

```go
// tuiEmitter forwards loop events into the BubbleTea program. Emit is called
// from the runner goroutine; the program consumes them as tea.Msg on the main
// goroutine. Buffered so a fast burst of events never blocks the loop.
type tuiEmitter struct{ ch chan agent.Event }

func (e tuiEmitter) Emit(ev agent.Event) { e.ch <- ev }

func waitForEvent(ch chan agent.Event) tea.Cmd {
    return func() tea.Msg { return eventMsg(<-ch) }
}
```

### 5.2 The channel Approver (M3)

```go
// tuiApprover turns a synchronous Approve() call (on the runner goroutine) into
// an async request the UI answers. It sends a request, then blocks on a reply
// channel — the loop pauses here exactly as it does on a readline y/N today.
type tuiApprover struct{ req chan approvalRequest }
type approvalRequest struct {
    tool  string
    input json.RawMessage
    reply chan bool          // ← the UI sends the user's decision here
}

func (a tuiApprover) Approve(tool string, input json.RawMessage) bool {
    reply := make(chan bool)
    a.req <- approvalRequest{tool, input, reply}
    return <-reply   // blocks the turn until the user answers — same semantics
}
```

## 6. Architecture

```
cmd/codeagent
   ├── repl.go     (unchanged path: readline + console/live emitter)
   └── tui/        (new package: the BubbleTea program)
         model.go        tea.Model: composer + timeline + (M2) header + (M3) approval
         emitter.go      tuiEmitter  (agent.Emitter, channel-backed)
         approver.go     tuiApprover (agent.Approver, channel-backed)
         timeline.go     the event→item reducer + collapse (§7, §8)
         view.go         lipgloss rendering: items, cards, layout
         keys.go         keymap (bubbles/key + help)
                 │
                 ▼  agent.Event over a channel  /  approval over a channel
            agent.Runner (UNCHANGED) ── RunTurn on a background goroutine
```

The runner is constructed exactly as in `runAgent`/`repl` (same `buildRegistry`,
`buildCompactor`, session builder) — only `Emitter` and `Approver` differ. Factor
that construction into a shared `buildRunner(...)` so `run`, `repl`, and `tui`
cannot drift apart.

## 7. Everything is a timeline event

The timeline is a list of **items** reduced from the `Event` stream. *Every* kind
of output — including the assistant's reply — is an item; nothing is privileged.
All the data already exists on `Event` ([event.go](../internal/agent/event.go)):

| Timeline item | Event(s) | Fields used |
|---|---|---|
| Thinking live preview | `EventReasoningDelta` | `Text`（瞬态 append） |
| Thinking snapshot | `EventThinking` | `Text`（持久化 replace） |
| Tool (header) | `EventToolStarted` | `Step`, `ToolName`, `ToolArgs` |
| Tool ✓/✗ status | `EventObserved` | `Failure` (`""`/`none` ⇒ ✓; else ✗ + label) |
| Tool body/result | `EventToolFinished` | `Observation` |
| Skill | `EventSkillLoaded` | `ToolName` (name), `Version` |
| Reflection | `EventReflected` | `Text` |
| Compaction | `EventCompacted` | `Before/After/Saved/Ratio/SummaryChars` |
| **Assistant reply** | `EventTurnFinished` | `Text` — *one item, not the frame* |
| Spinner on/off | `EventModelStarted` / `EventModelFinished` | `Elapsed` |
| Header: context budget (M2) | (session) | `sess.PromptTokens` / `sess.CompactThreshold` ([repl.go:174](../cmd/codeagent/repl.go#L174)) |

**Correlation by `Step`.** A tool emits `EventToolStarted` → `EventObserved` →
`EventToolFinished` in order, all carrying the same `Step`. The reducer keys open
tool items by `Step` and folds the later events into the same item (status from
`Observed`, body from `Finished`). This is the one piece of stateful view logic;
everything else is append-render.

### 7.1 Two layers: the domain event vs the timeline projection (the EventStore)

A tempting mistake is to treat the reducer's output as the durable "timeline
model". It is not — it is a **lossy fold**: `Collapse` merges N reads into one
group, and the tool reducer merges `started + observed + finished` into one item.
You cannot faithfully replay, search, or analyze from folded items.

The **domain event already exists**: `agent.Event`. Its own doc comment says it is
"plain data … so it can be rendered, logged, or sent over a wire unchanged," and
`Runner.emit` stamps every one with `At`, `SessionID`, `TurnID`. So the layering
is:

```
agent.Event  ──persist──▶  session_events           DOMAIN: raw, 1:1, replayable
     │                     (EventStore — the truth)
     └─reduce / fold──▶  []Item (timeline)           PROJECTION: TUI today, web tomorrow
```

- **EventStore (shipped with M1).** A `session_events` table (`id, session_id,
  turn_id, kind, at, payload`) plus `RecordEvent` / `SessionEvents` on the store.
  Events are persisted by `eventStoreEmitter` — a composable *decorator* (the same
  shape as `liveProgress`) that writes the event, then forwards it to the renderer
  unchanged. Best-effort, like `requestObserver`. `run` / `repl` / `tui` all wrap
  their renderer with it via `withEventStore`, so every entry point logs the same
  stream. This is the foundation for **timeline replay, search, analytics,
  export** — all of which need the raw stream, not the projection.
- **Projection (the reducer).** Stays pure and presentation-only — and a *shared*
  asset: a future web UI consumes the same `agent.Event` stream over a socket and
  runs the same reducer. Its items now carry a stable `ID` and `StartedAt`/
  `EndedAt` (tool **durations**), so a card can be addressed/expanded and, later,
  matched to a replay frame. It is a flattened discriminated union mirroring
  `agent.Event` — deliberately **not** `Payload any` (no type-assertion churn in
  renderers, full compile-time safety).

> Why `agent.Event` and not a new `TimelineEvent` type: the runtime already owns
> the canonical, serializable event. Adding a parallel domain type would mean two
> sources of truth to keep in sync. Replay re-feeds persisted `agent.Event`s
> through the same reducer — one event type, one reducer, two (and counting)
> renderers.

## 8. Timeline compaction (Event Collapse)

A long task explodes the timeline:

```
read_file  read_file  read_file  read_file  read_file  read_file   …
```

So the timeline **collapses runs of same-kind items** into one summary line,
expandable on demand — distinct from §12's *virtualization* (which is a perf
concern: don't hold 10k items in memory). Collapse is a *comprehension* concern:
don't make the human scroll past twelve identical lines to see what happened.

```
▸ ✓ Read 6 files            (Enter to expand)
        loop.go  event.go  session.go  …
▸ ✓ Ran 8 tests   → PASS
▸ ✓ Loaded 2 skills         verify-change · review-change
```

Rules (kept simple for MVP):

- Adjacent successful items of the same kind collapse: N×`read_file`,
  N×`grep`, N×`run_command`(test), N×`skill_loaded`.
- A **failure never collapses** — a `✗` item always shows on its own (it is the
  signal; §7's `EventObserved.Failure` drives this).
- The assistant reply, reflections, and compaction never collapse.
- Collapsed groups are expandable (`Enter`/`→`); the newest group defaults open.

This is what keeps a 200-step run readable. It lands in M2, with the cards.

## 9. Composer — edit, not just paste

The real problem the composer solves is **not** paste (already handled) and not
even newline preservation — it is that **the user never gets to edit before
sending.** Real use is:

```
paste code/log/PRD  →  delete a few lines  →  add a sentence of intent  →  send
```

Today there is no "before sending." So a `bubbles/textarea` (a real multi-line
editor that preserves newlines and lets you move around and revise) is worth far
more than the narrow "keep newlines on paste" fix. That is the M1 win.

Send semantics need an explicit call (see §13.2): the default is **`Enter` =
newline (you are editing), an explicit key = send**, made a keybinding and
documented, because an edit-first composer should not fire on every `Enter`.

## 10. Components / tech

Standard Charm stack — no wheel-reinvention:

| Concern | Library |
|---|---|
| Program / event loop | `bubbletea` |
| Multi-line composer | `bubbles/textarea` |
| Scrollable timeline | `bubbles/viewport` |
| Model-call spinner | `bubbles/spinner` |
| Keymap + help bar | `bubbles/key`, `bubbles/help` |
| Styling / cards / layout | `lipgloss` |

New `go.mod` deps: `bubbletea`, `bubbles`, `lipgloss` (none present today —
go.mod has only readline, yaml, sqlite).

## 11. Phasing — five milestones, each a felt jump

Re-cut per sign-off. Each milestone is named by **what the user can finally do**.
Note the key change: **the Header moves out of M1 into M2** — it solves no core
pain, and M1 must be the smallest thing that is genuinely usable.

| | Milestone | The user can finally… | Contents |
|---|---|---|---|
| **M1** | Workspace Skeleton | **use it** | BubbleTea program · async seam (§5: channel `Emitter` + `Approver`, background `RunTurn`) · a **minimal y/n approval footer** (auto-approving file writes would be unsafe; the full Approval *card* + diff preview stay M3) · `textarea` composer (§9) · the unified **timeline** (§7) with Event Collapse (§8) |
| **M2** | Structured Events | **understand it** | Tool cards (✓/✗ + duration + **expandable** result; failures show their body unprompted) · Skill cards · Reflection cards · **Event Collapse** (§8, expandable to members) · the session **Header** (live context gauge + skills) · an **inspection mode**: `Tab` moves focus to the timeline, where ↑/↓ move a cursor and `Enter` expands/collapses the focused card |
| **M3** | Approval UX | **trust it** | the channel `Approver` (§5.2) wired to an Approval card · `y`/`n`/`v(preview)` · a diff/preview renderer (fills the empty `internal/ui/diff.go`) |
| **M4** | Workspace Awareness | **feel it's an IDE** | Git panel (branch · modified/untracked) · Files panel · Context-budget gauge |
| **M5** | Power User | **not live without it** | Command palette (`/help /model /skills /context /compact /events /reset`, reusing `handleCommand`) · search · keyboard-shortcut help |

**M1 Done when:** you can run a full multi-tool turn in `codeagent tui`, draft and
edit a multi-line prompt before sending, and watch tools/skills/reflections and
the reply scroll as **one timeline** — even before any of it is a pretty card.

Cross-cutting perf note: raw transcript **virtualization** (a bounded ring buffer
so a 1k-message session stays cheap) rides along with M4/M5; `viewport` already
windows the *view*, so this is about bounding retained *items*, not rendering.

## 12. Testing strategy

The whole point of the seam is that the UX is testable *without* a terminal or a
model:

- **Channel `Emitter` / `Approver`**: unit-test that `Emit` enqueues and `Approve`
  blocks until a reply is sent — pure Go, no TUI.
- **The timeline reducer (§7, §8)**: feed a scripted `[]Event` and assert the item
  list — tool items keyed/merged by `Step`, runs collapsed, failures never
  collapsed, the reply as its own item. This is the highest-value test and needs
  no BubbleTea at all.
- **`tea.Model.Update`**: feed `eventMsg`s and assert model state (spinner on
  between model start/finish, approval card shown on a request). Models are plain
  `Update(msg) (Model, Cmd)` functions — table-testable; `teatest` for a
  golden-frame render if wanted.
- **`buildRunner` parity**: one test that `run`, `repl`, `tui` build an identical
  runner except for `Emitter`/`Approver`.
- **The REPL stays the headless golden path** — its existing tests
  ([repl_test.go](../cmd/codeagent/repl_test.go), [live_test.go](../cmd/codeagent/live_test.go),
  [paste_test.go](../cmd/codeagent/paste_test.go)) keep guarding the contract the
  TUI also consumes.

## 13. Open decisions (recommendations; flag any to revisit)

1. **Entry point** — new `codeagent tui`, keep `repl` (recommended) vs. make TUI
   default with `--plain`. Reco: **separate subcommand** now; flip later once
   proven.
2. **Send key** — `Ctrl+Enter` is ideal but **many terminals can't distinguish it
   from `Enter`**. For an edit-first composer (§9), Reco: **`Enter` = newline,
   `Alt+Enter` (or a configurable send key) = send**, made a keybinding and
   documented. This is the single most-felt interaction — calling it out for an
   explicit nod.
3. **Approval allow-always** — ship `y`/`n`/`v` in M3; add session-scoped
   "allow-always (this tool)" as a fast follow (maps onto `sandbox` policy, not
   the UI).
4. **Spinner vs. liveProgress** — retire `liveProgress` in TUI, use
   `bubbles/spinner` (recommended). `repl` keeps `liveProgress`.
5. **Timeline persistence** — ephemeral timeline, rebuilt from the loaded
   session's messages on resume; don't persist rendered items (the session store
   already persists the messages). Recommended.
6. **Collapse policy** — collapse adjacent same-kind *successes*, never failures,
   newest group open (§8). Recommended as the MVP rule; tune from real runs.
7. **Code location** — `cmd/codeagent/tui/` package (recommended) so `main.go`
   only dispatches.
8. **Color / theme** — `lipgloss` adaptive colors honoring `NO_COLOR` and light/
   dark terminals (recommended); no config knob in MVP.
9. **Mouse capture — off** (decided on first live use). Capturing the mouse
   (`WithMouseCellMotion`) blocks the terminal's own text selection; for a coding
   agent, copying output matters more than wheel-scroll. Scroll is keyboard
   (pgup/pgdn, Tab+↑/↓). A later "inline (no alt-screen)" mode — like Claude
   Code's transcript-in-scrollback — would restore native scroll+copy together,
   but that is a bigger rearchitecture, deferred.
10. **Composer at the bottom** (decided on first live use). It is the last element
    so the cursor sits near the terminal's bottom edge — a CJK IME candidate
    window then opens over the composer's own rows, not over the help/timeline.
    Full control of IME popup placement is not possible from a TUI; this is a
    mitigation.
11. **`ctrl+z` suspends** (job control, via `tea.Suspend`); `ctrl+c` quits.

## 14. Why this is worth doing now

Every recent phase — Observation, Reflection, Skills — raised the agent's
capability. P7 is the first to work on the **Human ↔ Agent interface**, and that
is precisely the axis that decides whether an agent crosses from "impressive demo"
to "the thing I open every morning." The same insight that made `liveProgress` a
90-line decorator means the entire TUI is a *consumer* of infrastructure that
already exists and is already tested; the risk is concentrated in one place (the
async seam, §5), it is well understood, and the old REPL stays as a safety net.
The payoff is the first time you'd *choose* to write DreamAI inside `codeagent` —
which, given the capabilities already shipped, is plausibly a bigger felt jump
than Skills, Reflection, and Observation combined.
