package main

import (
	"code-agent/internal/agent"
	"fmt"
)

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
	case agent.EventToolFinished:
		fmt.Printf("[observation]\n%s\n", e.Observation)
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
