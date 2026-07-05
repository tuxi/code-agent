package tui

import (
	"fmt"
	"time"

	"code-agent/internal/agent"
)

// ItemKind discriminates a timeline entry. Per the "Timeline First, Chat Second"
// north star, the assistant's reply (ItemAssistant) is just one kind among the
// rest — tools, skills, reflections — not the frame everything hangs off.
type ItemKind int

const (
	ItemUser ItemKind = iota // the prompt the user sent (from EventTurnStarted)
	ItemThinking
	ItemTool
	ItemSkill
	ItemReflection
	ItemCompaction
	ItemAssistant // the model's final reply (from EventTurnFinished)
	ItemSystem    // a UI-local notice (command output); never from an agent event
)

// ToolStatus tracks a tool item across its three events (started → observed →
// finished). A failure is the signal, so it is rendered distinctly and never
// collapses (§8).
type ToolStatus int

const (
	StatusPending ToolStatus = iota
	StatusOK
	StatusFail
)

// loadSkillTool is the tool whose effect a Skill card already represents, so its
// raw tool line is suppressed (the EventSkillLoaded card stands in for it). This
// is the one place the renderer knows a tool name — a presentation choice, like
// consoleEmitter already special-casing EventSkillLoaded.
const loadSkillTool = "load_skill"

// Item is one timeline entry — a *projection* folded from one or more agent
// events (a tool's started+observed+finished collapse to one item; a run of reads
// collapses to one group). The raw, replayable domain event is agent.Event in the
// EventStore; this is its display fold. A single item has no Children; a collapsed
// run (§8) is an item whose Children hold its members.
//
// ID is stable for the item's life (assigned at creation, never renumbered on a
// merge), so a card can be addressed — expanded, or matched to a replay frame.
// StartedAt/EndedAt bound the item; for a tool they span started→finished, giving
// a duration to display.
type Item struct {
	ID        string
	StartedAt time.Time
	EndedAt   time.Time

	Kind    ItemKind
	Step    int    // tool correlation key (ItemTool)
	Name    string // tool or skill name
	Args    string // tool arguments (ItemTool)
	Text    string // body: user input / thinking / tool result / reflection / reply
	Status  ToolStatus
	Failure string // failure label when Status == StatusFail
	Version string // skill version (ItemSkill)

	// Compaction (ItemCompaction). Pending marks the pre-measurement event (the
	// reclaimed size is only known at the next model call); Pruned marks a tier-0
	// deterministic prune (P12.c); Ineffective marks a measured compaction that
	// stayed over the threshold (P12.b) and renders as a warning.
	Before, After, Saved, SummaryChars int
	Ratio                              float64
	Pending                            bool
	Pruned                             bool
	Ineffective                        bool

	Children []Item // members of a collapsed run; empty for a single item
}

// Duration is how long the item took (meaningful for tools); zero if it is a
// point event or not yet finished.
func (it Item) Duration() time.Duration {
	if it.EndedAt.After(it.StartedAt) {
		return it.EndedAt.Sub(it.StartedAt)
	}
	return 0
}

// Timeline reduces the agent event stream into an ordered list of Items. It is
// pure and UI-free — feed it events, read Items, no terminal or model needed —
// which is what makes it the highest-value unit test in the package (§12).
type Timeline struct {
	Items []Item
	seq   int // backs stable per-item IDs
}

// add appends an item, assigning a stable ID and defaulting its timestamps to the
// event time when not already set.
func (t *Timeline) add(it Item, at time.Time) {
	t.seq++
	it.ID = fmt.Sprintf("e%d", t.seq)
	if it.StartedAt.IsZero() {
		it.StartedAt = at
	}
	if it.EndedAt.IsZero() {
		it.EndedAt = at
	}
	t.Items = append(t.Items, it)
}

