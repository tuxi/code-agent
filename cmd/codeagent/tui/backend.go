package tui

import (
	"context"
	"encoding/json"
	"sync"

	"code-agent/internal/agent"
	"code-agent/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// Backend is the channel seam between the agent runner (which runs on a
// background goroutine) and the BubbleTea program (which owns stdin and the
// render loop on the main goroutine). It carries the two existing interfaces —
// Emitter and Approver — as channel-backed implementations. Neither interface
// signature changes; only where the bytes go (§5).
//
// Construct it, hand Emitter/Approver to buildRunner, then pass it to Run.
type Backend struct {
	Emitter         agent.Emitter
	Approver        agent.Approver
	PlanApprover    agent.PlanApprover
	AskUserApprover agent.AskUserApprover

	events          chan agent.Event
	approvals       chan approvalReq
	inputs          chan string
	done            chan error
	sessSwap        chan *session.Session // /resume hands the run loop a new session
	modelSwap       chan string           // /use: TUI → run loop (model name to switch to)
	modelSwapResult chan modelSwappedMsg  // /use: run loop → TUI (result)
	planToggle      chan bool             // plan key: TUI → run loop (desired plan mode)
	planApprovals   chan planApprovalReq  // plan approval: run loop → TUI (blocking, like approvals)
	askUsers        chan askUserReq       // ask_user: run loop → TUI (blocking, like plan approvals)
	goalStart       chan string           // /goal: TUI → run loop (objective to pursue; "" resumes)
	goalDone        chan goalDoneMsg      // /goal: run loop → TUI (outcome summary)
	goalCtl         chan goalCtlReq       // /goal status|clear: TUI → run loop (quick, reply-back)

	mu         sync.Mutex
	turnCancel context.CancelFunc // set by the run loop before RunTurn; nil when idle
}

// CancelTurn cancels the in-flight turn (if any). The run loop saves the partial
// session and signals done — the TUI stays alive, the conversation is preserved.
func (b *Backend) CancelTurn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turnCancel != nil {
		b.turnCancel()
	}
}

// NewBackend wires the channels. events is buffered so a fast burst from the loop
// never blocks it; inputs has room for one queued prompt so submitting never
// blocks the UI; approvals is unbuffered (the loop must wait for an answer); done
// signals turn completion (even on error, where no EventTurnFinished is emitted);
// modelSwap is buffered (cap 1) so the UI never blocks posting a model name.
func NewBackend() *Backend {
	events := make(chan agent.Event, 256)
	approvals := make(chan approvalReq)
	inputs := make(chan string, 1)
	done := make(chan error, 1)
	sessSwap := make(chan *session.Session, 1)
	mSwap := make(chan string, 1)
	mSwapResult := make(chan modelSwappedMsg, 1)
	planToggle := make(chan bool, 1)
	planApprovals := make(chan planApprovalReq)
	askUsers := make(chan askUserReq)
	goalStart := make(chan string, 1)
	goalDone := make(chan goalDoneMsg, 1)
	goalCtl := make(chan goalCtlReq)
	return &Backend{
		Emitter:         tuiEmitter{ch: events},
		Approver:        tuiApprover{ch: approvals},
		PlanApprover:    &tuiPlanApprover{ch: planApprovals},
		AskUserApprover: &tuiAskUserApprover{ch: askUsers},
		events:          events,
		approvals:       approvals,
		inputs:          inputs,
		done:            done,
		sessSwap:        sessSwap,
		modelSwap:       mSwap,
		modelSwapResult: mSwapResult,
		planToggle:      planToggle,
		planApprovals:   planApprovals,
		askUsers:        askUsers,
		goalStart:       goalStart,
		goalDone:        goalDone,
		goalCtl:         goalCtl,
	}
}

// goalDoneMsg carries a finished /goal pursuit's one-line outcome (and error) to
// the model — the goal analogue of doneMsg, on its own channel so a pursuit and a
// plain turn never cross wires.
type goalDoneMsg struct {
	summary string
	err     error
}

