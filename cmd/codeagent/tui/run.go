// Package tui is the BubbleTea "agent workspace" renderer (Phase 7, M1). It is a
// second consumer of the runtime's existing event stream and Approver interface —
// it adds no agent capability and the agent never learns it exists. The loop runs
// on a background goroutine; events and approvals cross to the render loop over
// channels (see backend.go and docs/p7-tui.md §5).
package tui

import (
	"context"
	"encoding/json"
	"errors"

	"code-agent/internal/agent"
	"code-agent/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// Store is the slice of the session store the TUI needs: persist after each turn,
// list saved sessions (for /sessions and the /resume picker), and read a
// session's persisted events (to replay its history on resume).
type Store interface {
	Save(ctx context.Context, s *session.Session) error
	List(ctx context.Context) ([]session.Meta, error)
	SessionEvents(ctx context.Context, id string) ([]session.EventRecord, error)
}

// ResumeFunc loads a saved session by id and re-budgets it to the current model
// (provided by the cmd layer, which owns the config). Used by /resume.
type ResumeFunc func(id string) (*session.Session, error)

// AutoMode is the cmd layer's auto-approval toggle, surfaced to the /auto command.
// Satisfied by *approve.AutoApprover; kept as an interface so the tui package does
// not import the approve package.
type AutoMode interface {
	Enabled() bool
	SetEnabled(bool)
}

// PermissionGranter persists an "always allow" grant made from the approval card,
// so future matching calls skip the prompt. Satisfied by *approve.RuleStore; kept
// as a local interface so the tui package does not import the approve package
// (mirrors AutoMode). Nil means the card offers only allow-once / deny.
type PermissionGranter interface {
	AllowAlways(toolName string) (rule string, err error)
}

// GoalOps is the cmd layer's /goal capability injected into the run loop (it owns
// admission and engine/checker construction, so the tui package stays decoupled).
// None of its methods print — the model renders the returned strings. A nil GoalOps
// disables /goal in the TUI.
type GoalOps interface {
	// Pursue starts a new goal (objective != "") or resumes the session's existing
	// goal (objective == ""), runs it to a terminal state, and returns a one-line
	// outcome. It runs like a long turn — events and approval cards flow through the
	// existing channels.
	Pursue(ctx context.Context, sess *session.Session, objective string) (summary string, err error)
	// Status formats the session's current goal (for /goal with no args).
	Status(sess *session.Session) string
	// Clear drops the session's goal and persists.
	Clear(ctx context.Context, sess *session.Session) error
	// Finalize applies the end-of-pursuit policy (achieved → archive + auto-clear).
	// Called after a pursuit returns; a no-op for resumable terminals.
	Finalize(ctx context.Context, sess *session.Session)
}

// ModelSwapFunc switches the runner to a new model by name, rebuilding the
// provider/compactor and re-budgeting the session. Called inside the run-loop
// goroutine between turns (the same select safety as /resume). Returns the
// updated header info for the TUI, or an error.
type ModelSwapFunc func(name string) (HeaderInfo, error)

// modelInfo is one selectable model for the /use picker.
type modelInfo struct {
	name string
}

// sessionSource is the cmd-layer's window into saved sessions for /sessions and
// /resume: list them, load+rebudget one, and replay one's events.
type sessionSource struct {
	list   func() []session.Meta
	resume ResumeFunc
	events func(id string) []agent.Event

	modelNames   []modelInfo          // for the /use picker
	modelSwap    ModelSwapFunc        // switches the model between turns
	modelSwapped chan modelSwappedMsg // the TUI awaits this after posting a model name

	auto    AutoMode          // /auto toggle (nil if not wired)
	granter PermissionGranter // "always allow" from the approval card (nil if not wired)
}

// Run starts the workspace. The runner must already be wired to b.Emitter and
// b.Approver (via buildRunner). One background goroutine pulls a prompt off
// b.inputs, runs the turn, persists, and signals done — and handles
// session/model swaps between turns. The BubbleTea program owns the terminal.
// Run blocks until the user quits.
func Run(ctx context.Context, b *Backend, runner *agent.Runner, sess *session.Session, store Store, header HeaderInfo, resume ResumeFunc, modelSwap ModelSwapFunc, modelNames []string, auto AutoMode, granter PermissionGranter, goalOps GoalOps) error {
	// Cancelling on quit stops an in-flight turn rather than orphaning it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		for {
			select {
			case input, ok := <-b.inputs:
				if !ok {
					return
				}
				turnCtx, cancel := context.WithCancel(ctx)
				b.mu.Lock()
				b.turnCancel = cancel
				b.mu.Unlock()
				_, err := runner.RunTurn(turnCtx, sess, input)
				cancel() // always clean up
				b.mu.Lock()
				b.turnCancel = nil
				b.mu.Unlock()
				// Persist whatever the turn produced, even on error (including
				// context.Canceled from ctrl+c): the partial history is consistent
				// and resumable (same contract as run/repl).
				if serr := store.Save(ctx, sess); serr != nil && err == nil {
					err = serr
				}
				b.done <- err
			case ns := <-b.sessSwap:
				// /resume swaps the active session between turns. The select can't
				// fire mid-RunTurn, so a swap always lands at a turn boundary — no
				// hot-swap of a running turn. Persist the outgoing session first.
				if !sess.IsEmpty() {
					_ = store.Save(ctx, sess)
				}
				sess = ns
			case name := <-b.modelSwap:
				h, err := modelSwap(name)
				b.modelSwapResult <- modelSwappedMsg{header: h, err: err}
			case on := <-b.planToggle:
				// Applied at a turn boundary (the select can't fire mid-RunTurn), so
				// the next turn runs in the chosen mode — no hot-swap of a live turn.
				if on {
					runner.PlanState = agent.PlanStatusPlanning
				} else {
					runner.PlanState = agent.PlanStatusNone
				}
			case obj := <-b.goalStart:
				// A /goal pursuit is a long, multi-turn turn: it drives runner.RunTurn
				// repeatedly via the injected engine, so events and approval cards flow
				// through the same channels. turnCancel cancels the whole pursuit, so
				// ctrl+c pauses it (the engine settles to paused). The engine is the sole
				// session writer, so no Save here. obj == "" resumes the existing goal.
				var summary string
				var err error
				if goalOps == nil {
					err = errors.New("/goal is not available in this session")
				} else {
					turnCtx, cancel := context.WithCancel(ctx)
					b.mu.Lock()
					b.turnCancel = cancel
					b.mu.Unlock()
					summary, err = goalOps.Pursue(turnCtx, sess, obj)
					cancel()
					b.mu.Lock()
					b.turnCancel = nil
					b.mu.Unlock()
					if err == nil {
						goalOps.Finalize(ctx, sess) // achieved → archive + auto-clear
					}
				}
				b.goalDone <- goalDoneMsg{summary: summary, err: err}
			case req := <-b.goalCtl:
				// Quick, between-turns goal ops (status/clear). The model only sends
				// these when idle, so the select handles them immediately.
				switch {
				case goalOps == nil:
					req.reply <- "/goal is not available in this session"
				case req.kind == ctlStatus:
					req.reply <- goalOps.Status(sess)
				case req.kind == ctlClear:
					if err := goalOps.Clear(ctx, sess); err != nil {
						req.reply <- "clear failed: " + err.Error()
					} else {
						req.reply <- "goal cleared."
					}
				}
			}
		}
	}()

	models := make([]modelInfo, len(modelNames))
	for i, n := range modelNames {
		models[i] = modelInfo{name: n}
	}
	src := sessionSource{
		list: func() []session.Meta {
			metas, err := store.List(ctx)
			if err != nil {
				return nil
			}
			return metas
		},
		resume: resume,
		events: func(id string) []agent.Event {
			recs, err := store.SessionEvents(ctx, id)
			if err != nil {
				return nil
			}
			out := make([]agent.Event, 0, len(recs))
			for _, r := range recs {
				var ev agent.Event
				if json.Unmarshal(r.Payload, &ev) == nil {
					out = append(out, ev)
				}
			}
			return out
		},
		modelNames:   models,
		modelSwap:    modelSwap,
		modelSwapped: b.modelSwapResult,
		auto:         auto,
		granter:      granter,
	}
	// Inline mode (no alt-screen, no mouse capture): finalized output goes to the
	// terminal's own scrollback, so native select/copy, scroll, and Ctrl+R search
	// all just work. The program only owns the live region (status + composer).
	p := tea.NewProgram(
		newModel(b, header, src),
		tea.WithContext(ctx),
	)
	_, err := p.Run()
	close(b.inputs) // stop the turn loop; in-flight turn is cancelled via ctx
	return err
}
