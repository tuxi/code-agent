package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/cmd/codeagent/tui/components/chat"
	"code-agent/internal/agent"
)

// Conversation adapts the agent event stream into chat.Message transcript
// entries. It is the chat-side counterpart of Timeline: both fold the same
// events, but Conversation drives the interactive transcript (streaming
// assistant text, in-place tool cards) while Timeline is the immutable
// projection for replay.
//
// Contract for app.go:
//
//   - Construct once per session: c := &Conversation{}.
//   - Feed events in order: for _, m := range c.Apply(ev) { ... }. Apply
//     returns the messages to show: a brand-new entry, or an update to one
//     already on screen. The chat List upserts (matching by message ID, and by
//     Tool.CallID for tool cards), so dispatching every returned message as a
//     chat.NewMessageMsg is sufficient — no UpdateMessageMsg needed.
//   - Replaying a session's persisted events (resume) produces the same
//     transcript as the live run, because Apply is a pure fold over the events.
//
// Identity: a tool call's three events (started → observed → finished) share
// agent.Event.CallID, so the card is created once and updated in place. The
// streamed assistant reply keeps one message ID for its whole life, from the
// first token_delta until turn_finished marks it finished.
type Conversation struct {
	seq int // backs stable message IDs

	msgs        []chat.Message // the transcript as folded so far
	assistantID string         // ID of the in-flight streaming assistant message, if any
}

// Messages returns the current transcript (useful for a full re-render).
func (c *Conversation) Messages() []chat.Message { return c.msgs }

// openAssistant returns a pointer to the in-flight streaming assistant message,
// or nil when none is open.
func (c *Conversation) openAssistant() *chat.Message {
	if c.assistantID == "" {
		return nil
	}
	for i := range c.msgs {
		if c.msgs[i].ID == c.assistantID {
			return &c.msgs[i]
		}
	}
	c.assistantID = ""
	return nil
}

