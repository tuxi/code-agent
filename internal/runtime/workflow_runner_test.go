package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
)

func TestSaveToolSequencePersistsAndTriggerable(t *testing.T) {
	root := t.TempDir()
	// Seed a workflow DB so openFluxWorkflowRuntime finds a real store.
	seedFluxRun(t, root)

	runner := NewWorkflowRunner(nil)
	manifest := json.RawMessage(`{
		"type": "tool_sequence",
		"goal": "改简介",
		"description": "fixed steps",
		"inputs": [{"name": "bio", "type": "string", "required": true}],
		"steps": [
			{"tool": "mobile_launch_app", "args": {"bundle_id": "com.atebits.Tweetie2"}},
			{"tool": "mobile_type_keys", "args": {"text": "{{bio}}"}}
		]
	}`)
	name, err := runner.SaveToolSequence(context.Background(), root, "seq-tpl", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if name != "seq-tpl" {
		t.Fatalf("name=%s", name)
	}

	// The template must appear in the catalog as a template with its manifest.
	items, err := NewWorkflowListFunc()(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var found *WorkflowSummary
	for i := range items {
		if items[i].Name == "seq-tpl" {
			found = &items[i]
		}
	}
	if found == nil || !found.IsTemplate {
		t.Fatalf("template not found or not marked: %+v", items)
	}

	detail, err := NewWorkflowDetailFunc()(context.Background(), root, "seq-tpl")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Manifest) == 0 || detail.Versions == nil {
		t.Fatalf("detail=%+v", detail)
	}

	// The compiled DAG must be triggerable: Submit resolves the latest version
	// without error (workers are not started here, only the enqueue path).
	rt, err := openFluxWorkflowRuntime(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Shutdown()
	if _, err := rt.Submit(context.Background(), "seq-tpl", map[string]any{"bio": "new bio"}); err != nil {
		t.Fatalf("submit compiled template: %v", err)
	}
}

func TestSaveToolSequenceRejectsBadManifest(t *testing.T) {
	root := t.TempDir()
	seedFluxRun(t, root)
	runner := NewWorkflowRunner(nil)

	bad := []json.RawMessage{
		json.RawMessage(`{"type":"tool_sequence"}`),                                                           // no goal
		json.RawMessage(`{"type":"tool_sequence","goal":"g"}`),                                                // no steps
		json.RawMessage(`{"type":"tool_sequence","goal":"g","steps":[{"args":{}}]}`),                          // step missing tool
		json.RawMessage(`{"type":"tool_sequence","goal":"g","steps":[{"tool":"x","args":{"q":"{{nope}}"}}]}`), // undeclared var
	}
	for i, m := range bad {
		if _, err := runner.SaveToolSequence(context.Background(), root, "bad", m); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

var _ tools.WorkflowRunner = NewWorkflowRunner(nil)
