// Package chat implements the chat transcript and composer components of the
// TUI, ported from opencode (internal/tui/components/chat) and adapted to
// code-agent's local infrastructure.
//
// Export contract (consumed by the conversation adapter, see conversation.go):
//
//   - Message is the single unit of a transcript. The adapter constructs
//     Message values directly (no service layer) and hands them to the list
//     via the messages defined in list.go (NewMessageMsg / SetMessagesMsg /
//     ClearMessagesMsg) or by calling (*List).SetMessages/Append directly.
//
//   - Tool cards are identified by ToolCall.CallID: re-sending a Message with
//     the same CallID (and any ID) updates the existing card in place instead
//     of appending a new one.
package chat

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	chAnsi "github.com/charmbracelet/x/ansi"

	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
)

// Kind discriminates the visual rendering of a Message.
type Kind int

const (
	// KindUser is a user message. Content holds the plain text the user sent.
	KindUser Kind = iota
	// KindAssistant is an assistant message. Content holds the raw markdown
	// source; it is rendered with the theme's glamour renderer and cached by
	// message ID. Finished=false marks an in-flight (streaming) message, which
	// re-renders on a throttle; Finished=true messages never re-enter the
	// streaming render path.
	KindAssistant
	// KindTool is a tool card (see ToolCall).
	KindTool
	// KindThinking is a transient thinking block (Content = reasoning text).
	KindThinking
	// KindSystem is a system notice rendered muted without a border.
	KindSystem
	// KindCompact is a compacted/summarized assistant block ("(summary)").
	KindCompact
)

func (k Kind) String() string {
	switch k {
	case KindUser:
		return "user"
	case KindAssistant:
		return "assistant"
	case KindTool:
		return "tool"
	case KindThinking:
		return "thinking"
	case KindSystem:
		return "system"
	case KindCompact:
		return "compact"
	}
	return "unknown"
}

// ToolStatus is the lifecycle state of a tool call.
type ToolStatus int

const (
	// ToolRunning means the tool is executing; the card shows a spinner-like
	// "Building command..." action line and no result body.
	ToolRunning ToolStatus = iota
	// ToolCompleted means the tool returned; the card shows its key params and
	// the rendered result body.
	ToolCompleted
	// ToolFailed means the tool errored; the card shows the error inline.
	ToolFailed
)

func (s ToolStatus) String() string {
	switch s {
	case ToolRunning:
		return "running"
	case ToolCompleted:
		return "completed"
	case ToolFailed:
		return "failed"
	}
	return "unknown"
}

// Param is one key/value parameter shown on a tool card. Key may be empty for
// positional parameters (e.g. a bash command or file path).
type Param struct {
	Key   string
	Value string
}

// ToolCall is a tool card's data.
type ToolCall struct {
	// CallID is the stable identity of the call. Re-sending a tool card with
	// the same CallID updates the existing card in place.
	CallID string
	// Name is the tool name as emitted by the agent (e.g. "run_command").
	// It is normalized to a friendly display name (Run, Read, Bash...).
	Name string
	// Params are the key parameters shown on the card, first one first
	// (e.g. the command for run_command, the file path for read_file).
	Params []Param
	// Status drives the card rendering.
	Status ToolStatus
	// Result is the raw tool output. For run_command it is rendered inside a
	// ```bash block; for diff-like tools (IsDiff) it is colored line-wise;
	// otherwise inside a code block with Language (default "text").
	Result string
	// IsDiff marks Result as a unified diff to be colored with the theme's
	// diff palette.
	IsDiff bool
	// Language is the code-block language for Result when it is not a diff.
	// Empty means "text".
	Language string
}

// Message is the single transcript unit exchanged between the conversation
// adapter and the chat UI.
type Message struct {
	// ID is a stable, unique message identity; it is the render-cache key.
	// Tool cards additionally key on Tool.CallID.
	ID string
	// Kind selects the rendering path.
	Kind Kind
	// Content: user=plain text, assistant/thinking/compact=markdown source,
	// system=plain notice text.
	Content string
	// Tool is non-nil only for KindTool.
	Tool *ToolCall
	// Model is an optional assistant model name shown muted after the body.
	// Empty hides it.
	Model string
	// Finished marks a finalized assistant message. Streaming messages have
	// Finished=false and are throttled by the list (see list.go).
	Finished bool
	// Fold is non-nil when this message is a collapsible group representative:
	// a thinking block, a tool group, or a system-notice group. Collapsed it
	// renders one summary line; expanded it renders the members.
	Fold *Fold
}