// Apply folds one event and returns the messages that changed — new entries and
// in-place updates alike. Appending each to the chat List via upsert gives the
// correct transcript. Events with no transcript presence (model call brackets,
// tool stdout/stderr chunks, workflow telemetry) return nothing.
func (c *Conversation) Apply(ev agent.Event) []chat.Message {
	switch ev.Kind {
	case agent.EventTurnStarted:
		c.finalizeStreaming()
		if ev.Text == "" {
			return nil
		}
		return c.append(chat.Message{Kind: chat.KindUser, Content: ev.Text})

	case agent.EventThinking:
		if ev.Text == "" {
			return nil
		}
		return c.append(chat.Message{Kind: chat.KindThinking, Content: ev.Text})

	case agent.EventTokenDelta:
		return c.stream(ev.Text)

	case agent.EventTurnFinished:
		out := c.finalizeStreaming()
		if ev.Text == "" {
			return out
		}
		if m := c.openAssistant(); m != nil {
			// turn_finished carries the authoritative final answer; the streamed
			// preview may have been cut short.
			m.Content = ev.Text
			m.Finished = true
			c.assistantID = ""
			return append(out, *m)
		}
		return append(out, c.new(chat.Message{
			Kind: chat.KindAssistant, Content: ev.Text, Finished: true,
		}))

	case agent.EventToolStarted:
		if ev.ToolName == loadSkillTool {
			return nil // the skill card stands in for the load_skill tool line (see timeline)
		}
		return c.append(chat.Message{
			Kind: chat.KindTool,
			Tool: &chat.ToolCall{
				CallID:   toolCallID(ev),
				Name:     ev.ToolName,
				Params:   toolParams(ev.ToolArgs),
				Status:   chat.ToolRunning,
				IsDiff:   diffTools[ev.ToolName],
				Language: toolLanguage(ev.ToolName),
			},
		})

	case agent.EventObserved:
		if ev.ToolName == loadSkillTool {
			return nil
		}
		if card := c.openTool(ev.CallID, ev.Step); card != nil {
			// The observed classification decides the status; the result body is
			// still filled by tool_finished below.
			if isFailure(ev.Failure) {
				card.Tool.Status = chat.ToolFailed
				card.Tool.Result = ev.Observation
			} else {
				card.Tool.Status = chat.ToolCompleted
			}
			return []chat.Message{*card}
		}
		return nil

	case agent.EventToolFinished:
		if ev.ToolName == loadSkillTool {
			return nil
		}
		if card := c.openTool(ev.CallID, ev.Step); card != nil {
			card.Tool.Result = ev.Observation
			if ev.Err != "" {
				card.Tool.Status = chat.ToolFailed
			} else if card.Tool.Status == chat.ToolRunning {
				card.Tool.Status = chat.ToolCompleted
			}
			return []chat.Message{*card}
		}
		return nil

	case agent.EventSkillLoaded:
		// A completed tool card: name carries the skill, version rides in the
		// params line, no body. Keyed on the call ID so a replayed run updates
		// rather than duplicates.
		return c.append(chat.Message{
			Kind: chat.KindTool,
			Tool: &chat.ToolCall{
				CallID: ev.CallID,
				Name:   ev.ToolName,
				Params: []chat.Param{
					{Key: "skill", Value: ev.ToolName},
					{Key: "version", Value: ev.Version},
				},
				Status: chat.ToolCompleted,
			},
		})

	case agent.EventReflected, agent.EventPreMutation:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "↻ " + ev.Text})

	case agent.EventVerified:
		txt := ev.Text
		if txt == "" {
			txt = "verification passed"
		}
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "↻ verify: " + txt})

	case agent.EventCompacted:
		return c.append(chat.Message{
			Kind: chat.KindCompact,
			Content: compactionLine(Item{
				Kind:         ItemCompaction,
				Before:       ev.BeforeTokens,
				After:        ev.AfterTokens,
				Saved:        ev.SavedTokens,
				SummaryChars: ev.SummaryChars,
				Ratio:        ev.Ratio,
				// AfterTokens == 0 is the loop's "not yet measured" convention.
				Pending:     ev.AfterTokens == 0,
				Ineffective: ev.Ineffective,
			}),
		})

	case agent.EventContextPruned:
		return c.append(chat.Message{
			Kind: chat.KindCompact,
			Content: compactionLine(Item{
				Kind:   ItemCompaction,
				Pruned: true,
				Before: ev.BeforeTokens,
				Saved:  ev.SavedTokens,
			}),
		})

	case agent.EventContextEdited:
		return c.append(chat.Message{
			Kind:    chat.KindCompact,
			Content: "⤳ context edited — " + ev.Text + " stale tool results cleared (no LLM call)",
		})

	case agent.EventPlanProposed:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "📋 plan proposed — " + planTitle(ev.Text)})

	case agent.EventPlanApproved:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "✅ plan approved"})

	case agent.EventPlanRejected:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "↷ plan rejected — revising"})

	case agent.EventAskUserPosted:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "❓ " + ev.Text})

	case agent.EventAskUserResolved:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "↳ " + ev.Text})

	case agent.EventAskUserTimeout:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "↳ " + ev.Text})

	case agent.EventJobStarted:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "▶ job started: " + ev.Text})

	case agent.EventJobOutput:
		if ev.Chunk == "" {
			return nil
		}
		return c.append(chat.Message{Kind: chat.KindSystem, Content: ev.Chunk})

	case agent.EventJobFinished:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "■ job finished: " + ev.Text})

	case agent.EventTurnResumed:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "▶ resumed"})

	case agent.EventTurnPaused:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⏸ paused"})

	case agent.EventTurnFailed:
		msg := "✗ turn failed"
		if ev.Err != "" {
			msg += ": " + ev.Err
		} else if ev.ErrorCode != "" {
			msg += " (" + ev.ErrorCode + ")"
		}
		return c.append(chat.Message{Kind: chat.KindSystem, Content: msg})

	case agent.EventTurnCancelled:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "✕ turn cancelled"})

	case agent.EventTaskStarted:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⇶ task: " + ev.Text})

	case agent.EventTaskFinished:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⇶ task finished"})

	case agent.EventTodoUpdated:
		return c.append(chat.Message{
			Kind:    chat.KindSystem,
			Content: fmt.Sprintf("▤ todo updated — %d items", len(ev.Todos)),
		})

	case agent.EventPlanStateChanged:
		return c.append(chat.Message{
			Kind:    chat.KindSystem,
			Content: "📋 plan mode: " + ev.PlanState.String(),
		})

	case agent.EventAutoApproved:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⚡ auto-approved " + ev.ToolName})

	// No transcript presence — explicitly consumed elsewhere:
	// ModelStarted/ModelFinished drive the status bar spinner, ReasoningDelta is
	// the ephemeral preview that EventThinking supersedes, ToolStdout/Stderr are
	// live chunk streams, and the turn_accepted/turn_queued/workflow_*/
	// session_repaired events have no line in a single-session chat.
	case agent.EventModelStarted, agent.EventModelFinished, agent.EventReasoningDelta,
		agent.EventToolStdout, agent.EventToolStderr, agent.EventTurnAccepted,
		agent.EventTurnQueued, agent.EventSessionRepaired:
		return nil
	}
	return nil
}

