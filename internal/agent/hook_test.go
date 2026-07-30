package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

)

type fakeHook struct {
	block      bool
	preCalled  bool
	postCalled bool
	postTool   string
}

func (h *fakeHook) PreToolUse(context.Context, string, json.RawMessage) error {
	h.preCalled = true
	if h.block {
		return fmt.Errorf("blocked by policy")
	}
	return nil
}
func (h *fakeHook) PostToolUse(_ context.Context, tool string, _ json.RawMessage, _ string) error {
	h.postCalled, h.postTool = true, tool
	return nil
}

func TestPreHookBlocksExecution(t *testing.T) {
	hook := &fakeHook{block: true}
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("danger", map[string]any{}),
		FauxText("ok"),
	})
	tool := h.RegisterSideEffectingTool("danger", "a side-effecting tool", "did it")
	h.Runner.Approver = allowApprover{}
	h.Runner.Hook = hook

	if _, err := h.RunTurn("go"); err != nil {
		t.Fatal(err)
	}
	if !hook.preCalled {
		t.Fatal("the pre-hook should be consulted before the tool")
	}
	if tool.Ran {
		t.Fatal("a pre-hook block must prevent the tool from executing")
	}
	if hook.postCalled {
		t.Fatal("the post-hook must not run when the tool was blocked")
	}
}

func TestPostHookRunsAfterSuccess(t *testing.T) {
	hook := &fakeHook{block: false}
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("danger", map[string]any{}),
		FauxText("ok"),
	})
	tool := h.RegisterSideEffectingTool("danger", "a side-effecting tool", "did it")
	h.Runner.Approver = allowApprover{}
	h.Runner.Hook = hook

	if _, err := h.RunTurn("go"); err != nil {
		t.Fatal(err)
	}
	if !tool.Ran {
		t.Fatal("the tool should run when the pre-hook allows it")
	}
	if !hook.postCalled || hook.postTool != "danger" {
		t.Fatalf("the post-hook should run after success: called=%v tool=%q", hook.postCalled, hook.postTool)
	}
}