// Fold describes a collapsible transcript group. When non-nil the Message is a
// group representative; the List owns the open/close state keyed by Fold.ID.
type Fold struct {
	// ID is the group's stable identity, bound to Message.ID at creation.
	ID string
	// Title is the summary label ("Thought", "ℹ", or a tool display name).
	Title string
	// Count is the member count, shown as "Run ×3" / "ℹ 3 notices".
	Count int
	// Running reports whether any member is still in flight (a running tool, a
	// streaming thought). Running groups auto-expand; completion auto-collapses.
	Running bool
	// Open is the default expanded state the first time the List sees the group.
	Open bool
	// ToolCalls are the member cards of a tool group (KindTool).
	ToolCalls []ToolCall
	// Lines are the member lines of a system-notice group (KindSystem).
	Lines []string
}

// --- rendering -------------------------------------------------------------

type uiMessageType int

const (
	userMessageType uiMessageType = iota
	assistantMessageType
	toolMessageType
	thinkingMessageType
	systemMessageType
	compactMessageType
	foldSummaryMessageType

	// maxResultHeight caps the tool result body before the truncation marker.
	maxResultHeight = 10
)

// uiMessage is a single rendered block inside the transcript viewport.
type uiMessage struct {
	ID          string
	messageType uiMessageType
	position    int
	height      int
	content     string
}

// toMarkdown renders markdown source with the current theme's glamour
// renderer, wrapped at width.
func toMarkdown(content string, width int) string {
	r := styles.GetMarkdownRenderer(width)
	rendered, _ := r.Render(content)
	return rendered
}

