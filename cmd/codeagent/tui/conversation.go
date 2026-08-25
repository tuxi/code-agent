package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
//     Tool.CallID for tool cards). app.go sends each event's returned messages
//     as one chat.BatchMessagesMsg, applied in slice order — no per-message
//     UpdateMessageMsg needed.
//   - Replaying a session's persisted events (resume) produces the same
//     transcript as the live run, because Apply is a pure fold over the events.
//
// Identity: a tool call's three events (started → observed → finished) share
// agent.Event.CallID, so the card is created once and updated in place. The
// streamed assistant reply keeps one message ID for its whole life, from the
// first token_delta until turn_finished marks it finished.
//
// Folding: thinking, tool calls, and low-signal system notices are folded into
// collapsible groups (chat.Fold) so the transcript reads as blocks, not a flat
// line per event. Consecutive same-name tool calls merge into one group card;
// consecutive low-signal notices (reflections, verify, job output…) merge into
// one system group. The List owns the open/close state; Apply only decides
// grouping and default state.
type Conversation struct {
	seq int // backs stable message IDs

	msgs        []chat.Message // the transcript as folded so far
	assistantID string         // ID of the in-flight streaming assistant message, if any
	thinkingID  string         // ID of the in-flight streaming thinking block, if any

	// Current tool group: consecutive same-name tool calls fold together while
	// the group stays the transcript tail.
	toolGroupID   string
	toolGroupName string

	sysGroupID string // current system-notice group, while it is the transcript tail

	// Turn footer accumulation, reset at each turn boundary.
	turnTokens  int
	turnElapsed time.Duration
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
		// A new turn closes any leftovers from the previous one: the streaming
		// assistant preview is finalized and an abandoned thinking block is
		// marked finished so it stops spinning.
		var out []chat.Message
		out = append(out, c.finalizeStreaming()...)
		out = append(out, c.closeThinking()...)
		c.turnTokens, c.turnElapsed = 0, 0
		if ev.Text == "" {
			return out
		}
		return append(out, c.append(chat.Message{Kind: chat.KindUser, Content: ev.Text})...)

	case agent.EventReasoningDelta:
		return c.streamThinking(ev.Text)

	case agent.EventThinking:
		if ev.Text == "" {
			return nil
		}
		// The authoritative reasoning snapshot closes the streaming block,
		// replacing its incremental content in place.
		if m := c.openThinking(); m != nil {
			m.Content = ev.Text
			m.Finished = true
			m.Fold.Running = false
			c.thinkingID = ""
			return []chat.Message{*m}
		}
		f := &chat.Fold{Title: "Thought", Count: 1, Running: false, Open: false}
		return c.appendFold(chat.Message{
			Kind: chat.KindThinking, Content: ev.Text, Finished: true,
		}, f)

	case agent.EventTokenDelta:
		return c.stream(ev.Text)

	case agent.EventAssistantText:
		// Intermediate narration the model produced before calling tools. A
		// finished assistant message, distinct from the streamed final answer.
		if ev.Text == "" {
			return nil
		}
		return c.append(c.new(chat.Message{
			Kind: chat.KindAssistant, Content: ev.Text, Finished: true,
		}))

	case agent.EventTurnFinished:
		var out []chat.Message
		out = append(out, c.closeThinking()...)
		// turn_finished carries the authoritative final answer; it replaces the
		// streamed preview in place so a streaming turn yields one assistant
		// message, not a preview plus a duplicate.
		if m := c.openAssistant(); m != nil {
			if ev.Text != "" {
				m.Content = ev.Text
			}
			m.Finished = true
			c.assistantID = ""
			out = append(out, *m)
		} else if ev.Text != "" {
			out = append(out, c.new(chat.Message{
				Kind: chat.KindAssistant, Content: ev.Text, Finished: true,
			}))
		}
		if c.turnTokens > 0 || c.turnElapsed > 0 {
			out = append(out, c.new(chat.Message{
				Kind: chat.KindSystem,
				Content: fmt.Sprintf("⤷ %s · %s",
					formatElapsed(c.turnElapsed), formatTokens(c.turnTokens)),
			}))
		}
		c.turnTokens, c.turnElapsed = 0, 0
		return out

	case agent.EventToolStarted:
		if ev.ToolName == loadSkillTool {
			return nil // the skill card stands in for the load_skill tool line (see timeline)
		}
		call := chat.ToolCall{
			CallID:   toolCallID(ev),
			Name:     ev.ToolName,
			Params:   toolParams(ev.ToolArgs),
			Status:   chat.ToolRunning,
			IsDiff:   diffTools[ev.ToolName],
			Language: toolLanguage(ev.ToolName),
		}
		// Same-name tool calls that are still the transcript tail fold into one
		// group card ("Run ×3"); anything in between starts a new group.
		if g := c.openToolGroup(ev.ToolName); g != nil {
			g.Fold.ToolCalls = append(g.Fold.ToolCalls, call)
			g.Fold.Count++
			g.Fold.Running = true
			return []chat.Message{*g}
		}
		f := &chat.Fold{
			Title: chat.ToolDisplayName(ev.ToolName), Count: 1,
			// Running tool groups force-expand in the list (work in flight stays
			// visible); the collapsed default means a finished group folds back
			// to its one-line summary.
			Running: true, Open: false,
		}
		out := c.appendFold(chat.Message{Kind: chat.KindTool}, f)
		out[0].Fold.ToolCalls = append(out[0].Fold.ToolCalls, call)
		c.toolGroupID = out[0].ID
		c.toolGroupName = ev.ToolName
		return out

	case agent.EventObserved:
		if ev.ToolName == loadSkillTool {
			return nil
		}
		if m, j := c.findTool(ev.CallID, ev.Step); m != nil {
			tc := c.memberTool(m, j)
			// The observed classification decides the status; the result body is
			// still filled by tool_finished below.
			if isFailure(ev.Failure) {
				tc.Status = chat.ToolFailed
				tc.Result = ev.Observation
			} else {
				tc.Status = chat.ToolCompleted
			}
			c.syncGroupRunning(m)
			return []chat.Message{*m}
		}
		return nil

	case agent.EventToolFinished:
		if ev.ToolName == loadSkillTool {
			return nil
		}
		if m, j := c.findTool(ev.CallID, ev.Step); m != nil {
			tc := c.memberTool(m, j)
			tc.Result = ev.Observation
			if ev.Err != "" {
				tc.Status = chat.ToolFailed
			} else if tc.Status == chat.ToolRunning {
				tc.Status = chat.ToolCompleted
			}
			c.syncGroupRunning(m)
			return []chat.Message{*m}
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
		return c.systemNotice(chat.Message{Kind: chat.KindSystem, Content: "↻ " + ev.Text})

	case agent.EventVerified:
		txt := ev.Text
		if txt == "" {
			txt = "verification passed"
		}
		return c.systemNotice(chat.Message{Kind: chat.KindSystem, Content: "↻ verify: " + txt})

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
		return c.systemNotice(chat.Message{Kind: chat.KindSystem, Content: ev.Chunk})

	case agent.EventJobFinished:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "■ job finished: " + ev.Text})

	case agent.EventTurnResumed:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "▶ resumed"})

	case agent.EventTurnPaused:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⏸ paused"})

	case agent.EventTurnFailed:
		var out []chat.Message
		out = append(out, c.finalizeStreaming()...)
		msg := "✗ turn failed"
		if ev.Err != "" {
			msg += ": " + ev.Err
		} else if ev.ErrorCode != "" {
			msg += " (" + ev.ErrorCode + ")"
		}
		return append(out, c.append(chat.Message{Kind: chat.KindSystem, Content: msg})...)

	case agent.EventTurnCancelled:
		var out []chat.Message
		out = append(out, c.finalizeStreaming()...)
		return append(out, c.append(chat.Message{Kind: chat.KindSystem, Content: "✕ turn cancelled"})...)

	case agent.EventTaskStarted:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⇶ task: " + ev.Text})

	case agent.EventTaskFinished:
		return c.append(chat.Message{Kind: chat.KindSystem, Content: "⇶ task finished"})

	case agent.EventTodoUpdated:
		return c.systemNotice(chat.Message{
			Kind:    chat.KindSystem,
			Content: fmt.Sprintf("▤ todo updated — %d items", len(ev.Todos)),
		})

	case agent.EventPlanStateChanged:
		return c.systemNotice(chat.Message{
			Kind:    chat.KindSystem,
			Content: "📋 plan mode: " + ev.PlanState.String(),
		})

	case agent.EventAutoApproved:
		return c.systemNotice(chat.Message{Kind: chat.KindSystem, Content: "⚡ auto-approved " + ev.ToolName})

	case agent.EventModelStarted:
		// A new model call begins a new step, so the previous step's streamed
		// text is complete. Finalizing here gives one assistant block per model
		// invocation (text → tools → text → tools → final) instead of one giant
		// merged block. If no text was streamed (model went straight to tools),
		// finalizeStreaming returns nothing.
		return c.finalizeStreaming()

	case agent.EventToolStdout, agent.EventToolStderr, agent.EventTurnAccepted,
		agent.EventTurnQueued, agent.EventSessionRepaired:
		return nil

	case agent.EventModelFinished:
		// Accumulate the current turn's cost for the footer emitted at the next
		// turn_finished boundary. The status bar spinner is driven in app.go.
		c.turnTokens += ev.TotalTokens
		c.turnElapsed += ev.Elapsed
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

