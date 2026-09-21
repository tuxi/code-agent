package model

import (
	"errors"
	"strings"
	"testing"
)

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
	raw := `{"questions":[{"header":"Auth method"}]}]` // stray ']' — the ask_user shape
	resp := Response{
		ToolCalls: []ToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "ask_user",
					Arguments: raw,
				},
			},
		},
	}

	err := resp.ValidateAssistantTurn()
	if err == nil {
		t.Fatal("expected malformed arguments to be rejected")
	}
	// The sentinel drives the retry policy; without it the turn fails outright.
	if !errors.Is(err, ErrInvalidToolArguments) {
		t.Fatalf("error does not wrap ErrInvalidToolArguments: %v", err)
	}
	// The raw arguments ride along so a persistent failure is diagnosable.
	if !strings.Contains(err.Error(), raw) {
		t.Fatalf("error omits the offending arguments: %v", err)
	}
}

func TestValidateAssistantTurnRejectsNonObjectArguments(t *testing.T) {
	resp := Response{
		ToolCalls: []ToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "ask_user",
					Arguments: `[{"header":"Auth method"}]`,
				},
			},
		},
	}

	err := resp.ValidateAssistantTurn()
	if err == nil {
		t.Fatal("expected a top-level array to be rejected")
	}
	if !errors.Is(err, ErrInvalidToolArguments) {
		t.Fatalf("error does not wrap ErrInvalidToolArguments: %v", err)
	}
	if !strings.Contains(err.Error(), "expected a JSON object, got array") {
		t.Fatalf("error does not name the actual JSON kind: %v", err)
	}
}