// renderMessage renders a bordered markdown body. isUser switches the border
// color to the theme's secondary color; info lines are appended muted below.
func renderMessage(msg string, isUser bool, isFocused bool, width int, info ...string) string {
	t := theme.CurrentTheme()
	style := styles.BaseStyle().
		Width(width - 1).
		BorderLeft(true).
		Foreground(t.TextMuted()).
		BorderForeground(t.Primary()).
		BorderStyle(lipgloss.ThickBorder())
	if isUser {
		style = style.BorderForeground(t.Secondary())
	}
	parts := []string{
		styles.ForceReplaceBackgroundWithLipgloss(toMarkdown(msg, width), t.Background()),
	}
	// Remove the trailing newline the renderer adds.
	parts[0] = strings.TrimSuffix(parts[0], "\n")
	if len(info) > 0 {
		parts = append(parts, info...)
	}
	return style.Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// renderUserMessage renders a plain-text user message.
func renderUserMessage(msg Message, isFocused bool, width int, position int) uiMessage {
	content := renderMessage(msg.Content, true, isFocused, width)
	return uiMessage{
		ID:          msg.ID,
		messageType: userMessageType,
		position:    position,
		height:      lipgloss.Height(content),
		content:     content,
	}
}

// renderAssistantMessage renders an assistant message body plus an optional
// muted model footer.
func renderAssistantMessage(msg Message, width int, position int) []uiMessage {
	t := theme.CurrentTheme()
	baseStyle := styles.BaseStyle()
	content := msg.Content
	info := []string{}
	if msg.Finished && content == "" {
		content = "*Finished without output*"
	}
	if msg.Model != "" {
		info = append(info, baseStyle.
			Width(width-1).
			Foreground(t.TextMuted()).
			Render(" "+msg.Model))
	}
	rendered := renderMessage(content, false, true, width, info...)
	return []uiMessage{{
		ID:          msg.ID,
		messageType: assistantMessageType,
		position:    position,
		height:      lipgloss.Height(rendered),
		content:     rendered,
	}}
}

// renderThinkingMessage renders a muted thinking block. isFocused switches the
// border to the primary color while the model is working on that step.
func renderThinkingMessage(msg Message, isFocused bool, width int) uiMessage {
	content := renderMessage(msg.Content, false, isFocused, width)
	return uiMessage{
		ID:          msg.ID,
		messageType: thinkingMessageType,
		height:      lipgloss.Height(content),
		content:     content,
	}
}

// renderSystemMessage renders a muted, borderless notice.
func renderSystemMessage(msg Message, width int) uiMessage {
	t := theme.CurrentTheme()
	content := styles.BaseStyle().
		Width(width).
		Foreground(t.TextMuted()).
		Render(msg.Content)
	return uiMessage{
		ID:          msg.ID,
		messageType: systemMessageType,
		height:      lipgloss.Height(content),
		content:     content,
	}
}

// renderFoldSummary renders the collapsed one-line summary of a foldable group:
// "▸ Thought" (thinking, completed), "▸ Thinking…" (still streaming),
// "▸ Run ×3" / "▸ Read main.go" (tool group), "▸ ℹ 3 notices" (system group).
// A focused row highlights in the primary color for keyboard navigation.
func renderFoldSummary(f *Fold, focused bool, width int) uiMessage {
	t := theme.CurrentTheme()
	style := styles.BaseStyle().
		Width(width).
		Foreground(t.TextMuted())
	if focused {
		style = styles.BaseStyle().
			Width(width).
			Foreground(t.Primary()).
			Bold(true)
	}

	var summary string
	switch {
	case len(f.ToolCalls) > 0:
		// Tool group: "▸ Run ×3" or "▸ Read path" for a lone call.
		title := ToolDisplayName(f.ToolCalls[0].Name)
		switch {
		case f.Count > 1:
			summary = fmt.Sprintf("▸ %s ×%d", title, f.Count)
		case len(f.ToolCalls[0].Params) > 0:
			main := chAnsi.Truncate(
				f.ToolCalls[0].Params[0].Value,
				max(0, width-lipgloss.Width("▸ "+title)-4), "…")
			summary = fmt.Sprintf("▸ %s %s", title, main)
		default:
			summary = "▸ " + title
		}
	case len(f.Lines) > 0 && f.Count > 1:
		summary = fmt.Sprintf("▸ %s %d notices", f.Title, f.Count)
	case len(f.Lines) > 0:
		// Single notice stays informative; the chevron shows it is foldable.
		summary = "▸ " + chAnsi.Truncate(f.Lines[0], max(0, width-3), "…")
	default:
		// Thinking block.
		label := f.Title
		if f.Title == "Thought" && f.Running {
			label = "Thinking…"
		}
		summary = "▸ " + label
	}
	return uiMessage{
		ID:          f.ID,
		messageType: foldSummaryMessageType,
		height:      1,
		content:     style.Render(summary),
	}
}

// renderCompactMessage renders a summarized assistant block flagged "(summary)".
func renderCompactMessage(msg Message, width int) uiMessage {
	t := theme.CurrentTheme()
	baseStyle := styles.BaseStyle()
	content := msg.Content
	if content == "" {
		content = "*Finished without output*"
	}
	info := []string{
		baseStyle.
			Width(width - 1).
			Foreground(t.TextMuted()).
			Render(" (summary)"),
	}
	rendered := renderMessage(content, false, true, width, info...)
	return uiMessage{
		ID:          msg.ID,
		messageType: compactMessageType,
		height:      lipgloss.Height(rendered),
		content:     rendered,
	}
}

// ToolDisplayName maps a tool name to a short human-friendly label.
func ToolDisplayName(name string) string {
	switch strings.ToLower(name) {
	case "run_command":
		return "Run"
	case "read_file":
		return "Read"
	case "edit_file":
		return "Edit"
	case "write_file", "create_file":
		return "Write"
	case "list_files":
		return "List"
	case "grep":
		return "Grep"
	case "web_search":
		return "Search"
	case "web_fetch":
		return "Fetch"
	case "task":
		return "Task"
	case "apply_patch":
		return "Patch"
	case "todo_write":
		return "Plan"
	}
	return name
}

// getToolAction returns the "doing..." label shown while a tool is running.
func getToolAction(name string) string {
	switch strings.ToLower(name) {
	case "run_command":
		return "Running command..."
	case "read_file":
		return "Reading file..."
	case "edit_file":
		return "Editing file..."
	case "write_file", "create_file":
		return "Writing file..."
	case "list_files":
		return "Listing directory..."
	case "grep":
		return "Searching content..."
	case "web_search":
		return "Searching the web..."
	case "web_fetch":
		return "Fetching URL..."
	case "task":
		return "Preparing task..."
	case "todo_write":
		return "Updating plan..."
	}
	return "Working..."
}

// removeWorkingDirPrefix shortens absolute paths under the working directory to
// relative paths for display.
func removeWorkingDirPrefix(path string) string {
	wd, err := os.Getwd()
	if err == nil {
		if strings.HasPrefix(path, wd) {
			path = strings.TrimPrefix(path, wd)
		}
	}
	if strings.HasPrefix(path, "/") {
		path = strings.TrimPrefix(path, "/")
	}
	if strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	if strings.HasPrefix(path, "../") {
		path = strings.TrimPrefix(path, "../")
	}
	return path
}

// truncateHeight keeps only the first `height` lines of content.
func truncateHeight(content string, height int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		return strings.Join(lines[:height], "\n")
	}
	return content
}

