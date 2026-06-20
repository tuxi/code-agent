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