// stream appends a token to the in-flight assistant message, creating it on
// first use. The message ID is stable for the whole streaming life so the list
// re-renders one growing block instead of appending per token.
func (c *Conversation) stream(text string) []chat.Message {
	if text == "" {
		return nil
	}
	if m := c.openAssistant(); m != nil {
		m.Content += text
		return []chat.Message{*m}
	}
	return c.append(chat.Message{Kind: chat.KindAssistant, Content: text})
}

// finalizeStreaming marks the in-flight assistant message finished (its content
// was already streamed) and returns it if it changed. Called at the turn
// boundary so a dropped turn never leaves a message spinning.
func (c *Conversation) finalizeStreaming() []chat.Message {
	m := c.openAssistant()
	if m == nil {
		return nil
	}
	m.Finished = true
	c.assistantID = ""
	return []chat.Message{*m}
}

// append records a new message in the transcript order and returns it for the
// caller to dispatch. An assistant message opened here is the streaming target:
// later token_delta events continue it until a turn boundary finalizes it.
func (c *Conversation) append(m chat.Message) []chat.Message {
	c.seq++
	m.ID = fmt.Sprintf("m%d", c.seq)
	c.msgs = append(c.msgs, m)
	if m.Kind == chat.KindAssistant {
		c.assistantID = m.ID
	}
	return []chat.Message{m}
}

// new records a message and returns a pointer to the stored copy.
func (c *Conversation) new(m chat.Message) chat.Message {
	c.seq++
	m.ID = fmt.Sprintf("m%d", c.seq)
	c.msgs = append(c.msgs, m)
	return m
}

// openTool returns the stored tool card for the given call, or nil. The card
// keys on CallID (the model's stable tool_call id); Step is the fallback when a
// provider omitted the id (the loop fills one, so this is defensive).
func (c *Conversation) openTool(callID string, step int) *chat.Message {
	key := toolCallID(agent.Event{CallID: callID, Step: step})
	for i := range c.msgs {
		if c.msgs[i].Kind == chat.KindTool && c.msgs[i].Tool != nil && c.msgs[i].Tool.CallID == key {
			return &c.msgs[i]
		}
	}
	return nil
}

// toolCallID normalizes a tool call's identity: the model's call id, or a
// step-scoped fallback when it is empty.
func toolCallID(ev agent.Event) string {
	if ev.CallID != "" {
		return ev.CallID
	}
	return fmt.Sprintf("s%d", ev.Step)
}

// planTitle returns the first heading line of a proposed plan as a short
// title, falling back to a length-capped flat string.
func planTitle(content string) string {
	for _, ln := range strings.Split(content, "\n") {
		if t := strings.TrimSpace(strings.TrimPrefix(ln, "#")); t != "" {
			return t
		}
	}
	t := strings.Join(strings.Fields(content), " ")
	if len(t) > 72 {
		t = t[:72] + "…"
	}
	return t
}

// diffTools emit unified diffs in their result body and are colored line-wise
// by the chat renderer (IsDiff). A tool call never switches class mid-flight,
// so the flag is fixed at card creation.
var diffTools = map[string]bool{
	"apply_patch": true,
	"git_commit":  true,
	"git_diff":    true,
}

// toolParams turns a tool call's JSON args into the card's parameter line: the
// primary argument first (path/command/pattern…), then a few named extras that
// matter for orientation (offset/limit, language). The chat renderer truncates.
func toolParams(args string) []chat.Param {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil || len(m) == 0 {
		if a := briefArgs(args); a != "" {
			return []chat.Param{{Value: a}}
		}
		return nil
	}
	var ps []chat.Param
	primary := func(keys ...string) {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				ps = append(ps, chat.Param{Value: fmt.Sprint(v)})
				return
			}
		}
	}
	primary("path", "command", "pattern", "query", "name", "dir", "tool", "url")
	for _, k := range []string{"offset", "limit", "language", "base_ref"} {
		if v, ok := m[k]; ok && fmt.Sprint(v) != "" {
			ps = append(ps, chat.Param{Key: k, Value: fmt.Sprint(v)})
		}
	}
	if len(ps) == 0 {
		return nil
	}
	return ps
}

// toolLanguage picks the code-block language for a completed tool's result
// body; empty defers to the chat renderer's "text" default.
func toolLanguage(name string) string {
	switch name {
	case "run_command":
		return "bash"
	}
	return ""
}
