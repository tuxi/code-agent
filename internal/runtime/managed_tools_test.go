package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/tools"
	"code-agent/internal/tools/shell"
)

type managedForbiddenTool struct{ name string }

func (t managedForbiddenTool) Name() string      { return t.name }
func (managedForbiddenTool) Description() string { return "fixture" }
func (managedForbiddenTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (managedForbiddenTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	panic("forbidden tool executed")
}

func TestManagedToolProfilePreservesOrdinaryRegistryAndRejectsDetachedWork(t *testing.T) {
	base := tools.NewRegistry()
	command := shell.NewRunCommandTool()
	if err := base.Register(command); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"task", "plan_workflow", "send_to_session", "client_proxy", "external_mcp"} {
		if err := base.Register(managedForbiddenTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	managed := managedTurnTools(base)
	for _, name := range []string{"task", "plan_workflow", "send_to_session", "client_proxy", "external_mcp"} {
		if _, ok := managed.Get(name); ok {
			t.Fatal("execution domain escaped", name)
		}
		if _, ok := base.Get(name); !ok {
			t.Fatal("ordinary registry changed", name)
		}
	}
	raw, ok := managed.Get("run_command")
	if !ok {
		t.Fatal("foreground command tool missing")
	}
	selected := raw.(*shell.RunCommandTool)
	if selected == command || !selected.ForegroundOnly || command.ForegroundOnly || selected.Timeout != command.Timeout || selected.MaxOutputBytes != command.MaxOutputBytes {
		t.Fatal("shared tool mutated or original execution config replaced")
	}
	root := t.TempDir()
	_, err := selected.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"command":"touch marker","background":true}`))
	if err == nil {
		t.Fatal("background execution accepted")
	}
	if _, err = os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("side effect before rejection", err)
	}
}
