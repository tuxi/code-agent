package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"code-agent/internal/sandbox"
	"code-agent/internal/tools"
)

func TestAwkWorkspaceCheckDistinguishesProgramAndFileOperands(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	quoted := strconv.Quote(outside)
	for _, tc := range []struct {
		name, command string
		blocked       bool
	}{
		{"range program", `awk '/Undefined symbols for architecture arm64/,/ld: symbol\(s\) not found/' mmo_debug.log`, false},
		{"separator", `awk -F / '/start/' mmo_debug.log`, false},
		{"attached separator", `awk -F/ '/start/' mmo_debug.log`, false},
		{"variable", `awk -v value=/not-a-file '/start/' mmo_debug.log`, false},
		{"attached variable", `awk -vvalue=/not-a-file '/start/' mmo_debug.log`, false},
		{"end options", `awk -- '/start/' mmo_debug.log`, false},
		{"local script", `awk -f filter.awk mmo_debug.log`, false},
		{"attached local script", `awk -ffilter.awk mmo_debug.log`, false},
		{"outside input", `awk '/start/' ` + quoted, true},
		{"outside input after script", `awk -f filter.awk ` + quoted, true},
		{"outside input after end options", `awk -- '/start/' ` + quoted, true},
		{"flag-like data after program", `awk '/start/' -v ` + quoted, true},
		{"flag-like data after input", `awk -f filter.awk mmo_debug.log -F ` + quoted, true},
		{"outside script", `awk -f ` + quoted + ` mmo_debug.log`, true},
		{"attached outside script", `awk -f` + quoted + ` mmo_debug.log`, true},
		{"multiple scripts", `awk -f filter.awk -f ` + quoted + ` mmo_debug.log`, true},
		{"parent input", `awk '/start/' ../outside.log`, true},
		{"parent script", `awk -f../outside.awk mmo_debug.log`, true},
		{"managed worktree input", `awk '/start/' .codeagent/worktrees/other/input.log`, true},
		{"managed worktree script", `awk -f .codeagent/worktrees/other/filter.awk mmo_debug.log`, true},
		{"unknown option conservative", `awk -W unknown ` + quoted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := sandbox.SplitArgs(tc.command)
			if err != nil {
				t.Fatal(err)
			}
			if got := outsideWorkspaceRead(args, root); (got != "") != tc.blocked {
				t.Fatalf("command=%q refusal=%q want blocked=%v", tc.command, got, tc.blocked)
			}
		})
	}
}

func TestRunCommandAwkDiagnosticPipeline(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mmo_debug.log"), []byte("Undefined symbols for architecture arm64:\n  \"MissingSymbol\", referenced from:\nld: symbol(s) not found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := `grep -n "Undefined symbols\|ld: library\|duplicate symbol" mmo_debug.log | tail -5; echo "=== symbols"; awk '/Undefined symbols for architecture arm64/,/ld: symbol\(s\) not found/' mmo_debug.log | grep -E '^  "' | sed 's/^  //' | head -40`
	input, _ := json.Marshal(runCommandInput{Command: command})
	result, err := NewRunCommandTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, input)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeResult(t, result.Content)
	if result.Failure != nil || out.ExitCode != 0 || !strings.Contains(out.Stdout, "MissingSymbol") {
		t.Fatalf("diagnostic pipeline refused or failed: %+v %+v", result.Failure, out)
	}
}

func TestCompoundAwkStillRefusesOutsideFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.awk")
	if err := os.WriteFile(outside, []byte("BEGIN { print \"must-not-execute\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`awk -f ` + strconv.Quote(outside) + `; echo trailing`,
		`awk '/start/' ` + strconv.Quote(outside) + ` | head`,
		`sh -c ` + strconv.Quote(`awk -f `+outside) + ` && true`,
	} {
		input, _ := json.Marshal(runCommandInput{Command: command})
		result, err := NewRunCommandTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, input)
		if err != nil {
			t.Fatal(err)
		}
		out := decodeResult(t, result.Content)
		if result.Failure == nil || result.Failure.Kind != "blocked" || out.Stdout != "" {
			t.Fatalf("outside file reached execution: %+v", out)
		}
	}
}
