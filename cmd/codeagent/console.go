package main

import (
	"code-agent/internal/agent"
	"fmt"
	"os"
)

// buildEmitter wires the renderer stack: a console renderer, wrapped in the live
// "Thinking… Ns" ticker when stdout is a real terminal (the ticker's in-place
// rewrites would just be noise when piped to a file or pipe).
func buildEmitter() agent.Emitter {
	base := agent.Emitter(consoleEmitter{})
	if isTTY(os.Stdout) {
		return newLiveProgress(base, os.Stdout)
	}
	return base
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// consoleEmitter renders the agent's event stream to stdout. It reproduces the
// previous inline output exactly — the difference is that the loop no longer
// prints; it emits, and this is just one renderer. A live "Thinking… Ns" UI
// (P3.8) is another renderer fed by the same events (ModelStarted / Elapsed).
type consoleEmitter struct{}

func (consoleEmitter) Emit(e agent.Event) {
	switch e.Kind {
	case agent.EventThinking:
		fmt.Printf("\n[thinking] %s\n", e.Text)
	case agent.EventToolStarted:
		fmt.Printf("\n[%d] tool=%s args=%s\n", e.Step, e.ToolName, e.ToolArgs)
	case agent.EventAutoApproved:
		// Auto mode skipped a y/N. Show it so a transcript makes clear which
		// side-effecting calls ran without a human prompt, and why.
		fmt.Printf("[auto-approved] %s — %s\n", e.ToolName, e.Text)
	case agent.EventObserved:
		// A concise, scannable classification line printed just before the full
		// [result]. Only failures are shown — a successful command needs no line.
		if e.Failure != "" && e.Failure != "none" {
			fmt.Printf("[observed] %s  %s\n", e.Failure, e.Observation)
		}
	case agent.EventToolFinished:
		fmt.Printf("[result]\n%s\n", e.Observation)
	case agent.EventSkillLoaded:
		// Show which skill (and version) drove a behavior change, so a transcript
		// is debuggable ("why did it test-then-fix? — it loaded verify-change").
		name := e.ToolName
		if e.Version != "" {
			name += " v" + e.Version
		}
		fmt.Printf("\n[skill] loaded %s\n", name)
	case agent.EventTodoUpdated:
		fmt.Print("\n" + renderTodos(e.Todos))
	case agent.EventReflected:
		// The model said it was done; a grounded self-check sent it back for one
		// more pass. Show the human why, so the extra work reads as intent.
		fmt.Printf("\n[reflection] work looks incomplete — asking the model to self-check:\n%s\n", e.Text)
	case agent.EventCompacted:
		if e.AfterTokens == 0 {
			fmt.Printf("Context compacted: %d tokens → summary of %d chars (new size measured on next call)\n",
				e.BeforeTokens, e.SummaryChars)
		} else {
			fmt.Printf("[compaction] before=%d after=%d saved=%d ratio=%.1f%% summary=%dchars\n",
				e.BeforeTokens, e.AfterTokens, e.SavedTokens, e.Ratio*100, e.SummaryChars)
		}
	}
	// EventTurnStarted / EventModelStarted / EventModelFinished / EventTurnFinished
	// are emitted but intentionally not rendered here: the caller prints the final
	// answer from TurnResult, and model timing is for the P3.8 live renderer.
}
