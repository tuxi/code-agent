package model

import "testing"

func TestValidateAssistantTurnAcceptsValidToolCall(t *testing.T) {
	resp := Response{
		ToolCalls: []ToolCall{
			ToolCall{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "load_skill",
					Arguments: `{"name":"review-agent-runtime-architecture"}`,
				},
			},
		},
	}

	if err := resp.ValidateAssistantTurn(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAssistantTurnRejectsMalformedToolArguments(t *testing.T) {
	resp := Response{
		ToolCalls: []ToolCall{
			ToolCall{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "load_skill",
					Arguments: `{": "review-agent-runtime-architecture"}`,
				},
			},
		},
	}

	err := resp.ValidateAssistantTurn()
	if err == nil {
		t.Fatal("expected malformed arguments to be rejected")
	}
}
