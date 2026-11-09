package runtime

import (
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/capability"
	"code-agent/internal/conversation"
	"code-agent/internal/preview"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

func TestServeRunBuilderAppliesCapabilityBindingsToClientTools(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	base := tools.NewRegistry()
	sharedRef := WirePlanTools(base, t.TempDir())
	builder := NewServeRunBuilder(settings.Settings{}, settings.ModelConfig{}, nil, nil, base, NewWorkspaceRegistry(""), sharedRef)
	manifest := capability.Manifest{
		Name: "ui-adaptation", Version: "1", Description: "preview capability",
		ToolBindings: []capability.ToolBinding{
			{Name: "hidden_capture", ModelVisibility: tools.VisibilityHidden, Executor: tools.ExecutorClient},
			{Name: "ui_preview", ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorFlex},
		},
	}
	if _, err := builder.Capabilities().Activate(manifest, "project", nil); err != nil {
		t.Fatal(err)
	}
	store, err := preview.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := preview.NewService(store)
	builder.SetPreviewService(service)
	runner := builder.Build(conversation.RuntimeContext{
		Session: &session.Session{ID: "session_preview", WorkspacePath: t.TempDir()},
		ClientTools: []agent.ClientToolDef{
			{Name: "hidden_capture", Description: "hidden", InputSchema: json.RawMessage(`{"type":"object"}`), ModelVisibility: "hidden", Executor: "client"},
			{Name: "ui_preview", Description: "preview", InputSchema: json.RawMessage(`{"type":"object"}`), ModelVisibility: "direct", Executor: "flex"},
		},
	}).(*agent.Runner)
	if runner.PreviewProducer == nil {
		t.Fatal("preview producer was not wired into the runner")
	}
	if _, ok := runner.Tools.Get("hidden_capture"); !ok {
		t.Fatal("hidden client tool is not runtime-resolvable")
	}
	visible := runner.Tools.Visible()
	foundPreview := false
	for _, tool := range visible {
		if tool.Name() == "hidden_capture" {
			t.Fatal("hidden client tool leaked into the model-visible tool list")
		}
		if tool.Name() == "ui_preview" {
			foundPreview = true
		}
	}
	if !foundPreview {
		t.Fatal("direct client tool was not model-visible")
	}
	hiddenBinding, ok := runner.Tools.Binding("hidden_capture")
	if !ok || hiddenBinding.ModelVisibility != tools.VisibilityHidden || hiddenBinding.Executor != tools.ExecutorClient || hiddenBinding.Capability != "ui-adaptation" {
		t.Fatalf("hidden binding = %+v/%v", hiddenBinding, ok)
	}
	flexBinding, ok := runner.Tools.Binding("ui_preview")
	if !ok || flexBinding.ModelVisibility != tools.VisibilityDirect || flexBinding.Executor != tools.ExecutorFlex || flexBinding.Capability != "ui-adaptation" {
		t.Fatalf("flex binding = %+v/%v", flexBinding, ok)
	}
}
