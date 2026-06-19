// Package tui is the BubbleTea "agent workspace" renderer (Phase 7, M1). It is a
// second consumer of the runtime's existing event stream and Approver interface —
// it adds no agent capability and the agent never learns it exists. The loop runs
// on a background goroutine; events and approvals cross to the render loop over
// channels (see backend.go and docs/p7-tui.md §5).
package tui

import (
	"context"
	"encoding/json"

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
}

// Run starts the workspace. The runner must already be wired to b.Emitter and
// b.Approver (via buildRunner). One background goroutine pulls a prompt off
// b.inputs, runs the turn, persists, and signals done — and handles
// session/model swaps between turns. The BubbleTea program owns the terminal.
// Run blocks until the user quits.
func Run(ctx context.Context, b *Backend, runner *agent.Runner, sess *session.Session, store Store, header HeaderInfo, resume ResumeFunc, modelSwap ModelSwapFunc, modelNames []string) error {
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
