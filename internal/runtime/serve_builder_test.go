package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
	"code-agent/internal/tools/task"
	"code-agent/internal/tools/websearch"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestServeRunBuilderPlanToolsAreTurnScoped(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	base := tools.NewRegistry()
	sharedRef := WirePlanTools(base, t.TempDir())
	builder := NewServeRunBuilder(app.Config{}, settings.Settings{}, app.ModelConfig{}, nil, nil, base, NewWorkspaceRegistry(""), sharedRef)
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
	builder := NewServeRunBuilder(cfg, settings.Settings{}, app.ModelConfig{}, nil, baseCredential, base, NewWorkspaceRegistry(""), sharedRef)
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

func TestServeRunBuilderRuntimeAliasStrictness(t *testing.T) {
	alias := "provider.Y29tcGFueQ.model.ZGVlcHNlZWstY2hhdA"
	direct := app.ModelConfig{
		Name:       alias,
		Provider:   "openai",
		BaseURL:    "https://direct.example/v1",
		Model:      "deepseek-chat",
		Credential: app.CredentialRef{Namespace: "llm", Name: "company"},
	}
	cfg := app.Config{
		DefaultModel: alias,
		Models:       map[string]app.ModelConfig{alias: direct},
	}
	builder := NewServeRunBuilder(cfg, settings.Settings{}, direct, nil, nil, tools.NewRegistry(), NewWorkspaceRegistry(""), nil)

	got, err := builder.ResolveModel(alias)
	if err != nil || got == nil || got.Model != "deepseek-chat" {
		t.Fatalf("ResolveModel(alias) = %+v, %v", got, err)
	}
	if _, err := builder.ResolveModel("provider.bWlzc2luZw.model.bW9kZWw"); err == nil {
		t.Fatal("unknown provider.* Runtime Alias must fail")
	} else {
		var unavailable ModelUnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("unknown Runtime Alias error = %T, want ModelUnavailableError", err)
		}
		if unavailable.AgentInputErrorCode() != "model_unavailable" || unavailable.SafeMessage() == "" {
			t.Fatalf("unknown Runtime Alias contract = code %q message %q", unavailable.AgentInputErrorCode(), unavailable.SafeMessage())
		}
	}
	if _, err := builder.ResolveModel("legacy-wire-model"); err == nil {
		t.Fatal("bare wire model must not fall back through a Direct Provider")
	}

	gateway := direct
	gateway.BaseURL = "https://gateway.example/api/v1/agent"
	gateway.Credential = app.CredentialRef{Namespace: "gateway", Name: "default"}
	gatewayBuilder := NewServeRunBuilder(cfg, settings.Settings{}, gateway, nil, nil, tools.NewRegistry(), NewWorkspaceRegistry(""), nil)
	got, err = gatewayBuilder.ResolveModel("legacy-gateway-model")
	if err != nil || got == nil || got.Model != "legacy-gateway-model" {
		t.Fatalf("legacy Gateway wire model = %+v, %v", got, err)
	}
	if _, err := gatewayBuilder.ResolveModel("provider.bWlzc2luZw.model.bW9kZWw"); err == nil {
		t.Fatal("unknown Runtime Alias must fail even on the Gateway compatibility path")
	}
}