// Apply folds one event into the timeline. Model start/finish carry no item (they
// drive the spinner, handled by the view); a tool's later events merge into the
// open item for its Step; everything else appends.
func (t *Timeline) Apply(ev agent.Event) {
	switch ev.Kind {
	case agent.EventTurnStarted:
		if ev.Text != "" {
			t.add(Item{Kind: ItemUser, Text: ev.Text}, ev.At)
		}
	case agent.EventThinking:
		if ev.Text != "" {
			t.add(Item{Kind: ItemThinking, Text: ev.Text}, ev.At)
		}
	case agent.EventToolStarted:
		if ev.ToolName == loadSkillTool {
			return // the Skill card represents this; suppress the raw tool line
		}
		t.add(Item{
			Kind:   ItemTool,
			Step:   ev.Step,
			Name:   ev.ToolName,
			Args:   ev.ToolArgs,
			Status: StatusPending,
		}, ev.At)
	case agent.EventObserved:
		if it := t.openTool(ev.Step); it != nil {
			if isFailure(ev.Failure) {
				it.Status = StatusFail
				it.Failure = ev.Failure
			} else {
				it.Status = StatusOK
			}
		}
	case agent.EventToolFinished:
		if ev.ToolName == loadSkillTool {
			return
		}
		if it := t.openTool(ev.Step); it != nil {
			it.Text = ev.Observation
			if !ev.At.IsZero() {
				it.EndedAt = ev.At
			}
			switch {
			case ev.Err != "":
				it.Status = StatusFail
				if it.Failure == "" {
					it.Failure = "error"
				}
			case it.Status == StatusPending:
				it.Status = StatusOK
			}
		}
	case agent.EventSkillLoaded:
		t.add(Item{Kind: ItemSkill, Name: ev.ToolName, Version: ev.Version, Status: StatusOK}, ev.At)
	case agent.EventReflected, agent.EventPreMutation:
		t.add(Item{Kind: ItemReflection, Text: ev.Text}, ev.At)
	case agent.EventVerified:
		txt := ev.Text
		if txt == "" {
			txt = "verification passed"
		}
		t.add(Item{Kind: ItemReflection, Text: "verify: " + txt}, ev.At)
	case agent.EventCompacted:
		t.add(Item{
			Kind:         ItemCompaction,
			Before:       ev.BeforeTokens,
			After:        ev.AfterTokens,
			Saved:        ev.SavedTokens,
			SummaryChars: ev.SummaryChars,
			Ratio:        ev.Ratio,
			// AfterTokens == 0 is the loop's "not yet measured" convention.
			Pending:     ev.AfterTokens == 0,
			Ineffective: ev.Ineffective,
		}, ev.At)
	case agent.EventContextPruned:
		t.add(Item{
			Kind:   ItemCompaction,
			Pruned: true,
			Before: ev.BeforeTokens,
			Saved:  ev.SavedTokens,
		}, ev.At)
	case agent.EventTurnFinished:
		if ev.Text != "" {
			t.add(Item{Kind: ItemAssistant, Text: ev.Text}, ev.At)
		}
	}
}

// openTool returns the most recent tool item for the given Step (the one being
// filled in), or nil. Searches from the end since the open tool is the newest.
func (t *Timeline) openTool(step int) *Item {
	for i := len(t.Items) - 1; i >= 0; i-- {
		if t.Items[i].Kind == ItemTool && t.Items[i].Step == step {
			return &t.Items[i]
		}
	}
	return nil
}

func isFailure(f string) bool { return f != "" && f != "none" }

// Collapse folds adjacent runs of same-kind *successful* items into one group
// (§8): N successful calls of the same tool, or a run of skill loads, become a
// single summary line whose Children hold the members. A failure never collapses
// — it must stand alone as the signal. Pure; called at render time so the raw
// timeline is preserved (and expansion stays possible).
func Collapse(items []Item) []Item {
	var out []Item
	for _, it := range items {
		key, ok := collapseKey(it)
		if ok && len(out) > 0 {
			last := &out[len(out)-1]
			if lkey, lok := collapseKey(*last); lok && lkey == key {
				if len(last.Children) == 0 {
					first := *last // the existing single becomes the first member
					last.Children = []Item{first}
				}
				last.Children = append(last.Children, it)
				last.EndedAt = it.EndedAt // the group spans through its newest member
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// collapseKey returns an item's grouping key and whether it may collapse. Only
// successful tools (grouped by tool name) and skills collapse; failures and
// every other kind stay on their own line.
func collapseKey(it Item) (string, bool) {
	switch it.Kind {
	case ItemTool:
		if it.Status == StatusOK {
			return "tool:" + it.Name, true
		}
	case ItemSkill:
		return "skill", true
	}
	return "", false
}
