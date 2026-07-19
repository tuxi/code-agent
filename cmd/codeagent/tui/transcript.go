package tui

import "code-agent/internal/agent"

// transcript turns the agent event stream into the printed lines — the SAME logic
// live (model.handleEvent) and on replay (renderTranscript), so a resumed session
// reads exactly as it did when it ran. It groups a model call plus the tools it
// ran into one "Thought for Ns, read 1 file" step; user prompts, the reply,
// reflections, and compaction print as their own cards. Per-turn state resets at
// the turn boundary.
type transcript struct {
	timeline Timeline // merges a tool's started→observed→finished by Step
	step     stepBuf  // the current model-call-plus-its-tools
	started  bool     // a turn has printed (controls the inter-turn separator)
}

// render folds one event into transcript lines. It is time-independent — fed live
// or from the persisted log, the same sequence yields the same output.
func (tr *transcript) render(ev agent.Event, width int) []string {
	var out []string
	switch ev.Kind {
	case agent.EventTurnStarted:
		if tr.started {
			out = append(out, "") // blank separator between turns
		}
		tr.started = true
		out = append(out, renderEntry(Item{Kind: ItemUser, Text: ev.Text}, width)...)

	case agent.EventModelStarted:
		out = append(out, renderStep(tr.step, width, false)...) // flush the previous step
		tr.step = stepBuf{active: true}

	case agent.EventModelFinished:
		tr.step.elapsed = ev.Elapsed
		if !tr.step.thinkingFinal {
			tr.step.thinking = ""
		}

	case agent.EventThinking:
		// Thinking is the complete persisted snapshot. Replace any ephemeral
		// reasoning_delta preview instead of appending and duplicating it.
		tr.step.thinking = ev.Text
		tr.step.thinkingFinal = true

	case agent.EventToolStarted, agent.EventObserved:
		tr.timeline.Apply(ev)

	case agent.EventToolFinished:
		tr.timeline.Apply(ev)
		if it := tr.timeline.openTool(ev.Step); it != nil { // nil for suppressed load_skill
			tr.step.tools = append(tr.step.tools, *it)
		}

	case agent.EventSkillLoaded:
		tr.step.tools = append(tr.step.tools, Item{Kind: ItemSkill, Name: ev.ToolName, Version: ev.Version, Status: StatusOK})

	case agent.EventReflected, agent.EventPreMutation:
		out = append(out, tr.flush(width)...)
		out = append(out, renderEntry(Item{Kind: ItemReflection, Text: ev.Text}, width)...)

	case agent.EventVerified:
		txt := ev.Text
		if txt == "" {
			txt = "verification passed"
		}
		out = append(out, tr.flush(width)...)
		out = append(out, renderEntry(Item{Kind: ItemReflection, Text: "verify: " + txt}, width)...)

	case agent.EventCompacted:
		out = append(out, tr.flush(width)...)
		out = append(out, renderEntry(Item{
			Kind: ItemCompaction, Before: ev.BeforeTokens, After: ev.AfterTokens,
			Saved: ev.SavedTokens, SummaryChars: ev.SummaryChars, Ratio: ev.Ratio,
			Pending: ev.AfterTokens == 0, Ineffective: ev.Ineffective,
		}, width)...)

	case agent.EventContextPruned:
		out = append(out, tr.flush(width)...)
		out = append(out, renderEntry(Item{Kind: ItemCompaction, Pruned: true, Before: ev.BeforeTokens, Saved: ev.SavedTokens}, width)...)

	case agent.EventTurnFinished:
		out = append(out, tr.flush(width)...)
		if ev.Text != "" {
			out = append(out, renderEntry(Item{Kind: ItemAssistant, Text: ev.Text}, width)...)
		}
		tr.timeline = Timeline{} // bound memory; next turn starts fresh
	}
	return out
}

// flush renders the buffered step (if any) and clears it. Committed steps are
// always collapsed (scrollback is immutable — no expand there).
func (tr *transcript) flush(width int) []string {
	lines := renderStep(tr.step, width, false)
	tr.step = stepBuf{}
	return lines
}

// renderTranscript replays a session's persisted events into the lines that
// reproduce its conversation — the /resume history view.
func renderTranscript(events []agent.Event, width int) []string {
	var tr transcript
	var lines []string
	for _, ev := range events {
		lines = append(lines, tr.render(ev, width)...)
	}
	return append(lines, tr.flush(width)...)
}
