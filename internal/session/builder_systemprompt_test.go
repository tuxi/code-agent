package session

import (
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/prompt"
)

func TestWithSystemPromptOverridesIdentity(t *testing.T) {
	const custom = "CUSTOM-SUBAGENT-IDENTITY"
	sess, err := NewBuilder(t.TempDir()).WithSystemPrompt(custom).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sys := sess.Messages[0]
	if sys.Role != model.RoleSystem {
		t.Fatalf("first message role = %q, want system", sys.Role)
	}
	if !strings.Contains(sys.Content, custom) {
		t.Fatalf("system prompt should contain the custom identity, got: %q", sys.Content)
	}
	if strings.Contains(sys.Content, "You are CodeAgent") {
		t.Fatal("the default agent identity must not appear when overridden")
	}
}

func TestDefaultSystemPromptWhenUnset(t *testing.T) {
	sess, err := NewBuilder(t.TempDir()).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.HasPrefix(sess.Messages[0].Content, prompt.AgentSystemPrompt[:20]) {
		t.Fatal("an unset system prompt should fall back to the default agent identity")
	}
}
