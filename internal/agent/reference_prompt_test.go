package agent

import (
	"strings"
	"testing"

	"code-agent/internal/model"
)

func TestWithReferenceProtocolAddsEphemeralSystemGuidance(t *testing.T) {
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "You are CodeAgent."},
		{Role: model.RoleUser, Content: "hello"},
	}
	out := withReferenceProtocol(msgs)
	if !strings.Contains(out[0].Content, "$ref:ref_0001") {
		t.Fatalf("reference guidance missing: %q", out[0].Content)
	}
	if strings.Contains(msgs[0].Content, "$ref:ref_0001") {
		t.Fatalf("persisted system message was mutated: %q", msgs[0].Content)
	}
}
