package agent

import "encoding/json"

type DecisionType string

const (
	DecisionFinalAnswer   DecisionType = "final_answer"
	DecisionToolCall      DecisionType = "tool_call"
	DecisionAskUser       DecisionType = "ask_user"
	DecisionPlan          DecisionType = "plan"
	DecisionPatchProposal DecisionType = "patch_proposal"
)

type Decision struct {
	Type    DecisionType    `json:"type"`
	Message string          `json:"message,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Reason  string          `json:"reason,omitempty"`

	// Plan fields
	Summary           string   `json:"summary,omitempty"`
	Steps             []string `json:"steps,omitempty"`
	Risks             []string `json:"risks,omitempty"`
	NeedsConfirmation bool     `json:"needs_confirmation,omitempty"`

	// patch_proposal fields
	Patch string `json:"patch,omitempty"`
	Risk  string `json:"risk,omitempty"`
}
