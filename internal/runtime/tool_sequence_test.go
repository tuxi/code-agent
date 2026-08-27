package runtime

import (
	"encoding/json"
	"testing"

	"github.com/tuxi/flux-workflow/definition"
)

func TestCompileToolSequenceSerialDAG(t *testing.T) {
	m := &ToolSequenceManifest{
		Type: "tool_sequence",
		Goal: "在 X app 上改简介",
		Inputs: []ToolSequenceInput{
			{Name: "bio", Type: "string", Required: true},
		},
		Steps: []ToolSequenceStep{
			{Tool: "mobile_launch_app", Args: map[string]any{"bundle_id": "com.atebits.Tweetie2"}},
			{Tool: "mobile_click_on_screen_at_coordinates", Args: map[string]any{"x": 30, "y": 30}},
			{Tool: "mobile_type_keys", Args: map[string]any{"text": "{{bio}}"}},
		},
	}
	def, err := CompileToolSequence(m)
	if err != nil {
		t.Fatal(err)
	}
	// start + 3 steps + end = 5 nodes; 5 serial edges.
	if len(def.Nodes) != 5 {
		t.Fatalf("nodes=%d, want 5", len(def.Nodes))
	}
	if len(def.Edges) != 5 {
		t.Fatalf("edges=%d, want 5", len(def.Edges))
	}
	// Node order and types.
	wantTypes := []definition.NodeType{definition.NodeStart, definition.NodeTool, definition.NodeTool, definition.NodeTool, definition.NodeEnd}
	for i, nd := range def.Nodes {
		if nd.Type != wantTypes[i] {
			t.Fatalf("node %d type=%s, want %s", i, nd.Type, wantTypes[i])
		}
	}
	// The variable step carries an input_mapping to the declared input.
	last := def.Nodes[3]
	if last.InputMapping["text"] != "input.bio" {
		t.Fatalf("input_mapping=%v, want text->input.bio", last.InputMapping)
	}
	if last.Config["tool"] != "mobile_type_keys" {
		t.Fatalf("config=%v", last.Config)
	}
	// Static args stay in Config (not InputMapping).
	if _, ok := def.Nodes[1].InputMapping["bundle_id"]; ok {
		t.Fatal("static arg must not be in input_mapping")
	}
	if def.Nodes[1].Config["bundle_id"] != "com.atebits.Tweetie2" {
		t.Fatalf("static arg not in config: %v", def.Nodes[1].Config)
	}
}

func TestCompileToolSequenceValidation(t *testing.T) {
	cases := []struct {
		name string
		m    *ToolSequenceManifest
	}{
		{"missing goal", &ToolSequenceManifest{Steps: []ToolSequenceStep{{Tool: "x"}}}},
		{"no steps", &ToolSequenceManifest{Goal: "g"}},
		{"step missing tool", &ToolSequenceManifest{Goal: "g", Steps: []ToolSequenceStep{{Args: map[string]any{}}}}},
		{"undeclared var", &ToolSequenceManifest{
			Goal:  "g",
			Steps: []ToolSequenceStep{{Tool: "x", Args: map[string]any{"q": "{{undeclared}}"}}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CompileToolSequence(tc.m); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestToolSequenceFromManifestJSON(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "tool_sequence",
		"goal": "g",
		"steps": [{"tool": "read_file", "args": {"path": "/tmp/a.txt"}}]
	}`)
	def, manifest, err := toolSequenceFromManifestJSON(raw, "my-template")
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "my-template" || manifest.Goal != "g" || len(def.Nodes) != 3 {
		t.Fatalf("def=%+v manifest=%+v", def, manifest)
	}
}
