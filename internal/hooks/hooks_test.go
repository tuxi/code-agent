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

// ---- project_trust tests -----------------------------------------------

// Exit 0 with valid JSON decides trusted.
func TestProjectTrustDecidesTrusted(t *testing.T) {
	r := New([]Hook{{Event: ProjectTrust, Command: `echo '{"trusted":true,"reason":"corp policy"}'`}}, t.TempDir())
	v, err := r.ProjectTrustDecide(context.Background(), "/project", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Decided || !v.Trusted || v.Reason != "corp policy" {
		t.Fatalf("verdict = %+v, want decided+trusted", v)
	}
}

// Exit 0 with trusted:false decides untrusted.
func TestProjectTrustDecidesUntrusted(t *testing.T) {
	r := New([]Hook{{Event: ProjectTrust, Command: `echo '{"trusted":false,"reason":"blocked"}'`}}, t.TempDir())
	v, err := r.ProjectTrustDecide(context.Background(), "/project", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Decided || v.Trusted {
		t.Fatalf("verdict = %+v, want decided+untrusted", v)
	}
}

// Non-zero exit: hook is undecided (falls through).
func TestProjectTrustUndecidedOnNonZeroExit(t *testing.T) {
	r := New([]Hook{{Event: ProjectTrust, Command: `echo "oops"; exit 1`}}, t.TempDir())
	v, err := r.ProjectTrustDecide(context.Background(), "/project", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decided {
		t.Fatalf("verdict = %+v, want undecided on non-zero exit", v)
	}
}

// Empty stdout: hook is undecided.
func TestProjectTrustUndecidedOnEmptyOutput(t *testing.T) {
	r := New([]Hook{{Event: ProjectTrust, Command: `exit 0`}}, t.TempDir())
	v, err := r.ProjectTrustDecide(context.Background(), "/project", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decided {
		t.Fatalf("verdict = %+v, want undecided on empty stdout", v)
	}
}

// Malformed JSON: hook is undecided.
func TestProjectTrustUndecidedOnInvalidJSON(t *testing.T) {
	r := New([]Hook{{Event: ProjectTrust, Command: `echo "not json"`}}, t.TempDir())
	v, err := r.ProjectTrustDecide(context.Background(), "/project", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decided {
		t.Fatalf("verdict = %+v, want undecided on invalid JSON", v)
	}
}

// No project_trust hooks: undecided (not an error).
func TestProjectTrustNoHooks(t *testing.T) {
	r := New([]Hook{{Event: PreToolUse, Match: "*", Command: "true"}}, t.TempDir())
	v, err := r.ProjectTrustDecide(context.Background(), "/project", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Decided {
		t.Fatalf("verdict = %+v, want undecided with no project_trust hooks", v)
	}
}

func TestHasProjectTrustHook(t *testing.T) {
	if New([]Hook{{Event: PreToolUse, Match: "*", Command: "true"}}, t.TempDir()).HasProjectTrustHook() {
		t.Fatal("pre_tool_use hooks must not count as project_trust")
	}
	if !New([]Hook{{Event: ProjectTrust, Command: "exit 0"}}, t.TempDir()).HasProjectTrustHook() {
		t.Fatal("a project_trust hook must be detected")
	}
}

// ProjectTrust sees cwd and hasResources on stdin.
func TestProjectTrustSeesInput(t *testing.T) {
	hook := `read -r line; echo "$line" | grep -q '"hasResources":true' && echo '{"trusted":true,"reason":"ok"}' || echo '{"trusted":false,"reason":"no resources"}'; exit 0`
	r := New([]Hook{{Event: ProjectTrust, Command: hook}}, t.TempDir())
	v, _ := r.ProjectTrustDecide(context.Background(), "/my/project", true)
	if !v.Decided || !v.Trusted {
		t.Fatalf("verdict = %+v, want decided+trusted when hasResources=true", v)
	}
}

// ---- context tests -----------------------------------------------------

// Context hooks chain: each sees the previous output.
func TestContextHooksChainMessages(t *testing.T) {
	// H1 adds a prefix, H2 adds a suffix.
	h1v2 := `jq '.messages[0].content = "H1:" + .messages[0].content'`
	h2 := `jq '.messages[0].content = .messages[0].content + ":H2"'`
	r := New([]Hook{
		{Event: ContextPreRequest, Command: h1v2},
		{Event: ContextPreRequest, Command: h2},
	}, t.TempDir())
	msgs := []ContextHookMessage{{Role: "system", Content: "hello"}}
	result := r.RunContextHooks(context.Background(), msgs, "s1", "t1")
	if len(result) != 1 {
		t.Fatalf("got %d messages, want 1", len(result))
	}
	if result[0].Content != "H1:hello:H2" {
		t.Fatalf("content = %q, want H1:hello:H2 (chained)", result[0].Content)
	}
}

// Context hook preserves state on non-zero exit.
func TestContextHooksPreserveOnError(t *testing.T) {
	good := `jq '.messages[0].content = "good"'`
	bad := `echo "crashed"; exit 1`
	r := New([]Hook{
		{Event: ContextPreRequest, Command: good},
		{Event: ContextPreRequest, Command: bad},
	}, t.TempDir())
	msgs := []ContextHookMessage{{Role: "user", Content: "hi"}}
	result := r.RunContextHooks(context.Background(), msgs, "s1", "t1")
	if result[0].Content != "good" {
		t.Fatalf("content = %q, want 'good' (second hook errored, state preserved)", result[0].Content)
	}
}

// Context hook empty output is a no-op.
func TestContextHooksEmptyOutputIsNoop(t *testing.T) {
	h1 := `jq '.messages[0].content = "modified"'`
	empty := `exit 0`
	r := New([]Hook{
		{Event: ContextPreRequest, Command: h1},
		{Event: ContextPreRequest, Command: empty},
	}, t.TempDir())
	msgs := []ContextHookMessage{{Role: "user", Content: "hi"}}
	result := r.RunContextHooks(context.Background(), msgs, "s1", "t1")
	if result[0].Content != "modified" {
		t.Fatalf("content = %q, want 'modified' (empty output preserved state)", result[0].Content)
	}
}

func TestHasContextHook(t *testing.T) {
	if New([]Hook{{Event: PreToolUse, Match: "*", Command: "true"}}, t.TempDir()).HasContextHook() {
		t.Fatal("pre_tool_use hooks must not count as context")
	}
	if !New([]Hook{{Event: ContextPreRequest, Command: "exit 0"}}, t.TempDir()).HasContextHook() {
		t.Fatal("a context hook must be detected")
	}
}

// Context hooks receive session_id and turn_id.
func TestContextHooksSeeSessionAndTurn(t *testing.T) {
	hook := `read -r line; echo "$line" | grep -q '"session_id":"ses-123"' && echo '{"messages":[{"role":"user","content":"seen"}]}' || echo '{"messages":[{"role":"user","content":"missing"}]}'; exit 0`
	r := New([]Hook{{Event: ContextPreRequest, Command: hook}}, t.TempDir())
	result := r.RunContextHooks(context.Background(), []ContextHookMessage{{Role: "user", Content: "hi"}}, "ses-123", "t-456")
	if result[0].Content != "seen" {
		t.Fatalf("content = %q, want 'seen' (session_id was passed)", result[0].Content)
	}
}

// ---- post_tool_result tests -------------------------------------------

// PostToolResult modifies the content.
func TestPostToolResultModifiesContent(t *testing.T) {
	r := New([]Hook{{Event: PostToolResult, Match: "run_command", Command: `jq '.result.content = "[REDACTED]"'`}}, t.TempDir())
	content, isError, _, err := r.PostToolResult(context.Background(), "run_command", json.RawMessage(`{}`), "secret", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "[REDACTED]" {
		t.Fatalf("content = %q, want [REDACTED]", content)
	}
	if isError {
		t.Fatal("isError must stay false")
	}
}

// PostToolResult can flip isError.
func TestPostToolResultFlipsIsError(t *testing.T) {
	r := New([]Hook{{Event: PostToolResult, Match: "*", Command: `jq '.result.isError = true'`}}, t.TempDir())
	_, isError, _, err := r.PostToolResult(context.Background(), "anything", json.RawMessage(`{}`), "ok", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError {
		t.Fatal("isError must be flipped to true")
	}
}

// PostToolResult chains: each hook sees the previous output.
func TestPostToolResultChains(t *testing.T) {
	h1 := `jq '.result.content = "A"'`
	h2 := `jq '.result.content = .result.content + "B"'`
	r := New([]Hook{
		{Event: PostToolResult, Match: "test", Command: h1},
		{Event: PostToolResult, Match: "test", Command: h2},
	}, t.TempDir())
	content, _, _, _ := r.PostToolResult(context.Background(), "test", json.RawMessage(`{}`), "orig", false, nil)
	if content != "AB" {
		t.Fatalf("content = %q, want AB (chained)", content)
	}
}

// PostToolResult preserves original on error.
func TestPostToolResultPreservesOnError(t *testing.T) {
	good := `jq '.result.content = "changed"'`
	bad := `exit 1`
	r := New([]Hook{
		{Event: PostToolResult, Match: "*", Command: good},
		{Event: PostToolResult, Match: "*", Command: bad},
	}, t.TempDir())
	content, _, _, err := r.PostToolResult(context.Background(), "tool", json.RawMessage(`{}`), "original", false, nil)
	// The first hook succeeded, second errored -> result from first preserved.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "changed" {
		t.Fatalf("content = %q, want changed (second hook errored, first preserved)", content)
	}
}

// PostToolResult respects Match.
func TestPostToolResultRespectsMatch(t *testing.T) {
	r := New([]Hook{{Event: PostToolResult, Match: "edit_file", Command: `jq '.result.content = "edited"'`}}, t.TempDir())
	content, _, _, _ := r.PostToolResult(context.Background(), "read_file", json.RawMessage(`{}`), "original", false, nil)
	if content != "original" {
		t.Fatalf("content = %q, want original (hook matched edit_file, not read_file)", content)
	}
}
