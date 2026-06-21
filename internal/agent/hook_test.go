package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
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

func runWithHook(t *testing.T, hook ToolHook) (*recordingTool, *fakeHook) {
	t.Helper()
	rt := &recordingTool{} // "danger" (side-effecting)
	reg := tools.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("danger", "{}"),
		{Content: "ok", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, Approver: allowApprover{}, Hook: hook, MaxSteps: 5}
	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}
	return rt, hook.(*fakeHook)
}

func TestPreHookBlocksExecution(t *testing.T) {
	rt, hook := runWithHook(t, &fakeHook{block: true})
	if !hook.preCalled {
		t.Fatal("the pre-hook should be consulted before the tool")
	}
	if rt.ran {
		t.Fatal("a pre-hook block must prevent the tool from executing")
	}
	if hook.postCalled {
		t.Fatal("the post-hook must not run when the tool was blocked")
	}
}

func TestPostHookRunsAfterSuccess(t *testing.T) {
	rt, hook := runWithHook(t, &fakeHook{block: false})
	if !rt.ran {
		t.Fatal("the tool should run when the pre-hook allows it")
	}
	if !hook.postCalled || hook.postTool != "danger" {
		t.Fatalf("the post-hook should run after success: called=%v tool=%q", hook.postCalled, hook.postTool)
	}
}
