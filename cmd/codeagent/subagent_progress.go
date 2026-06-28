package main

import (
	"code-agent/internal/agent"
	"code-agent/internal/runtime"
	"fmt"
	"io"
	"os"
)

// taskProgress renders a single, in-place-updating heartbeat for a running
// subagent, so a `task` call is not a silent black box — WITHOUT echoing the
// subagent's full (default-quiet) transcript. It collapses the sub-runner's
// tool-start events onto one line that overwrites itself, and erases it when the
// delegation finishes so the parent's next output starts clean.
//
// Single-goroutine by construction: in run/repl the nested subagent turn runs
// inline on the main goroutine, so emits arrive sequentially. The TUI passes nil
// (it owns the screen); a TUI sub-stream is a separate follow-on.
type taskProgress struct {
	w      io.Writer
	active bool
	step   int    // model calls so far — the loop ITERATION count, which the budget bounds
	tool   string // the tool the current iteration is running, if any
}

func newTaskProgress(w io.Writer) *taskProgress { return &taskProgress{w: w} }

func (p *taskProgress) Emit(e agent.Event) {
	switch e.Kind {
	case agent.EventTaskStarted:
		p.step, p.tool = 0, ""
		p.show("⟳ subagent starting…")
	case agent.EventModelStarted:
		// One model call == one loop iteration; this is the unit runtime.SubAgentMaxSteps
		// bounds. Counting it (not EventToolStarted.Step, which is the cumulative
		// tool-call ordinal and batches many-per-iteration) keeps the heartbeat's
		// "step N/M" honest against the budget.
		p.step++
		p.render()
	case agent.EventToolStarted:
		p.tool = e.ToolName
		p.render()
	case agent.EventTaskFinished:
		p.erase()
	}
}

func (p *taskProgress) render() {
	s := fmt.Sprintf("⟳ subagent · step %d/%d", p.step, runtime.SubAgentMaxSteps)
	if p.tool != "" {
		s += " · " + p.tool
	}
	p.show(s)
}

// show overwrites the current line (\r + clear-to-end) with s.
func (p *taskProgress) show(s string) {
	fmt.Fprintf(p.w, "\r\x1b[K%s", s)
	p.active = true
}

// erase clears the heartbeat line so the parent's next output starts clean.
func (p *taskProgress) erase() {
	if p.active {
		fmt.Fprint(p.w, "\r\x1b[K")
		p.active = false
	}
}

// subagentProgress returns the heartbeat renderer for run/repl when stdout is a
// terminal (the \r rewrites would be noise in a file or pipe), else nil. The TUI
// passes nil directly — raw writes would corrupt its alt-screen.
func subagentProgress() agent.Emitter {
	if isTTY(os.Stdout) {
		return newTaskProgress(os.Stdout)
	}
	return nil
}
