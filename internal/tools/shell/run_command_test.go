package shell

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func decodeResult(t *testing.T, content string) commandResult {
	t.Helper()
	var r commandResult
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, content)
	}
	return r
}

func TestRunCommandAllowedReadOnly(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (note=%q stderr=%q)", r.ExitCode, r.Note, r.Stderr)
	}
	if r.Decision != "allow" {
		t.Errorf("decision = %q, want allow", r.Decision)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("stdout = %q, want it to contain hello", r.Stdout)
	}
	if r.Command != "echo hello" {
		t.Errorf("command = %q, want %q", r.Command, "echo hello")
	}
}

func TestRunCommandBlockedDoesNotExecute(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"rm -rf /"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.Decision != "block" {
		t.Errorf("decision = %q, want block", r.Decision)
	}
	if r.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", r.ExitCode)
	}
	if r.Stdout != "" {
		t.Errorf("blocked command produced stdout %q; it must never run", r.Stdout)
	}
}

func TestRunCommandShellOperatorsRejected(t *testing.T) {
	tool := NewRunCommandTool()
	// $(date) is still rejected (command substitution not supported).
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"echo $(date)"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", r.ExitCode)
	}
	if !strings.Contains(r.Note, "command substitution") {
		t.Errorf("note = %q, want it to mention command substitution", r.Note)
	}
}

// The rejection note is tailored to the operator used, so the model learns the
// concrete single-command alternative instead of retrying the same broken shape.
func TestRunCommandHintIsTailored(t *testing.T) {
	cases := []struct {
		command  string
		wantHint string
	}{
		{"cd cmd/foo && go vet", "path"},  // cd → "pass a path"
		{"echo $(date)", "command"},       // $() → "command substitution"
		{"echo `date`", "command"},        // backticks → "command substitution"
	}
	for _, c := range cases {
		if got := shellOperatorHint(c.command); !strings.Contains(got, c.wantHint) {
			t.Errorf("shellOperatorHint(%q) = %q, want to contain %q", c.command, got, c.wantHint)
		}
	}
}

func TestRunCommandSideEffectsFor(t *testing.T) {
	tool := NewRunCommandTool()
	cases := []struct {
		cmd  string
		want bool
	}{
		{"git status", false},      // read-only: no prompt
		{"go test ./...", false},   // build/test: no prompt
		{"rm file.txt", true},      // mutating: prompt
		{"git commit -m x", true},  // mutating: prompt
		{"rm -rf /", false},        // blocked: refused in Execute, so no prompt
		{"some-unknown-cmd", true}, // unknown: default to prompt
	}
	for _, tc := range cases {
		in := json.RawMessage(`{"command":` + strconv.Quote(tc.cmd) + `}`)
		if got := tool.SideEffectsFor(in); got != tc.want {
			t.Errorf("SideEffectsFor(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestRunCommandRefusesReadPathOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewRunCommandTool()
	in := json.RawMessage(`{"command":` + strconv.Quote("cat "+outside) + `}`)
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", r.ExitCode)
	}
	if !strings.Contains(r.Note, "inside the workspace") {
		t.Fatalf("note = %q, want workspace refusal", r.Note)
	}
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty because outside file must not be read", r.Stdout)
	}
}

func TestRunCommandStreamCallbacks(t *testing.T) {
	tool := NewRunCommandTool()

	var stdoutChunks, stderrChunks []string
	ec := tools.ExecutionContext{
		WorkspaceRoot: ".",
		OnStdout:      func(chunk string) { stdoutChunks = append(stdoutChunks, chunk) },
		OnStderr:      func(chunk string) { stderrChunks = append(stderrChunks, chunk) },
	}

	// echo writes to stdout and produces no stderr; each write triggers a callback.
	res, err := tool.Execute(context.Background(), ec, json.RawMessage(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", r.ExitCode)
	}

	// Verify stream callbacks were invoked.
	if len(stdoutChunks) == 0 {
		t.Error("OnStdout was never called; streaming callbacks broken")
	}
	joined := strings.Join(stdoutChunks, "")
	if !strings.Contains(joined, "hello") {
		t.Errorf("streamed stdout = %q, want it to contain hello", joined)
	}

	// Verify the final result still has the full output (buffer capture still works).
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("result stdout = %q, want it to contain hello", r.Stdout)
	}

	// stderr should have no chunks (echo writes nothing to stderr).
	if len(stderrChunks) != 0 {
		t.Errorf("unexpected stderr chunks: %q", stderrChunks)
	}
}

func TestRunCommandAllowsReadPathInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"command":"cat ok.txt"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (note=%q)", r.ExitCode, r.Note)
	}
	if !strings.Contains(r.Stdout, "yes") {
		t.Fatalf("stdout = %q, want file content", r.Stdout)
	}
}
