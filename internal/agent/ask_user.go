package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

// AutoModeDetector is an optional interface satisfied by *approve.AutoApprover.
// The agent package defines it here to avoid a circular import (approve imports
// agent; agent must not import approve). When the configured Approver implements
// this and reports auto mode on, ask_user falls back to its headless message
// instead of blocking — the user chose unattended operation.
type AutoModeDetector interface {
	IsAutoModeEnabled() bool
}

// --- Types ------------------------------------------------------------------

// AskUserQuestion is a clarification question the model asks the user when it
// encounters ambiguity. It carries a short header, a question, and 2–4 options.
// When AllowCustom is true, the UI appends an "Other" option that lets the user
// type their own answer — the answer's Notes field carries that text.
type AskUserQuestion struct {
	ID          string      `json:"id"`
	Question    string      `json:"question"`
	Header      string      `json:"header"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multi_select,omitempty"`
	AllowCustom bool        `json:"allow_custom,omitempty"`
}

// AskOption is one choice the user can select. Label is short (1-5 words);
// Description explains what choosing it means.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// AskUserAnswer is the user's reply: the selected option labels and, when the
// user chose "Other" (AllowCustom), free-text notes.
type AskUserAnswer struct {
	Selected []string `json:"selected"`
	Notes    string   `json:"notes,omitempty"`
}

// --- Interface --------------------------------------------------------------

// AskUserApprover presents a clarification question to the user and blocks
// until the user answers. It is the task-clarification counterpart to Approver
// (permission gate) and PlanApprover (workflow gate).
//
// Nil means "no user available" — the tool returns a fallback message telling
// the model to make its best judgment. Auto mode also skips the prompt.
type AskUserApprover interface {
	AskUser(q AskUserQuestion) (AskUserAnswer, error)
}

// --- Tool -------------------------------------------------------------------

// askUserTool lets the model voluntarily ask the user for clarification when it
// encounters genuine ambiguity — multiple valid approaches, unclear
// requirements, or a design trade-off the user should weigh in on. It is a
// model-visible tool registered in the main toolset, like enter_plan_mode.
type askUserTool struct {
	ref *RunnerRef
}

func (t *askUserTool) Name() string { return "ask_user" }

func (t *askUserTool) Description() string {
	return "Ask the user a question when you encounter ambiguity. " +
		"Use this when: (1) there are multiple valid approaches and you cannot " +
		"determine which the user prefers, (2) the requirements are unclear and " +
		"you need the user to clarify, (3) there is a design trade-off the user " +
		"should weigh in on. Provide 2-4 concrete options describing distinct " +
		"approaches — NOT yes/no questions (use the approval system for those). " +
		"The user sees this as a card with selectable options. Do NOT use this " +
		"for trivial choices you can safely decide yourself."
}

func (t *askUserTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"question": {
			Type:        "string",
			Description: "The complete question to ask the user. Should be clear, specific, and end with a question mark. Example: \"Which library should we use for date formatting?\"",
		},
		"header": {
			Type:        "string",
			Description: "Very short label displayed as a chip/tag (max 12 chars). Examples: \"Auth method\", \"Library\", \"Approach\".",
		},
		"options": {
			Type:        "array",
			Description: "2-4 distinct options for the user to choose from. Each option has a label (1-5 words) and a description explaining the trade-offs.",
			Items: &tools.Property{
				Type: "object",
				Properties: map[string]tools.Property{
					"label": {
						Type:        "string",
						Description: "The display text for this option (1-5 words). Should be concise and clearly describe the choice. Add \"(Recommended)\" at the end if this is your recommended option.",
					},
					"description": {
						Type:        "string",
						Description: "Explanation of what this option means or what will happen if chosen. Useful for providing context about trade-offs or implications.",
					},
				},
				Required: []string{"label", "description"},
			},
		},
		"multiSelect": {
			Type:        "boolean",
			Description: "Set to true to allow the user to select multiple options instead of just one. Use when choices are not mutually exclusive. Default false.",
		},
		"allowCustom": {
			Type:        "boolean",
			Description: "Set to true to add an \"Other\" option that lets the user type their own answer. Use when the listed options may not cover all possibilities. Default false.",
		},
	}, "question", "header", "options").JSON()
}

// fallbackMessage is returned when no user is available (headless, auto mode,
// goal mode). It tells the model to press on with its best judgment rather
// than blocking indefinitely.
const fallbackMessage = "No user is available to answer this question. " +
	"Make your best judgment based on the available information and proceed. " +
	"If you genuinely cannot decide, explain the trade-offs and pick the safer option."

