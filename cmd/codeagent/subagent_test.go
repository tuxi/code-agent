package main

import (
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// answerProvider returns a fixed text answer with no tool calls — the subagent's
// turn finishes immediately with this as its conclusion.
type answerProvider struct{ content string }

func (p answerProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{Content: p.content, FinishReason: "stop"}, nil
}

// loopingProvider always requests a tool, so a turn never converges and runs to
// the step limit — exercising the non-convergence path.
type loopingProvider struct{}

func (loopingProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{ToolCalls: []model.ToolCall{{
		ID: "c1", Type: "function",
		Function: model.FunctionCall{Name: "nope", Arguments: "{}"},
	}}}, nil
}

type namedTool struct{ name string }

func (n namedTool) Name() string                 { return n.name }
func (n namedTool) Description() string          { return "" }
func (n namedTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n namedTool) Execute(context.Context, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{}, nil
}

func testSubAgent(provider model.Provider, root string) *subAgent {
	return &subAgent{
		root:     root,
		provider: provider,
		mc:       app.ModelConfig{Name: "test", Model: "test-model", ContextWindow: 128000, Temperature: 0.2},
		cfg:      app.Config{Agent: app.AgentConfig{CompactRatio: 0.5}},
		readOnly: tools.NewRegistry(), // empty: the fake model ignores tools
	}
}

func TestSubAgentRunReturnsConclusion(t *testing.T) {
	sa := testSubAgent(answerProvider{content: "root cause: nil deref at loop.go:42"}, t.TempDir())
	out, err := sa.Run(context.Background(), "why does X fail?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "root cause: nil deref at loop.go:42" {
		t.Fatalf("conclusion = %q", out)
	}
}

func TestSubAgentRunFlagsNonConvergence(t *testing.T) {
	sa := testSubAgent(loopingProvider{}, t.TempDir())
	out, err := sa.Run(context.Background(), "dig forever")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "did not converge") {
		t.Fatalf("a non-convergent run should be flagged, got: %q", out)
	}
}

func TestNewSubAgentReadOnlySetIsFailClosed(t *testing.T) {
	full := tools.NewRegistry()
	for _, name := range []string{"read_file", "grep", "edit_file", "run_command", "git_commit"} {
		_ = full.Register(namedTool{name})
	}
	sa := newSubAgent(app.Config{}, app.ModelConfig{Name: "m"}, answerProvider{}, t.TempDir(), full, "")

	for _, want := range []string{"read_file", "grep"} {
		if _, ok := sa.readOnly.Get(want); !ok {
			t.Errorf("read-only tool %q should be in the subagent set", want)
		}
	}
	for _, banned := range []string{"edit_file", "run_command", "git_commit", "task"} {
		if _, ok := sa.readOnly.Get(banned); ok {
			t.Errorf("side-effecting tool %q must NOT be in the subagent set", banned)
		}
	}
}

func TestDenyAllRefusesEverything(t *testing.T) {
	if (denyAll{}).Approve("edit_file", json.RawMessage(`{}`)) {
		t.Fatal("the fail-closed approver must deny every call")
	}
}

func TestResolveSubAgentModelInheritsWhenUnset(t *testing.T) {
	parent := answerProvider{content: "x"}
	mc := app.ModelConfig{Name: "main", Model: "main-model"}
	prov, gotMC := resolveSubAgentModel(app.Config{}, mc, parent)
	if gotMC.Name != "main" {
		t.Fatalf("unset subagent_model should inherit the parent, got %q", gotMC.Name)
	}
	if prov != model.Provider(parent) {
		t.Fatal("unset subagent_model should reuse the parent provider")
	}
}

func TestResolveSubAgentModelFallsBackOnUnknown(t *testing.T) {
	parent := answerProvider{content: "x"}
	mc := app.ModelConfig{Name: "main", Model: "main-model"}
	cfg := app.Config{Agent: app.AgentConfig{SubagentModel: "ghost"}} // not in Models
	_, gotMC := resolveSubAgentModel(cfg, mc, parent)
	if gotMC.Name != "main" {
		t.Fatalf("an unknown subagent_model should fall back to the parent, got %q", gotMC.Name)
	}
}
