package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreToolUseBlocksOnNonZeroExit(t *testing.T) {
	r := New([]Hook{{Event: PreToolUse, Match: "run_command", Command: `echo "nope"; exit 1`}}, t.TempDir())
	err := r.PreToolUse(context.Background(), "run_command", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a non-zero pre-hook should block the call")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("the block reason should carry the hook's output, got %q", err.Error())
	}
}

func TestPreToolUseAllowsOnZeroExit(t *testing.T) {
	r := New([]Hook{{Event: PreToolUse, Match: "*", Command: "exit 0"}}, t.TempDir())
	if err := r.PreToolUse(context.Background(), "anything", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a zero-exit pre-hook should allow: %v", err)
	}
}

func TestPreToolUseSeesInputAndToolName(t *testing.T) {
	// A guard that reads the tool input on stdin and the tool name from the env.
	guard := `if grep -q "rm -rf"; then echo "blocked $CODEAGENT_TOOL_NAME"; exit 1; fi`
	r := New([]Hook{{Event: PreToolUse, Match: "run_command", Command: guard}}, t.TempDir())

	err := r.PreToolUse(context.Background(), "run_command", json.RawMessage(`{"command":"rm -rf /"}`))
	if err == nil {
		t.Fatal("should block a command containing rm -rf")
	}
	if !strings.Contains(err.Error(), "run_command") {
		t.Fatalf("reason should include the tool name from $CODEAGENT_TOOL_NAME: %q", err.Error())
	}
	if err := r.PreToolUse(context.Background(), "run_command", json.RawMessage(`{"command":"ls"}`)); err != nil {
		t.Fatalf("should allow a safe command: %v", err)
	}
}

func TestPostToolUseRunsInWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	r := New([]Hook{{Event: PostToolUse, Match: "edit_file", Command: "echo done > marker.txt"}}, dir)
	if err := r.PostToolUse(context.Background(), "edit_file", json.RawMessage(`{}`), "result"); err != nil {
		t.Fatalf("PostToolUse: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker.txt")); err != nil {
		t.Fatalf("the post-hook should have run in the workspace root: %v", err)
	}
}

func TestMatchedFiltersByEventAndTool(t *testing.T) {
	r := &Runner{hooks: []Hook{
		{Event: PreToolUse, Match: "edit_file"},
		{Event: PostToolUse, Match: "edit_file"},
		{Event: PreToolUse, Match: "*"},
	}}
	if got := len(r.matched(PreToolUse, "edit_file")); got != 2 { // exact + wildcard
		t.Fatalf("edit_file pre hooks = %d, want 2", got)
	}
	if got := len(r.matched(PreToolUse, "read_file")); got != 1 { // wildcard only
		t.Fatalf("read_file pre hooks = %d, want 1", got)
	}
	if got := len(r.matched(PostToolUse, "edit_file")); got != 1 {
		t.Fatalf("edit_file post hooks = %d, want 1", got)
	}
}

func TestNewReturnsNilWhenEmpty(t *testing.T) {
	if New(nil, "/tmp") != nil {
		t.Fatal("New(nil) must return nil so the loop's nil-safe path applies")
	}
}

// after_turn stop-policy contract (8.5): exit 0 + empty stdout accepts.
func TestStopDecideAcceptsOnEmptyOutput(t *testing.T) {
	r := New([]Hook{{Event: AfterTurn, Command: "exit 0"}}, t.TempDir())
	v := r.StopDecide(context.Background(), StopHookInput{LastText: "done"})
	if v.Continue {
		t.Fatalf("exit 0 + empty body must accept the finish, got %+v", v)
	}
}

// exit 0 + {"continue":true,"message":...} rejects and carries the nudge.
func TestStopDecideContinuesWithMessage(t *testing.T) {
	r := New([]Hook{{Event: AfterTurn, Command: `echo '{"continue":true,"message":"still have todo X"}'`}}, t.TempDir())
	v := r.StopDecide(context.Background(), StopHookInput{})
	if !v.Continue || v.Message != "still have todo X" {
		t.Fatalf("verdict = %+v, want continue+message", v)
	}
}

// exit 0 + {"continue":false} accepts (explicit).
func TestStopDecideAcceptsOnExplicitFalse(t *testing.T) {
	r := New([]Hook{{Event: AfterTurn, Command: `echo '{"continue":false}'`}}, t.TempDir())
	if v := r.StopDecide(context.Background(), StopHookInput{}); v.Continue {
		t.Fatalf("explicit continue:false must accept, got %+v", v)
	}
}

// non-zero exit FAILS CLOSED: the finish is rejected and stderr/stdout is the
// reason — a stop policy that errored must not silently accept a premature stop.
func TestStopDecideFailsClosedOnNonZeroExit(t *testing.T) {
	r := New([]Hook{{Event: AfterTurn, Command: `echo "policy crashed"; exit 1`}}, t.TempDir())
	v := r.StopDecide(context.Background(), StopHookInput{})
	if !v.Continue || !strings.Contains(v.Message, "policy crashed") {
		t.Fatalf("verdict = %+v, want fail-closed with the hook's output", v)
	}
}

// malformed stdout FAILS CLOSED too.
func TestStopDecideFailsClosedOnInvalidJSON(t *testing.T) {
	r := New([]Hook{{Event: AfterTurn, Command: `echo "not json"`}}, t.TempDir())
	v := r.StopDecide(context.Background(), StopHookInput{})
	if !v.Continue || !strings.Contains(v.Message, "invalid JSON") {
		t.Fatalf("verdict = %+v, want fail-closed on malformed output", v)
	}
}

// The snapshot arrives on stdin as JSON; Match is ignored for after_turn.
func TestStopDecideSeesInputAndIgnoresMatch(t *testing.T) {
	hook := `read -r line; echo "$line" | grep -q '"planState":"executing"' && echo '{"continue":false}' || echo '{"continue":true,"message":"wrong state"}'; exit 0`
	r := New([]Hook{{Event: AfterTurn, Match: "bash", Command: hook}}, t.TempDir())
	v := r.StopDecide(context.Background(), StopHookInput{PlanState: "executing", ToolCalls: 3})
	if v.Continue {
		t.Fatalf("hook saw planState=executing on stdin, should accept: %+v", v)
	}
}

// A StopHookInput todo round-trips onto stdin.
func TestStopDecideSeesTodos(t *testing.T) {
	hook := `read -r line; echo "$line" | grep -q '"status":"in_progress"' && echo '{"continue":true,"message":"incomplete"}' || exit 0`
	r := New([]Hook{{Event: AfterTurn, Command: hook}}, t.TempDir())
	v := r.StopDecide(context.Background(), StopHookInput{
		Todos: []StopHookTodo{{Content: "step", Status: "in_progress"}},
	})
	if !v.Continue || v.Message != "incomplete" {
		t.Fatalf("verdict = %+v, want continue on pending todo", v)
	}
}

func TestHasAfterTurn(t *testing.T) {
	if New([]Hook{{Event: PreToolUse, Match: "*", Command: "true"}}, t.TempDir()).HasAfterTurn() {
		t.Fatal("pre_tool_use hooks must not count as after_turn")
	}
	if !New([]Hook{{Event: AfterTurn, Command: "exit 0"}}, t.TempDir()).HasAfterTurn() {
		t.Fatal("an after_turn hook must be detected")
	}
}