func waitForGoalDone(ch chan goalDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// goal control ops are quick, between-turns requests (status/clear) that reply on
// a per-request channel — distinct from goalStart/goalDone, which run a long pursuit.
const (
	ctlStatus = iota
	ctlClear
)

type goalCtlReq struct {
	kind  int
	reply chan string
}

// goalCtlResultMsg carries a status/clear reply back into the model for printing.
type goalCtlResultMsg string

// modelSwappedMsg carries the result of a /use model switch so the TUI can update
// its header and gauge. nil header means the switch failed (err is set).
type modelSwappedMsg struct {
	header HeaderInfo
	err    error
}

func waitForModelSwapResult(ch chan modelSwappedMsg) tea.Cmd {
	return func() tea.Msg { m, _ := <-ch; return m }
}

// doneMsg signals that a turn finished (err non-nil if it failed). It is the
// single source of "the composer is free again" — robust to the error path,
// where the loop returns without an EventTurnFinished.
type doneMsg struct{ err error }

func waitForDone(ch chan error) tea.Cmd {
	return func() tea.Msg {
		err, ok := <-ch
		if !ok {
			return nil
		}
		return doneMsg{err: err}
	}
}

// --- Emitter -------------------------------------------------------------

// eventMsg carries one agent event into the BubbleTea update loop.
type eventMsg agent.Event

// tuiEmitter forwards loop events to the program over a buffered channel. Emit is
// called on the runner goroutine; the program drains them as eventMsg on the main
// goroutine. (Same decoupling as consoleEmitter — see cmd/codeagent/live.go.)
type tuiEmitter struct{ ch chan agent.Event }

func (e tuiEmitter) Emit(ev agent.Event) { e.ch <- ev }

// waitForEvent blocks for the next event and delivers it as an eventMsg. The
// Update loop re-issues it after each event to keep draining.
func waitForEvent(ch chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg(ev)
	}
}

// --- Approver ------------------------------------------------------------

// approvalReq is a side-effecting tool awaiting the user's decision. The runner
// goroutine blocks on reply until the UI answers — the same pause the REPL gets
// from a readline y/N, but async now that the render loop owns stdin (§5.2).
type approvalReq struct {
	tool  string
	input json.RawMessage
	reply chan agent.Verdict
}

type approvalMsg approvalReq

// promptRenderedMsg carries the result of rendering an MCP prompt template
// (/mcp__server__prompt) off the UI goroutine back to Update, which then runs the
// text as a turn (or reports the error).
type promptRenderedMsg struct {
	text string
	err  error
}

// tuiApprover is the agent.Approver for the TUI: Approve runs on the runner
// goroutine, posts a request, and blocks until the UI sends a decision back.
type tuiApprover struct{ ch chan approvalReq }

func (a tuiApprover) Approve(tool string, input json.RawMessage) agent.Verdict {
	reply := make(chan agent.Verdict, 1)
	a.ch <- approvalReq{tool: tool, input: input, reply: reply}
	return <-reply
}

func waitForApproval(ch chan approvalReq) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return approvalMsg(req)
	}
}

// --- Plan Approver -------------------------------------------------------

// planApprovalReq carries a proposed plan to the TUI for a human decision.
// The runner goroutine blocks on reply until the UI answers.
type planApprovalReq struct {
	plan  agent.Plan
	reply chan agent.PlanDecision
}

type planApprovalMsg planApprovalReq

// tuiPlanApprover implements agent.PlanApprover for the TUI. ApprovePlan runs
// on the runner goroutine, posts the plan, and blocks until the UI answers.
type tuiPlanApprover struct{ ch chan planApprovalReq }

func (a *tuiPlanApprover) ApprovePlan(plan agent.Plan) agent.PlanDecision {
	reply := make(chan agent.PlanDecision, 1)
	a.ch <- planApprovalReq{plan: plan, reply: reply}
	return <-reply
}

func waitForPlanApproval(ch chan planApprovalReq) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return planApprovalMsg(req)
	}
}

// --- AskUser Approver ------------------------------------------------------

// askUserReq carries a clarification question to the TUI for a human decision.
// The runner goroutine blocks on reply until the UI answers.
type askUserReq struct {
	q     agent.AskUserQuestion
	reply chan agent.AskUserAnswer
}

type askUserMsg askUserReq

// tuiAskUserApprover implements agent.AskUserApprover for the TUI. AskUser runs
// on the runner goroutine, posts the question, and blocks until the UI answers.
type tuiAskUserApprover struct{ ch chan askUserReq }

func (a *tuiAskUserApprover) AskUser(q agent.AskUserQuestion) (agent.AskUserAnswer, error) {
	reply := make(chan agent.AskUserAnswer, 1)
	a.ch <- askUserReq{q: q, reply: reply}
	return <-reply, nil
}

func waitForAskUser(ch chan askUserReq) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return askUserMsg(req)
	}
}
