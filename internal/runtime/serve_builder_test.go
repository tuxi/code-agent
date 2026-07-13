package runtime

import (
	"context"
	"sync"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

func TestServeRunBuilderPlanToolsAreTurnScoped(t *testing.T) {
	base := tools.NewRegistry()
	sharedRef := WirePlanTools(base, t.TempDir())
	builder := NewServeRunBuilder(app.Config{}, app.ModelConfig{}, nil, base, NewWorkspaceRegistry(""), sharedRef)
	workspace := t.TempDir()

	var runners [2]*agent.Runner
	var wg sync.WaitGroup
	for i := range runners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runners[i] = builder.Build(conversation.RuntimeContext{
				Session: &session.Session{ID: string(rune('a' + i)), WorkspacePath: workspace},
			}).(*agent.Runner)
		}(i)
	}
	wg.Wait()

	for i, runner := range runners {
		tool, ok := runner.Tools.Get("enter_plan_mode")
		if !ok {
			t.Fatalf("runner %d missing enter_plan_mode", i)
		}
		if _, err := tool.Execute(context.Background(), tools.ExecutionContext{}, []byte(`{"title":"plan"}`)); err != nil {
			t.Fatalf("runner %d enter plan: %v", i, err)
		}
		if runner.PlanState != agent.PlanStatusPlanning {
			t.Fatalf("runner %d state = %v, want planning", i, runner.PlanState)
		}
	}
}
