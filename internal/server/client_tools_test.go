package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/agent"
)

type registerCaptureCommands struct {
	fakeCommands
	defs []agent.ClientToolDef
}

func (c *registerCaptureCommands) RegisterTools(defs []agent.ClientToolDef) { c.defs = defs }

func TestRegisterToolsPreservesVisibilityAndExecutor(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1", "tool-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		RegisterTools json.RawMessage `json:"register_tools"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	commands := &registerCaptureCommands{fakeCommands: *newFakeCommands()}
	Router{Commands: commands}.Route(context.Background(), fixture.RegisterTools)
	if len(commands.defs) != 2 {
		t.Fatalf("registered defs = %+v", commands.defs)
	}
	if got := commands.defs[0]; got.Name != "ios_preview_capture" || got.ModelVisibility != "hidden" || got.Executor != "client" {
		t.Fatalf("hidden binding = %+v", got)
	}
	if got := commands.defs[1]; got.Name != "ui_preview" || got.ModelVisibility != "direct" || got.Executor != "flex" {
		t.Fatalf("flex binding = %+v", got)
	}
}

func TestRegisterToolsLegacyDefaultsAndValidation(t *testing.T) {
	commands := &registerCaptureCommands{fakeCommands: *newFakeCommands()}
	Router{Commands: commands}.Route(context.Background(), []byte(`{"type":"register_tools","tools":[{"name":"legacy","description":"legacy","input_schema":{"type":"object"}}]}`))
	if len(commands.defs) != 1 || commands.defs[0].ModelVisibility != "direct" || commands.defs[0].Executor != "client" {
		t.Fatalf("legacy defaults = %+v", commands.defs)
	}
	commands.defs = nil
	Router{Commands: commands}.Route(context.Background(), []byte(`{"type":"register_tools","tools":[{"name":"x","description":"x","input_schema":{},"producer_id":"forged"}]}`))
	if len(commands.defs) != 0 {
		t.Fatalf("unknown registration field was accepted: %+v", commands.defs)
	}
	Router{Commands: commands}.Route(context.Background(), []byte(`{"type":"register_tools","tools":[{"name":"x","description":"x","input_schema":{},"model_visibility":"hidden","executor":"client"},{"name":"x","description":"x","input_schema":{},"model_visibility":"direct","executor":"client"}]}`))
	if len(commands.defs) != 0 {
		t.Fatalf("duplicate registration was accepted: %+v", commands.defs)
	}
}

func TestClientToolResultPreviewsReachWaiter(t *testing.T) {
	waiter := &fakeToolResults{called: make(chan struct{}, 1)}
	Router{ToolResults: waiter}.Route(context.Background(), []byte(`{
		"type":"agent_input","kind":"tool_result",
		"tool_result":{"tool_use_id":"call_1","subtype":"result","content":"ok","previews":[{"artifact_key":"typed","kind":"interactive_html","title":"Typed"}]}
	}`))
	<-waiter.called
	if len(waiter.result.Previews) != 1 || waiter.result.Previews[0].ArtifactKey != "typed" {
		t.Fatalf("client previews = %+v", waiter.result.Previews)
	}
}

func TestClientToolPreviewRejectsForgedIdentity(t *testing.T) {
	for _, payload := range []string{
		`{"type":"agent_input","kind":"tool_result","tool_result":{"tool_use_id":"call","subtype":"result","previews":[{"artifact_key":"x","kind":"interactive_html","title":"x","producer_id":"forged"}]}}`,
		`{"type":"agent_input","kind":"tool_result","tool_result":{"tool_use_id":"call","subtype":"result","producer_id":"forged"}}`,
		`{"type":"agent_input","kind":"tool_result","tool_result":{"tool_use_id":"call","subtype":"result","previews":[{"artifact_key":"x","kind":"interactive_html","title":"x","capability":"forged"}]}}`,
	} {
		var input AgentInput
		if err := json.Unmarshal([]byte(payload), &input); err == nil {
			t.Fatalf("forged client identity was accepted: %s", payload)
		}
	}
}
