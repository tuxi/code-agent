package capability

import (
	"reflect"
	"testing"

	"code-agent/internal/tools"
)

func TestDecodeRejectsUnknownManifestField(t *testing.T) {
	_, err := Decode([]byte(`{"name":"ui-adaptation","version":"1","description":"x","unexpected":true}`))
	if err == nil {
		t.Fatal("unknown manifest field was accepted")
	}
}

func TestManifestRequiresExplicitBindingDimensions(t *testing.T) {
	_, err := Decode([]byte(`{"name":"ui-adaptation","version":"1","description":"x","tool_bindings":[{"name":"ui_preview"}]}`))
	if err == nil {
		t.Fatal("binding without model_visibility/executor was accepted")
	}
}

func TestActivationResolvesUniqueAndAmbiguousCapabilityBindings(t *testing.T) {
	registry := NewRegistry()
	first := Manifest{
		Name: "cap-a", Version: "1", ClientCapabilities: []string{"preview_contract_v1", "preview_interactive_html_v1"},
		ToolBindings: []ToolBinding{{Name: "ui_preview", ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorFlex}},
	}
	second := Manifest{
		Name: "cap-b", Version: "1",
		ToolBindings: []ToolBinding{{Name: "ui_preview", ModelVisibility: tools.VisibilityHidden, Executor: tools.ExecutorClient}, {Name: "capture", ModelVisibility: tools.VisibilityHidden, Executor: tools.ExecutorClient}},
	}
	activation, err := registry.Activate(first, "project", []string{"preview_contract_v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activation.MissingClientCapabilities, []string{"preview_interactive_html_v1"}) {
		t.Fatalf("missing capabilities = %v", activation.MissingClientCapabilities)
	}
	if _, err := registry.Activate(second, "project", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Resolve("ui_preview", ""); ok {
		t.Fatalf("ambiguous tool resolved to %q", got)
	}
	if got, ok := registry.Resolve("ui_preview", "cap-b"); !ok || got != "cap-b" {
		t.Fatalf("context resolution = %q/%v", got, ok)
	}
	if got, ok := registry.Resolve("capture", ""); !ok || got != "cap-b" {
		t.Fatalf("unique hidden binding = %q/%v", got, ok)
	}
	binding := registry.Apply("ui_preview", "cap-a", tools.Binding{Name: "ui_preview", ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorFlex})
	if binding.Capability != "cap-a" {
		t.Fatalf("applied binding = %+v", binding)
	}
}
