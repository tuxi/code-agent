package shell

import (
	"context"
	"encoding/json"
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
	tool := NewRunCommandTool(".")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hello"}`))
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
	tool := NewRunCommandTool(".")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"rm -rf /"}`))
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
	tool := NewRunCommandTool(".")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hi | grep h"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := decodeResult(t, res.Content)
	if r.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", r.ExitCode)
	}
	if !strings.Contains(r.Note, "not supported") {
		t.Errorf("note = %q, want it to explain operators are not supported", r.Note)
	}
}

func TestRunCommandSideEffectsFor(t *testing.T) {
	tool := NewRunCommandTool(".")
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