func (t *askUserTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	r := t.ref.R
	if r == nil {
		return tools.ToolResult{}, fmt.Errorf("ask_user: runner not wired")
	}

	var in struct {
		Question    string       `json:"question"`
		Header      string       `json:"header"`
		Options     []AskOption  `json:"options"`
		MultiSelect bool         `json:"multiSelect"`
		AllowCustom bool         `json:"allowCustom"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("ask_user: invalid input: %w", err)
		}
	}

	if in.Question == "" {
		return tools.ToolResult{}, fmt.Errorf("ask_user: question is required")
	}
	if in.Header == "" {
		return tools.ToolResult{}, fmt.Errorf("ask_user: header is required")
	}
	if len(in.Options) < 2 {
		return tools.ToolResult{}, fmt.Errorf("ask_user: at least 2 options are required")
	}
	if len(in.Options) > 4 {
		return tools.ToolResult{}, fmt.Errorf("ask_user: at most 4 options are allowed")
	}

	// --- Headless / auto-mode fallback ---
	if r.AskUserApprover == nil {
		r.emit(Event{
			Kind:     EventAskUserTimeout,
			ToolName: "ask_user",
			ToolArgs: string(input),
			Text:     "no AskUserApprover wired (headless); falling back",
		})
		return tools.ToolResult{Content: fallbackMessage}, nil
	}
	if detector, ok := r.Approver.(AutoModeDetector); ok && detector.IsAutoModeEnabled() {
		r.emit(Event{
			Kind:     EventAskUserTimeout,
			ToolName: "ask_user",
			ToolArgs: string(input),
			Text:     "auto mode enabled; falling back",
		})
		return tools.ToolResult{Content: fallbackMessage}, nil
	}

	q := AskUserQuestion{
		ID:          newAskUserID(),
		Question:    in.Question,
		Header:      in.Header,
		Options:     in.Options,
		MultiSelect: in.MultiSelect,
		AllowCustom: in.AllowCustom,
	}

	r.emit(Event{
		Kind:     EventAskUserPosted,
		ToolName: "ask_user",
		ToolArgs: string(input),
		Text:     formatAskUserPosted(q),
	})

	answer, err := r.AskUserApprover.AskUser(q)

	// Build a concise resolution text for the event.
	resolutionText := formatAskUserResolved(answer, err)
	r.emit(Event{
		Kind:     EventAskUserResolved,
		ToolName: "ask_user",
		Text:     resolutionText,
	})

	if err != nil {
		return tools.ToolResult{
			Content: fmt.Sprintf(
				"The user could not answer: %s. %s", err.Error(), fallbackMessage),
		}, nil
	}

	// Format the answer as a structured result the model can use.
	return tools.ToolResult{
		Content: formatAnswer(answer),
	}, nil
}

func formatAskUserPosted(q AskUserQuestion) string {
	var b strings.Builder
	b.WriteString(q.Header)
	b.WriteString(": ")
	b.WriteString(q.Question)
	for _, o := range q.Options {
		b.WriteString("\n  • ")
		b.WriteString(o.Label)
		if o.Description != "" {
			b.WriteString(" — ")
			b.WriteString(o.Description)
		}
	}
	return b.String()
}

func formatAskUserResolved(a AskUserAnswer, err error) string {
	if err != nil {
		return "ask_user: error — " + err.Error()
	}
	if len(a.Selected) == 0 && a.Notes == "" {
		return "ask_user: user skipped"
	}
	var b strings.Builder
	b.WriteString("ask_user: selected [")
	b.WriteString(strings.Join(a.Selected, ", "))
	b.WriteString("]")
	if a.Notes != "" {
		b.WriteString("; notes: ")
		b.WriteString(a.Notes)
	}
	return b.String()
}

func formatAnswer(a AskUserAnswer) string {
	if len(a.Selected) == 0 && a.Notes == "" {
		return "The user did not select any option (skipped). Proceed with your best judgment."
	}
	var b strings.Builder
	b.WriteString("User selected: ")
	b.WriteString(strings.Join(a.Selected, ", "))
	if a.Notes != "" {
		b.WriteString("\nUser notes: ")
		b.WriteString(a.Notes)
	}
	return b.String()
}

func newAskUserID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "ask_" + hex.EncodeToString(b[:])
}

// NewAskUserTool creates the ask_user tool. ref.R is set after Runner
// construction — the tool dereferences it lazily at Execute time.
func NewAskUserTool(ref *RunnerRef) *askUserTool {
	return &askUserTool{ref: ref}
}
