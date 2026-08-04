package credential

import "testing"

func TestTargetConnectionID(t *testing.T) {
	tests := []struct {
		target Target
		want   string
	}{
		// BYOK connections flatten to their name.
		{Target{Namespace: "llm", Name: "deepseek"}, "deepseek"},
		{Target{Namespace: "llm", Name: "qwen"}, "qwen"},
		// The special gateway connection flattens to "gateway".
		{Target{Namespace: "gateway", Name: "default"}, "gateway"},
		// MCP stays independent — not flattenable.
		{Target{Namespace: "mcp", Name: "github"}, ""},
		// Undefined gateway connections are rejected, not guessed.
		{Target{Namespace: "gateway", Name: "other"}, ""},
		// Unknown namespaces are not flattenable.
		{Target{Namespace: "enterprise/sso", Name: "default"}, ""},
	}

	for _, tt := range tests {
		if got := tt.target.ConnectionID(); got != tt.want {
			t.Errorf("Target%v.ConnectionID() = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestTargetFromConnectionID(t *testing.T) {
	tests := []struct {
		id   string
		want Target
	}{
		{"deepseek", Target{Namespace: "llm", Name: "deepseek"}},
		{"qwen", Target{Namespace: "llm", Name: "qwen"}},
		{"gateway", Target{Namespace: "gateway", Name: "default"}},
	}

	for _, tt := range tests {
		if got := TargetFromConnectionID(tt.id); got != tt.want {
			t.Errorf("TargetFromConnectionID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// The flatten mapping must round-trip through the canonical Target for the
// known connection ids (llm/<name> and gateway/default).
func TestConnectionIDRoundTrip(t *testing.T) {
	ids := []string{"deepseek", "qwen", "glm", "gateway"}
	for _, id := range ids {
		back := TargetFromConnectionID(id).ConnectionID()
		if back != id {
			t.Errorf("round-trip %q → %v → %q", id, TargetFromConnectionID(id), back)
		}
	}
}