// renderDiff colors a unified diff line-wise with the theme's diff palette.
func renderDiff(diffText string, width int) string {
	t := theme.CurrentTheme()
	base := styles.BaseStyle()
	lines := strings.Split(diffText, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		var style lipgloss.Style
		switch {
		case strings.HasPrefix(l, "@@"):
			style = base.Width(width).Foreground(t.DiffHunkHeader())
		case strings.HasPrefix(l, "+"):
			style = base.Width(width).Foreground(t.DiffAdded())
		case strings.HasPrefix(l, "-"):
			style = base.Width(width).Foreground(t.DiffRemoved())
		default:
			style = base.Width(width).Foreground(t.DiffContext())
		}
		out = append(out, style.Render(l))
	}
	return lipgloss.JoinVertical(lipgloss.Left, out...)
}

// renderToolParams renders the card's parameter line: the first param is the
// main argument; the rest are key=value pairs, all truncated to paramWidth.
func renderToolParams(paramWidth int, tool ToolCall) string {
	if len(tool.Params) == 0 {
		return ""
	}
	main := strings.ReplaceAll(tool.Params[0].Value, "\n", " ")
	if len(main) > paramWidth {
		if paramWidth > 3 {
			main = main[:paramWidth-3] + "..."
		} else {
			main = main[:min(paramWidth, len(main))]
		}
	}
	extras := []string{}
	for _, p := range tool.Params[1:] {
		if p.Value == "" {
			continue
		}
		if p.Key == "" {
			extras = append(extras, p.Value)
		} else {
			extras = append(extras, fmt.Sprintf("%s=%s", p.Key, p.Value))
		}
	}
	if len(extras) == 0 {
		return main
	}
	joined := strings.Join(extras, ", ")
	remaining := paramWidth - lipgloss.Width(main) - lipgloss.Width(joined) - 5
	if remaining < 30 {
		// No room for the extras; show just the main parameter.
		return main
	}
	return chAnsi.Truncate(fmt.Sprintf("%s (%s)", main, joined), paramWidth, "...")
}

// renderToolResponse renders the result body of a completed tool call: diffs
// are colored line-wise, everything else is shown inside a code block.
func renderToolResponse(tool ToolCall, width int) string {
	t := theme.CurrentTheme()
	result := truncateHeight(tool.Result, maxResultHeight)
	if tool.IsDiff {
		return renderDiff(result, width)
	}
	lang := tool.Language
	if lang == "" {
		lang = "text"
	}
	result = fmt.Sprintf("```%s\n%s\n```", lang, result)
	return styles.ForceReplaceBackgroundWithLipgloss(
		toMarkdown(result, width),
		t.Background(),
	)
}

// renderToolMessage renders a tool card. The card keeps a stable identity via
// ToolCall.CallID (list.go matches on it), so a re-sent card updates in place
// rather than appending.
func renderToolMessage(tool ToolCall, width int) uiMessage {
	t := theme.CurrentTheme()
	baseStyle := styles.BaseStyle()
	style := baseStyle.
		Width(width - 1).
		BorderLeft(true).
		BorderStyle(lipgloss.ThickBorder()).
		PaddingLeft(1).
		BorderForeground(t.TextMuted())

	nameText := baseStyle.
		Foreground(t.TextMuted()).
		Render(fmt.Sprintf("%s: ", ToolDisplayName(tool.Name)))

	var parts []string
	switch tool.Status {
	case ToolRunning:
		action := getToolAction(tool.Name)
		progressText := baseStyle.
			Width(width - 2 - lipgloss.Width(nameText)).
			Foreground(t.TextMuted()).
			Render(action)
		content := style.Render(lipgloss.JoinHorizontal(lipgloss.Left, nameText, progressText))
		return uiMessage{
			messageType: toolMessageType,
			height:      lipgloss.Height(content),
			content:     content,
		}
	default: // ToolCompleted or ToolFailed
		params := renderToolParams(width-2-lipgloss.Width(nameText), tool)
		formattedParams := baseStyle.
			Width(width - 2 - lipgloss.Width(nameText)).
			Foreground(t.TextMuted()).
			Render(params)
		parts = append(parts, lipgloss.JoinHorizontal(lipgloss.Left, nameText, formattedParams))
	}

	if tool.Status == ToolFailed {
		errContent := fmt.Sprintf("Error: %s", strings.ReplaceAll(tool.Result, "\n", " "))
		errContent = chAnsi.Truncate(errContent, width-1, "...")
		parts = append(parts, baseStyle.Width(width-2).Foreground(t.Error()).Render(errContent))
	} else if tool.Status == ToolCompleted {
		response := renderToolResponse(tool, width-2)
		parts = append(parts, strings.TrimSuffix(response, "\n"))
	}

	content := style.Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
	return uiMessage{
		messageType: toolMessageType,
		height:      lipgloss.Height(content),
		content:     content,
	}
}