// findTool returns the message holding the tool call with the given identity
// and the member index within it: j >= 0 for a fold-group member (use
// memberTool), -1 for a standalone card. Returns nil when the call is unknown.
func (c *Conversation) findTool(callID string, step int) (*chat.Message, int) {
	key := toolCallID(agent.Event{CallID: callID, Step: step})
	for i := range c.msgs {
		m := &c.msgs[i]
		if m.Kind != chat.KindTool {
			continue
		}
		if m.Tool != nil && m.Tool.CallID == key {
			return m, -1
		}
		if m.Fold != nil {
			for j := range m.Fold.ToolCalls {
				if m.Fold.ToolCalls[j].CallID == key {
					return m, j
				}
			}
		}
	}
	return nil, -1
}

// memberTool dereferences a findTool result to the card it identified.
func (c *Conversation) memberTool(m *chat.Message, j int) *chat.ToolCall {
	if j >= 0 {
		return &m.Fold.ToolCalls[j]
	}
	return m.Tool
}

// syncGroupRunning refreshes a tool group's Running flag from its members, so
// the list can auto-expand while work is in flight and auto-collapse on
// completion.
func (c *Conversation) syncGroupRunning(m *chat.Message) {
	if m.Fold == nil {
		return
	}
	m.Fold.Running = false
	for _, tc := range m.Fold.ToolCalls {
		if tc.Status == chat.ToolRunning {
			m.Fold.Running = true
			break
		}
	}
}

