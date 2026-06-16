package agent

import (
	"context"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// shrinkCompactor stands in for a real compactor: it replaces history with
// system + a summary message + the latest message, and sets Summary.
type shrinkCompactor struct{}

func (shrinkCompactor) Compact(_ context.Context, s *session.Session) error {
	s.Summary = "DIGEST"
	s.Messages = []model.Message{
		s.Messages[0],
		{Role: model.RoleUser, Content: "[summary] DIGEST"},
		s.Messages[len(s.Messages)-1],
	}
	return nil
}

// noopCompactor satisfies the interface but never changes anything, modeling the
// "recent window already is the whole history" case.
type noopCompactor struct{}

func (noopCompactor) Compact(_ context.Context, _ *session.Session) error { return nil }

func overBudgetSession() *session.Session {
	return &session.Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "old"},
			{Role: model.RoleAssistant, Content: "ans"},
		},
		Metadata:         map[string]any{},
		PromptTokens:     95000,
		CompactThreshold: 90000,
	}
}

// A real compaction is recorded and then finalized using the next model call's
// measured prompt size — proving BeforeTokens comes from the trigger and
// AfterTokens from a measurement, not a fabricated 0.
func TestRunTurnRecordsAndFinalizesCompaction(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop", Usage: model.Usage{PromptTokens: 25000}},
	}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, Compactor: shrinkCompactor{}}

	sess := overBudgetSession()
	if _, err := runner.RunTurn(context.Background(), sess, "hi"); err != nil {
		t.Fatal(err)
	}

	if len(sess.Compactions) != 1 {
		t.Fatalf("expected exactly one recorded compaction, got %d", len(sess.Compactions))
	}
	st := sess.Compactions[0]
	if !st.Finalized() {
		t.Fatal("compaction stat was never finalized by the next model call")
	}
	if st.BeforeTokens != 95000 || st.AfterTokens != 25000 || st.SavedTokens != 70000 {
		t.Fatalf("unexpected stats: %s", st)
	}
	// PromptTokens must reflect the measured post-compaction size, not a fake 0.
	if sess.PromptTokens != 25000 {
		t.Fatalf("PromptTokens = %d, want the measured 25000", sess.PromptTokens)
	}
}

// A compaction that changes nothing must not be recorded as a stat.
func TestRunTurnSkipsStatsOnNoOpCompaction(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop", Usage: model.Usage{PromptTokens: 95000}},
	}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, Compactor: noopCompactor{}}

	sess := overBudgetSession()
	if _, err := runner.RunTurn(context.Background(), sess, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(sess.Compactions) != 0 {
		t.Fatalf("a no-op compaction must not record a stat, got %d", len(sess.Compactions))
	}
}
