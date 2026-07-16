package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"code-agent/internal/tools/websearch"
)

func TestServeRunBuilderPlanToolsAreTurnScoped(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	base := tools.NewRegistry()
	sharedRef := WirePlanTools(base, t.TempDir())
	builder := NewServeRunBuilder(app.Config{}, app.ModelConfig{}, nil, nil, base, NewWorkspaceRegistry(""), sharedRef)
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

func TestServeRunBuilderUsesSessionCredentialForManagedSearch(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	target := credential.Target{Namespace: "gateway", Name: "default"}
	baseCredential := credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "base-token"},
	}
	sessionCredential := credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "session-token"},
	}
	cfg := app.Config{Web: app.WebConfig{Search: app.WebSearchConfig{
		Provider: "gateway", GatewayBaseURL: "https://gateway.example/api/v1/agent",
		Credential: app.CredentialRef{Namespace: "gateway", Name: "default"}, TopK: 5, GatewayTimeoutSeconds: 77,
	}}}
	base := tools.NewRegistry()
	if err := base.Register(websearch.NewTool(cfg.Web, baseCredential)); err != nil {
		t.Fatal(err)
	}
	sharedRef := WirePlanTools(base, t.TempDir())
	builder := NewServeRunBuilder(cfg, app.ModelConfig{}, nil, baseCredential, base, NewWorkspaceRegistry(""), sharedRef)
	workspace := t.TempDir()
	runner := builder.Build(conversation.RuntimeContext{
		Session:    &session.Session{ID: "session-a", WorkspacePath: workspace},
		Credential: sessionCredential,
	}).(*agent.Runner)

	toolValue, ok := runner.Tools.Get("web_search")
	if !ok {
		t.Fatal("managed web_search is missing")
	}
	provider := toolValue.(*websearch.Tool).Primary.(*websearch.GatewaySearchProvider)
	if provider.Client.Timeout != 77*time.Second {
		t.Fatalf("managed web_search timeout = %s", provider.Client.Timeout)
	}
	resolved, err := provider.Credential.Resolve(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Secret != "session-token" {
		t.Fatalf("managed web_search token = %q, want session token", resolved.Secret)
	}
}
