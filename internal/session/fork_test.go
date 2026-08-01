package session

import (
	"errors"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/reference"
)

func TestForkHistoryIntoKeepsChildIdentityAndCopiesDurableHistory(t *testing.T) {
	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	source := &Session{
		ID:            "source",
		WorkspacePath: "/source",
		Name:          "Review",
		Model:         "source-model",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "source system"},
			{Role: model.RoleUser, Content: "inspect this"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call-1", Type: "function", Function: model.FunctionCall{Name: "read", Arguments: `{}`}}}},
		},
		Summary:      "durable summary",
		PromptTokens: 321,
		Compactions:  []CompactionStats{{BeforeTokens: 1000, AfterTokens: 300}},
		ArchivedAt:   now.Add(-time.Hour),
	}
	child := &Session{
		ID:            "child",
		WorkspacePath: "/child",
		Messages:      []model.Message{{Role: model.RoleSystem, Content: "child system"}},
		Metadata:      map[string]any{"owner": "child"},
	}

	if err := ForkHistoryInto(child, source, "", now); err != nil {
		t.Fatalf("ForkHistoryInto: %v", err)
	}
	if child.ID != "child" || child.WorkspacePath != "/child" || child.Metadata["owner"] != "child" {
		t.Fatalf("child identity changed: %+v", child)
	}
	if got := child.Messages[0].Content; got != "child system" {
		t.Fatalf("system prompt = %q", got)
	}
	if len(child.Messages) != 3 || child.Messages[1].Content != "inspect this" {
		t.Fatalf("history = %+v", child.Messages)
	}
	if child.Name != "Review (fork)" || child.Model != "source-model" || child.Summary != "durable summary" || child.PromptTokens != 321 || !child.ArchivedAt.IsZero() || !child.UpdatedAt.Equal(now) {
		t.Fatalf("fork fields = %+v", child)
	}

	// Ensure the copied tool-call slice does not alias the source checkpoint.
	child.Messages[2].ToolCalls[0].ID = "changed"
	if source.Messages[2].ToolCalls[0].ID != "call-1" {
		t.Fatal("forked tool calls alias source history")
	}
}

func TestForkHistoryIntoFailsClosedForSessionScopedState(t *testing.T) {
	tests := []struct {
		name   string
		source *Session
		want   error
	}{
		{name: "gateway cache", source: &Session{GatewayAssetCache: map[string]model.GatewayAssetRef{"x": {AssetID: 1}}}, want: ErrForkAssetsUnsupported},
		{name: "reference ledger", source: &Session{ReferenceLedger: []reference.Entry{{}}}, want: ErrForkAssetsUnsupported},
		{name: "message asset", source: &Session{Messages: []model.Message{{Assets: []model.GatewayAssetRef{{AssetID: 1}}}}}, want: ErrForkAssetsUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			child := &Session{Messages: []model.Message{{Role: model.RoleSystem}}}
			if err := ForkHistoryInto(child, test.source, "", time.Now()); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestManagedWorktreeHistoryCanBeCopiedButNotShared(t *testing.T) {
	source := sessionWithExecutionPolicy(ExecutionPolicyIsolatedWorktree)
	source.Messages = []model.Message{{Role: model.RoleSystem}, {Role: model.RoleUser, Content: "managed history"}}
	child := &Session{Messages: []model.Message{{Role: model.RoleSystem}}}
	if err := ForkHistoryInto(child, source, "", time.Now()); err != nil {
		t.Fatalf("ForkHistoryInto: %v", err)
	}
	if len(child.Messages) != 2 || child.Messages[1].Content != "managed history" {
		t.Fatalf("history = %+v", child.Messages)
	}
	if err := ValidateForkSource(source); !errors.Is(err, ErrForkManagedWorktreeUnsupported) {
		t.Fatalf("ValidateForkSource error = %v", err)
	}
}

func sessionWithExecutionPolicy(policy string) *Session {
	s := &Session{Metadata: map[string]any{}}
	s.SetExecutionPolicy(policy)
	return s
}
