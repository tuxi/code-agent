package main

import (
	"code-agent/internal/agent"
	"code-agent/internal/runtime"
	"fmt"
	"io"
	"os"
	"sync"
)

// taskProgress renders a single, in-place-updating heartbeat for running
// subagents, so a `task` call is not a silent black box — WITHOUT echoing the
// subagents' full (default-quiet) transcripts. It collapses the sub-runners'
// events onto one line that overwrites itself, and erases it when the last
// delegation finishes so the parent's next output starts clean.
//
// Concurrency-safe (P8.8 §8.2): with parallel tool execution the model can fan
// out several `task` calls at once, so multiple subagent goroutines emit into
// this ONE shared instance concurrently. State is keyed by subagent session id
// and guarded by mu; a single active subagent shows "step N/M · tool" as
// before, while N running collapse to "N subagents running · S steps".
type taskProgress struct {
	w     io.Writer
	mu    sync.Mutex
	subs  map[string]*subProgress // keyed by the subagent's session id
	shown bool
}

type subProgress struct {
	step int    // model calls so far — the loop ITERATION count the budget bounds
	tool string // the tool the current iteration is running, if any
}

func newTaskProgress(w io.Writer) *taskProgress {
	return &taskProgress{w: w, subs: make(map[string]*subProgress)}
}

func (p *taskProgress) Emit(e agent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Kind {
	case agent.EventTaskStarted:
		p.subs[e.SessionID] = &subProgress{}
	case agent.EventModelStarted:
		// One model call == one loop iteration; the unit runtime.SubAgentMaxSteps
		// bounds. Counting it (not EventToolStarted.Step, the cumulative tool-call
		// ordinal) keeps the "step N/M" honest against the budget.
		if s := p.subs[e.SessionID]; s != nil {
			s.step++
		}
	case agent.EventToolStarted:
		if s := p.subs[e.SessionID]; s != nil {
			s.tool = e.ToolName
		}
	case agent.EventTaskFinished:
		delete(p.subs, e.SessionID)
	default:
		return
	}
	p.render()
}

// render redraws the heartbeat from current state. Caller holds mu.
func (p *taskProgress) render() {
	switch len(p.subs) {
	case 0:
		p.erase()
	case 1:
		var s *subProgress
		for _, v := range p.subs {
			s = v
		}
		line := fmt.Sprintf("⟳ subagent · step %d/%d", s.step, runtime.SubAgentMaxSteps)
		if s.tool != "" {
			line += " · " + s.tool
		}
		p.show(line)
	default:
		total := 0
		for _, v := range p.subs {
			total += v.step
		}
		p.show(fmt.Sprintf("⟳ %d subagents running · %d steps", len(p.subs), total))
	}
}

// show overwrites the current line (\r + clear-to-end) with s. Caller holds mu.
func (p *taskProgress) show(s string) {
	fmt.Fprintf(p.w, "\r\x1b[K%s", s)
	p.shown = true
}

// erase clears the heartbeat line so the parent's next output starts clean.
// Caller holds mu.
func (p *taskProgress) erase() {
	if p.shown {
		fmt.Fprint(p.w, "\r\x1b[K")
		p.shown = false
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
