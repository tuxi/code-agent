package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// fakeSkillTool implements tools.SkillAnnouncer so the loop should emit a
// versioned EventSkillLoaded after it runs.
type fakeSkillTool struct{}

func (fakeSkillTool) Name() string                 { return "load_skill" }
func (fakeSkillTool) Description() string          { return "loads a skill" }
func (fakeSkillTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (fakeSkillTool) Execute(context.Context, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "Loaded skill: verify-change (v1)\n\nbody"}, nil
}
func (fakeSkillTool) AnnounceSkill(json.RawMessage) (string, string, bool) {
	return "verify-change", "1", true
}

func TestSkillLoadedEvent(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(fakeSkillTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("load_skill", `{"name":"verify-change"}`),
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em}

	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}

	ev, ok := em.first(EventSkillLoaded)
	if !ok {
		t.Fatal("no EventSkillLoaded emitted")
	}
	if ev.ToolName != "verify-change" || ev.Version != "1" {
		t.Errorf("EventSkillLoaded = name %q version %q, want verify-change / 1", ev.ToolName, ev.Version)
	}
}
