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

// sessionSource is the cmd-layer's window into saved sessions for /sessions and
// /resume: list them, load+rebudget one, and replay one's events.
type sessionSource struct {
	list   func() []session.Meta
	resume ResumeFunc
	events func(id string) []agent.Event
}

// Run starts the workspace. The runner must already be wired to b.Emitter and
// b.Approver (via buildRunner). One background goroutine pulls a prompt off
// b.inputs, runs the turn, persists, and signals done — and swaps the session on
// /resume. The BubbleTea program owns the terminal. Run blocks until the user
// quits.
func Run(ctx context.Context, b *Backend, runner *agent.Runner, sess *session.Session, store Store, header HeaderInfo, resume ResumeFunc) error {
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
				_, err := runner.RunTurn(ctx, sess, input)
				// Persist whatever the turn produced, even on error: the partial
				// history is consistent and resumable (same contract as run/repl).
				if serr := store.Save(ctx, sess); serr != nil && err == nil {
					err = serr
				}
				b.done <- err
			case ns := <-b.swap:
				// /resume swaps the active session between turns. The select can't
				// fire mid-RunTurn, so a swap always lands at a turn boundary — no
				// hot-swap of a running turn. Persist the outgoing session first.
				if !sess.IsEmpty() {
					_ = store.Save(ctx, sess)
				}
				sess = ns
			}
		}
	}()

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
