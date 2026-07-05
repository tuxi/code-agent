package agent

import (
	"context"
	"strings"
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

// An ineffective compaction — measured still at/over the threshold — must be
// flagged on the finalized event and cool the session down: the next turn does
// not compact again even though the prompt is still over the threshold. This is
// the guard against the local-model compact-measure-compact loop (P12.b).
func TestRunTurnIneffectiveCompactionCoolsDown(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "one", FinishReason: "stop", Usage: model.Usage{PromptTokens: 92000}},
		{Content: "two", FinishReason: "stop", Usage: model.Usage{PromptTokens: 92500}},
	}}
	rec := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, Compactor: shrinkCompactor{}, Emitter: rec}

	sess := overBudgetSession()
	if _, err := runner.RunTurn(context.Background(), sess, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(sess.Compactions) != 1 {
		t.Fatalf("first turn should compact once, got %d", len(sess.Compactions))
	}

	// Still over the threshold (92500 ≥ 90000), but the measured floor says
	// compacting again is futile — cooldown must hold.
	if _, err := runner.RunTurn(context.Background(), sess, "again"); err != nil {
		t.Fatal(err)
	}
	if len(sess.Compactions) != 1 {
		t.Fatalf("cooldown violated: %d compactions after second turn", len(sess.Compactions))
	}

	var flagged bool
	for _, e := range rec.events {
		if e.Kind == EventCompacted && e.Ineffective {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("the finalized event must be flagged Ineffective so the UI can warn")
	}
}

// Tier-0 pruning that already reclaims enough must skip the LLM summarize
// entirely for that round (P12.c): no compactor call, no compaction stat, and a
// context_pruned event with the old tool result truncated in place.
func TestRunTurnPruningSkipsSummarize(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop", Usage: model.Usage{PromptTokens: 50000}},
	}}
	rec := &capturingEmitter{}
	cc := &countingCompactor{}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3,
		Compactor: cc, CompactKeepTokens: 100, Emitter: rec}

	// 40k chars of old tool output ≈ 10k approx tokens; pruning it estimates
	// 95000 − ~10000 < 90000, under the threshold.
	sess := &session.Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "old"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}}},
			{Role: model.RoleTool, ToolCallID: "a", Content: strings.Repeat("A", 40000)},
			{Role: model.RoleAssistant, Content: "ans"},
		},
		Metadata:         map[string]any{},
		PromptTokens:     95000,
		CompactThreshold: 90000,
	}
	if _, err := runner.RunTurn(context.Background(), sess, "hi"); err != nil {
		t.Fatal(err)
	}

	if cc.calls != 0 {
		t.Fatalf("summarize must be skipped when pruning suffices, got %d compactor calls", cc.calls)
	}
	if len(sess.Compactions) != 0 {
		t.Fatalf("pruning is not a compaction: no stat expected, got %d", len(sess.Compactions))
	}
	if _, ok := rec.first(EventContextPruned); !ok {
		t.Fatal("expected a context_pruned event")
	}
	if got := sess.Messages[3].Content; len(got) >= 40000 || !strings.Contains(got, "[pruned:") {
		t.Fatalf("old tool result should be truncated in place, got %d chars", len(got))
	}
}

// countingCompactor records whether the LLM-summarize path was reached.
type countingCompactor struct{ calls int }

func (c *countingCompactor) Compact(_ context.Context, _ *session.Session) error {
	c.calls++
	return nil
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
