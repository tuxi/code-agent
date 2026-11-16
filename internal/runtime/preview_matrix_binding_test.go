package runtime

import (
	"context"
	"testing"

	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

// TestPreviewMatrixToolIsRegisteredAsServerBuiltin covers the shared daemon,
// serve and embedded assembly path: ui_preview_matrix is a direct, server-side
// built-in that the allowlist can gate like every other tool.
func TestPreviewMatrixToolIsRegisteredAsServerBuiltin(t *testing.T) {
	allow := []string{"ui_preview_matrix"}
	cfg := settings.Settings{Agent: settings.AgentConfig{BuiltinTools: &allow}}
	registry, _, _, _, err := BuildBaseRegistry(context.Background(), cfg, settings.ModelConfig{}, nil, nil, nil, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("BuildBaseRegistry: %v", err)
	}
	if got := visibleToolNames(registry); !equalStrings(got, []string{"ui_preview_matrix"}) {
		t.Fatalf("visible tools = %v", got)
	}
	binding, ok := registry.Binding("ui_preview_matrix")
	if !ok {
		t.Fatal("ui_preview_matrix binding is missing")
	}
	if binding.ModelVisibility != tools.VisibilityDirect ||
		binding.Executor != tools.ExecutorServer ||
		binding.Capability != "ui-adaptation" {
		t.Fatalf("binding = %+v", binding)
	}
}
