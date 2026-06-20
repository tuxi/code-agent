package main

import (
	"code-agent/internal/agent"
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
}

func newTaskProgress(w io.Writer) *taskProgress { return &taskProgress{w: w} }

func (p *taskProgress) Emit(e agent.Event) {
	switch e.Kind {
	case agent.EventTaskStarted:
		p.show("⟳ subagent starting…")
	case agent.EventToolStarted:
		p.show(fmt.Sprintf("⟳ subagent · step %d · %s", e.Step, e.ToolName))
	case agent.EventTaskFinished:
		p.erase()
	}
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

// multiEmitter fans one event out to several emitters — e.g. persist the
// subagent's transcript AND show a live heartbeat from the same stream. Nil
// entries are skipped, so callers need not special-case an absent sink.
type multiEmitter []agent.Emitter

func (m multiEmitter) Emit(e agent.Event) {
	for _, em := range m {
		if em != nil {
			em.Emit(e)
		}
	}
}