func TestServeRunBuilderRoutesConcurrentProvidersAndCredentials(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	type modelCase struct {
		alias, baseURL, wireModel, target, secret string
	}
	cases := []modelCase{
		{"provider.Z2F0ZXdheQ.model.Z2F0ZXdheS1tb2RlbA", "https://gateway.test/api/v1/agent", "gateway-model", "gateway/default", "gateway-token"},
		{"provider.ZGVlcHNlZWs.model.ZGVlcHNlZWstY2hhdA", "https://deepseek.test/v1", "deepseek-chat", "llm/deepseek", "deepseek-key"},
		{"provider.cXdlbg.model.cXdlbjMtY29kZXI", "https://qwen.test/v1", "qwen3-coder", "llm/qwen", "qwen-key"},
		{"provider.cHJveHktcHJvZA.model.c2hhcmVk", "https://proxy-prod.test/v1", "shared", "llm/proxy-prod", "prod-key"},
		{"provider.cHJveHktc3RhZ2U.model.c2hhcmVk", "https://proxy-stage.test/v1", "shared", "llm/proxy-stage", "stage-key"},
	}

	cfg := app.Config{
		DefaultModel: cases[0].alias,
		Models:       make(map[string]app.ModelConfig, len(cases)),
		Provider:     app.ProviderConfig{RequestTimeoutSeconds: 5},
		Agent:        app.AgentConfig{MaxSteps: 1},
	}
	baseCredentials := make(credential.StaticResolver)
	for _, tc := range cases {
		namespace, name, _ := strings.Cut(tc.target, "/")
		cfg.Models[tc.alias] = app.ModelConfig{
			Name: tc.alias, Provider: "openai", BaseURL: tc.baseURL, Model: tc.wireModel,
			ContextWindow: 128000,
			Credential:    app.CredentialRef{Namespace: namespace, Name: name},
		}
		if namespace == "llm" {
			baseCredentials[credential.Target{Namespace: namespace, Name: name}] =
				credential.Credential{Type: credential.Bearer, Secret: tc.secret}
		}
	}
	sessionCredential := credential.StaticResolver{
		{Namespace: "gateway", Name: "default"}: {Type: credential.Bearer, Secret: "gateway-token"},
	}
	defaultMC := cfg.Models[cfg.DefaultModel]
	defaultProvider, err := BuildProvider(defaultMC, cfg.Provider, baseCredentials)
	if err != nil {
		t.Fatal(err)
	}
	builder := NewServeRunBuilder(
		cfg, settings.Settings{}, defaultMC, defaultProvider, baseCredentials,
		tools.NewRegistry(), NewWorkspaceRegistry(""), nil,
	)

	type observedRequest struct {
		alias, url, auth, model string
	}
	observed := make(chan observedRequest, len(cases))
	var wg sync.WaitGroup
	for _, tc := range cases {
		tc := tc
		runner := builder.Build(conversation.RuntimeContext{
			Session:         &session.Session{ID: tc.alias, WorkspacePath: t.TempDir()},
			Model:           tc.alias,
			ResolvedModel:   tc.wireModel,
			Credential:      sessionCredential,
			Approver:        runtimeTestApprover{},
			PlanApprover:    nil,
			AskUserApprover: nil,
		}).(*agent.Runner)
		resilient := runner.Model.(*model.ResilientProvider)
		openAI := resilient.Inner.(*model.OpenAICompatibleProvider)
		openAI.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			observed <- observedRequest{
				alias: tc.alias,
				url:   req.URL.String(),
				auth:  req.Header.Get("Authorization"),
				model: body.Model,
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`,
				)),
			}, nil
		})}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := runner.Model.Complete(context.Background(), model.Request{
				Model: tc.wireModel,
				Messages: []model.Message{{
					Role: model.RoleUser, Content: "route",
				}},
			}); err != nil {
				t.Errorf("%s Complete: %v", tc.alias, err)
			}
		}()
	}
	wg.Wait()
	close(observed)

	byAlias := make(map[string]observedRequest, len(cases))
	for req := range observed {
		byAlias[req.alias] = req
	}
	for _, tc := range cases {
		req, ok := byAlias[tc.alias]
		if !ok {
			t.Errorf("%s was not called", tc.alias)
			continue
		}
		if req.url != tc.baseURL+"/chat/completions" {
			t.Errorf("%s URL = %q", tc.alias, req.url)
		}
		if req.model != tc.wireModel {
			t.Errorf("%s model = %q, want %q", tc.alias, req.model, tc.wireModel)
		}
		if req.auth != "Bearer "+tc.secret {
			t.Errorf("%s auth routed to wrong credential", tc.alias)
		}
	}
}

func TestServeRunBuilderRebindsTaskToCurrentTurn(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	mainAlias := "provider.bWFpbg.model.bWFpbi1tb2RlbA"
	altAlias := "provider.YWx0.model.YWx0LW1vZGVs"
	mainMC := app.ModelConfig{Name: mainAlias, Provider: "openai", BaseURL: "http://localhost:11434/v1", Model: "main-model", ContextWindow: 128000}
	altMC := app.ModelConfig{Name: altAlias, Provider: "openai", BaseURL: "http://localhost:11435/v1", Model: "alt-model", ContextWindow: 128000}
	cfg := app.Config{
		DefaultModel: mainAlias,
		Models:       map[string]app.ModelConfig{mainAlias: mainMC, altAlias: altMC},
		Agent:        app.AgentConfig{MaxSteps: 1},
	}
	mainProvider, err := BuildProvider(mainMC, cfg.Provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := tools.NewRegistry()
	template := NewSubAgentWithCredential(cfg, mainMC, mainProvider, nil, "", base, "", nil, nil, nil)
	if err := base.Register(task.NewTool(template)); err != nil {
		t.Fatal(err)
	}
	builder := NewServeRunBuilder(cfg, settings.Settings{}, mainMC, mainProvider, nil, base, NewWorkspaceRegistry(""), nil)
	runner := builder.Build(conversation.RuntimeContext{
		Session:       &session.Session{ID: "alt", WorkspacePath: t.TempDir()},
		Model:         altAlias,
		ResolvedModel: altMC.Model,
	}).(*agent.Runner)

	rawTask, ok := runner.Tools.Get("task")
	if !ok {
		t.Fatal("task tool missing")
	}
	rebound := rawTask.(*task.Tool).Agent().(*SubAgent)
	if rebound.MC.Name != altAlias || rebound.MC.Model != altMC.Model {
		t.Fatalf("task model = %q/%q, want current turn %q/%q", rebound.MC.Name, rebound.MC.Model, altAlias, altMC.Model)
	}
	if rebound.Provider != runner.Model {
		t.Fatal("task without explicit subagent_model must inherit the current turn Provider")
	}

	cfg.Agent.SubagentModel = mainAlias
	explicitBuilder := NewServeRunBuilder(cfg, settings.Settings{}, mainMC, mainProvider, nil, base, NewWorkspaceRegistry(""), nil)
	explicitRunner := explicitBuilder.Build(conversation.RuntimeContext{
		Session:       &session.Session{ID: "explicit", WorkspacePath: t.TempDir()},
		Model:         altAlias,
		ResolvedModel: altMC.Model,
	}).(*agent.Runner)
	rawTask, _ = explicitRunner.Tools.Get("task")
	explicitSubagent := rawTask.(*task.Tool).Agent().(*SubAgent)
	if explicitSubagent.InitErr != nil {
		t.Fatalf("explicit subagent Alias: %v", explicitSubagent.InitErr)
	}
	if explicitSubagent.MC.Name != mainAlias || explicitSubagent.Provider == explicitRunner.Model {
		t.Fatal("explicit subagent Alias must resolve independently from the current turn")
	}

	cfg.Agent.SubagentModel = "provider.bWlzc2luZw.model.bWlzc2luZw"
	strictBuilder := NewServeRunBuilder(cfg, settings.Settings{}, mainMC, mainProvider, nil, base, NewWorkspaceRegistry(""), nil)
	strictRunner := strictBuilder.Build(conversation.RuntimeContext{
		Session:       &session.Session{ID: "strict", WorkspacePath: t.TempDir()},
		Model:         altAlias,
		ResolvedModel: altMC.Model,
	}).(*agent.Runner)
	rawTask, _ = strictRunner.Tools.Get("task")
	strictSubagent := rawTask.(*task.Tool).Agent().(*SubAgent)
	if strictSubagent.InitErr == nil {
		t.Fatal("unknown explicit subagent Alias must fail instead of falling back")
	}
}

type runtimeTestApprover struct{}

func (runtimeTestApprover) Approve(string, json.RawMessage) agent.Verdict {
	return agent.VerdictAllow
}
