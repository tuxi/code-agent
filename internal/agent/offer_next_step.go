package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"code-agent/internal/followup"
	"code-agent/internal/tools"
)

const (
	maxFollowUpDetailRunes      = 2000
	maxFollowUpQuestionRunes    = 240
	maxFollowUpLabelRunes       = 40
	maxFollowUpMessageRunes     = 1000
	maxFollowUpDescriptionRunes = 500
)

// TurnFinalizer marks a tool whose successful invocation may end the current
// turn when it is the model's only tool call. The loop checks this interface,
// never the tool name, so terminal presentation stays a capability rather than
// a special case.
type TurnFinalizer interface {
	FinalizesTurn() bool
}

// offerNextStepTool is the model-facing producer for an end-of-turn follow-up
// card. It is registered only for clients that negotiated the capability.
type offerNextStepTool struct{}

func NewOfferNextStepTool() tools.Tool { return offerNextStepTool{} }

func (offerNextStepTool) Name() string { return "offer_next_step" }

func (offerNextStepTool) Description() string {
	return "Offer the user a structured next step after completing the current turn. " +
		"Call this only when the result exposes a real decision or a clearly useful next action. " +
		"Do not call it for simple acknowledgements or routine completion. " +
		"Provide 2-3 mutually exclusive options with exactly one recommended option, or omit options " +
		"for an open-ended question. Each option has a short label and the exact message to send as the " +
		"user's next turn. Call this tool only as the sole tool call in the final assistant response; " +
		"the assistant text is the final answer shown above the card."
}

func (offerNextStepTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"detail": {
			Type:        "string",
			Description: "Concise factual context for the decision: what was completed and why a next step is needed. Do not repeat the full final answer.",
		},
		"question": {
			Type:        "string",
			Description: "One direct question shown as the card title. For example: \"要顺手把 bind_device 的 TypeError 修掉吗？\"",
		},
		"options": {
			Type:        "array",
			Description: "Optional choices. Omit or use an empty array for an open-ended prompt. When present, use 2-3 options and mark exactly one recommended.",
			Items: &tools.Property{
				Type: "object",
				Properties: map[string]tools.Property{
					"label": {
						Type:        "string",
						Description: "Short button text and selected-state trace, usually 1-5 words.",
					},
					"message": {
						Type:        "string",
						Description: "The exact next-turn user message sent when this option is clicked. Defaults to label when omitted.",
					},
					"description": {
						Type:        "string",
						Description: "Optional explanation of the option's effect. Not always displayed.",
					},
					"recommended": {
						Type:        "boolean",
						Description: "Whether this is the single recommended option.",
					},
				},
				Required: []string{"label"},
			},
		},
	}, "detail", "question").JSON()
}

func (offerNextStepTool) FinalizesTurn() bool { return true }

func (offerNextStepTool) Execute(_ context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in struct {
		Detail   string `json:"detail"`
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Message     string `json:"message"`
			Description string `json:"description"`
			Recommended bool   `json:"recommended"`
		} `json:"options"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("offer_next_step: invalid input: %w", err)
		}
	}
	detail := strings.TrimSpace(in.Detail)
	question := strings.TrimSpace(in.Question)
	if detail == "" {
		return tools.ToolResult{}, fmt.Errorf("offer_next_step: detail is required")
	}
	if utf8.RuneCountInString(detail) > maxFollowUpDetailRunes {
		return tools.ToolResult{}, fmt.Errorf("offer_next_step: detail exceeds %d runes", maxFollowUpDetailRunes)
	}
	if question == "" {
		return tools.ToolResult{}, fmt.Errorf("offer_next_step: question is required")
	}
	if utf8.RuneCountInString(question) > maxFollowUpQuestionRunes {
		return tools.ToolResult{}, fmt.Errorf("offer_next_step: question exceeds %d runes", maxFollowUpQuestionRunes)
	}
	if len(in.Options) == 1 || len(in.Options) > 3 {
		return tools.ToolResult{}, fmt.Errorf("offer_next_step: options must be empty or contain 2-3 entries")
	}

	options := make([]followup.Option, 0, len(in.Options))
	recommended := 0
	for i, raw := range in.Options {
		label := strings.TrimSpace(raw.Label)
		message := strings.TrimSpace(raw.Message)
		if message == "" {
			message = label
		}
		description := strings.TrimSpace(raw.Description)
		if label == "" {
			return tools.ToolResult{}, fmt.Errorf("offer_next_step: option %d label is required", i+1)
		}
		if utf8.RuneCountInString(label) > maxFollowUpLabelRunes {
			return tools.ToolResult{}, fmt.Errorf("offer_next_step: option %d label exceeds %d runes", i+1, maxFollowUpLabelRunes)
		}
		if utf8.RuneCountInString(message) > maxFollowUpMessageRunes {
			return tools.ToolResult{}, fmt.Errorf("offer_next_step: option %d message exceeds %d runes", i+1, maxFollowUpMessageRunes)
		}
		if utf8.RuneCountInString(description) > maxFollowUpDescriptionRunes {
			return tools.ToolResult{}, fmt.Errorf("offer_next_step: option %d description exceeds %d runes", i+1, maxFollowUpDescriptionRunes)
		}
		if raw.Recommended {
			recommended++
		}
		options = append(options, followup.Option{
			ID:          fmt.Sprintf("opt_%d", i+1),
			Label:       label,
			Message:     message,
			Description: description,
			Recommended: raw.Recommended,
		})
	}
	if len(options) > 0 && recommended != 1 {
		return tools.ToolResult{}, fmt.Errorf("offer_next_step: exactly one option must be recommended")
	}

	sum := sha256.Sum256([]byte(ec.TurnID + "\x00" + ec.CallID))
	offer := &followup.Offer{
		ID:          "fu_" + hex.EncodeToString(sum[:8]),
		TurnID:      ec.TurnID,
		Detail:      detail,
		Question:    question,
		Options:     options,
		Dismissible: true,
	}
	return tools.ToolResult{
		Content:  "Next-step offer recorded. The turn ends with the assistant text that accompanied this tool call.",
		FollowUp: offer,
	}, nil
}

var (
	_ tools.Tool    = offerNextStepTool{}
	_ TurnFinalizer = offerNextStepTool{}
)