// openToolGroup returns the current tool group if it is still the transcript
// tail and matches the tool name; otherwise nil (a new group must start).
func (c *Conversation) openToolGroup(name string) *chat.Message {
	if c.toolGroupID == "" || c.toolGroupName != name {
		return nil
	}
	if n := len(c.msgs); n > 0 && c.msgs[n-1].ID == c.toolGroupID {
		return &c.msgs[n-1]
	}
	return nil
}

// openThinking returns the in-flight streaming thinking block, or nil.
func (c *Conversation) openThinking() *chat.Message {
	if c.thinkingID == "" {
		return nil
	}
	for i := range c.msgs {
		if c.msgs[i].ID == c.thinkingID {
			return &c.msgs[i]
		}
	}
	c.thinkingID = ""
	return nil
}

// streamThinking appends a reasoning delta to the in-flight thinking block,
// creating it on first use. The block is folded by default; only its summary
// line is visible while it streams.
func (c *Conversation) streamThinking(text string) []chat.Message {
	if text == "" {
		return nil
	}
	if m := c.openThinking(); m != nil {
		m.Content += text
		return []chat.Message{*m}
	}
	f := &chat.Fold{Title: "Thought", Count: 1, Running: true, Open: false}
	out := c.appendFold(chat.Message{Kind: chat.KindThinking, Content: text}, f)
	c.thinkingID = out[0].ID
	return out
}

// closeThinking marks any in-flight thinking block finished (dropped or
// cancelled turn, new turn boundary). A block left open would show a spinner
// forever.
func (c *Conversation) closeThinking() []chat.Message {
	m := c.openThinking()
	if m == nil {
		return nil
	}
	m.Finished = true
	m.Fold.Running = false
	c.thinkingID = ""
	return []chat.Message{*m}
}

// appendFold appends a group-representative message, binding the fold's ID to
// the message ID — the List keys open/close state on that identity.
func (c *Conversation) appendFold(m chat.Message, f *chat.Fold) []chat.Message {
	c.seq++
	m.ID = fmt.Sprintf("m%d", c.seq)
	f.ID = m.ID
	m.Fold = f
	c.msgs = append(c.msgs, m)
	return []chat.Message{m}
}

// systemNotice folds a low-signal system line into the open system-notice
// group when that group is still the transcript tail; otherwise it starts a
// new group. High-signal events (plan/askuser/job/task/failed…) bypass this
// and stay flat lines.
func (c *Conversation) systemNotice(m chat.Message) []chat.Message {
	if m.Content == "" {
		return nil
	}
	if g := c.openSystemGroup(); g != nil {
		g.Fold.Lines = append(g.Fold.Lines, m.Content)
		g.Fold.Count++
		return []chat.Message{*g}
	}
	f := &chat.Fold{Title: "ℹ", Count: 1, Open: false}
	out := c.appendFold(m, f)
	out[0].Fold.Lines = append(out[0].Fold.Lines, m.Content)
	c.sysGroupID = out[0].ID
	return out
}

// openSystemGroup returns the current system-notice group if it is still the
// transcript tail, else nil.
func (c *Conversation) openSystemGroup() *chat.Message {
	if c.sysGroupID == "" {
		return nil
	}
	if n := len(c.msgs); n > 0 && c.msgs[n-1].ID == c.sysGroupID {
		return &c.msgs[n-1]
	}
	return nil
}

// formatElapsed renders a turn's accumulated model time, e.g. "12.4s" or "2m3s".
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// formatTokens renders a token count compactly: "1.2k", "3.4k", "12k".
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d tokens", n)
	}
	return fmt.Sprintf("%.1fk tokens", float64(n)/1000)
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
