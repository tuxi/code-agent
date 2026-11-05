package runtime

import (
	"fmt"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

func TestCodingProfileKeepsCoreAndIsolatesExtensions(t *testing.T) {
	reg := tools.NewRegistry()
	for _, name := range codingToolNames {
		if err := reg.Register(statelessTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 116; i++ {
		if err := reg.Register(statelessTool{name: fmt.Sprintf("extension_%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	enabled := true
	r := BuildRunner(settings.Settings{Agent: settings.AgentConfig{CodingTools: &enabled}}, settings.ModelConfig{}, nil, reg, nil, nil, nil, nil, "")
	if len(r.Tools.Visible()) != len(codingToolNames) || len(reg.Visible()) != len(codingToolNames)+116 {
		t.Fatal("profile mutated registry or leaked extensions")
	}
	if _, ok := r.Tools.Get("run_command"); !ok {
		t.Fatal("command requires discovery")
	}
	if _, ok := r.Tools.Get("job_wait"); !ok {
		t.Fatal("background control unavailable")
	}
	if _, ok := r.PlanTools.Get("run_command"); ok {
		t.Fatal("plan mode gained command execution")
	}
	full := BuildRunner(settings.Settings{}, settings.ModelConfig{}, nil, reg, nil, nil, nil, nil, "")
	if full.Tools != reg {
		t.Fatal("default profile changed")
	}
}

func TestServeCodingProfileFiltersAfterClientMerge(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	reg := tools.NewRegistry()
	_ = reg.Register(statelessTool{name: "read_file"})
	ref := WirePlanTools(reg, t.TempDir())
	enabled := true
	b := NewServeRunBuilder(settings.Settings{Agent: settings.AgentConfig{CodingTools: &enabled}}, settings.ModelConfig{}, nil, nil, reg, NewWorkspaceRegistry(""), ref)
	r := b.Build(conversation.RuntimeContext{Session: &session.Session{ID: "coding", WorkspacePath: t.TempDir()}, ClientTools: []agent.ClientToolDef{{Name: "extension", Description: "client", InputSchema: []byte(`{"type":"object"}`)}}}).(*agent.Runner)
	if _, ok := r.Tools.Get("extension"); ok {
		t.Fatal("merged client tool leaked into coding profile")
	}
	if _, ok := r.Tools.Get("read_file"); !ok {
		t.Fatal("core lost after merge")
	}
}
