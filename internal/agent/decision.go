package agent

import "encoding/json"

type DecisionType string

const (
	DecisionFinalAnswer DecisionType = "final_answer"
	DecisionToolCall    DecisionType = "tool_call"
	DecisionAskUser     DecisionType = "ask_user"
)

type Decision struct {
	Type    DecisionType    `json:"type"`
	Message string          `json:"message,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}
