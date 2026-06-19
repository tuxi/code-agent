package tui

import (
	"strings"
	"testing"
	"time"
)

func okTool(name, args string) Item {
	return Item{Kind: ItemTool, Name: name, Args: args, Status: StatusOK}
}

// The headline format the user asked for: a step prints as
// "Thought for Ns, read 1 file" with the real command beneath it.
func TestRenderStepHeaderAndCommand(t *testing.T) {
	s := stepBuf{active: true, elapsed: 2 * time.Second,
		tools: []Item{okTool("read_file", `{"path":"internal/agent/tool_defs.go"}`)}}
	out := strings.Join(renderStep(s, 80), "\n")

	if !strings.Contains(out, "Thought for 2s, read 1 file") {
		t.Fatalf("missing step header:\n%s", out)
	}
	if !strings.Contains(out, "Read(internal/agent/tool_defs.go)") {
		t.Fatalf("missing the actual command:\n%s", out)
	}
}

func TestSummarizeToolsCountsByKind(t *testing.T) {
	tools := []Item{
		okTool("read_file", `{"path":"a"}`),
		okTool("read_file", `{"path":"b"}`),
		okTool("run_command", `{"command":"go test"}`),
	}
	if got := summarizeTools(tools); got != "read 2 files, ran 1 command" {
		t.Fatalf("summary = %q", got)
	}
}

func TestToolActionShowsRealCommand(t *testing.T) {
	cases := []struct {
		in   Item
		want string
	}{
		{okTool("read_file", `{"path":"loop.go"}`), "Read(loop.go)"},
		{okTool("run_command", `{"command":"go test ./..."}`), "$ go test ./..."},
		{okTool("grep", `{"pattern":"Emitter"}`), "Grep(Emitter)"},
		{Item{Kind: ItemSkill, Name: "verify-change", Version: "2"}, "◆ skill verify-change v2"},
	}
	for _, c := range cases {
		if got := toolAction(c.in); !strings.Contains(got, c.want) {
			t.Errorf("toolAction(%s) = %q, want to contain %q", c.in.Name, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	if got := humanDuration(2 * time.Second); got != "2s" {
		t.Errorf("2s → %q", got)
	}
	if got := humanDuration(500 * time.Millisecond); got != "<1s" {
		t.Errorf("sub-second → %q", got)
	}
}

// A failure prints its body — it is the signal.
func TestStepDetailShowsFailureBody(t *testing.T) {
	fail := Item{Kind: ItemTool, Name: "run_command", Args: `{"command":"go test"}`,
		Status: StatusFail, Failure: "test", Text: "FAILED: boom"}
	out := strings.Join(toolDetailLines(fail, 80), "\n")
	if !strings.Contains(out, "boom") {
		t.Fatalf("a failed tool should print its body:\n%s", out)
	}
}

// A mutation tool (edit/create/apply/commit) prints its body even on success —
// the user needs to see what changed, same as the old REPL.
func TestMutationToolShowsBodyOnSuccess(t *testing.T) {
	for _, name := range []string{"edit_file", "create_file", "apply_patch", "git_commit"} {
		it := okTool(name, `{"path":"loop.go"}`)
		it.Text = "THE_DIFF_CONTENT"
		out := strings.Join(toolDetailLines(it, 80), "\n")
		if !strings.Contains(out, "THE_DIFF_CONTENT") {
			t.Errorf("%s (mutation) should show its body on success:\n%s", name, out)
		}
	}
}

// A read-only tool hides its body on success — only the command line shows.
func TestReadOnlyToolHidesBodyOnSuccess(t *testing.T) {
	ok := okTool("read_file", `{"path":"x"}`)
	ok.Text = "SECRET_BODY"
	out := strings.Join(toolDetailLines(ok, 80), "\n")
	if strings.Contains(out, "SECRET_BODY") {
		t.Fatalf("a successful read_file should not print its body:\n%s", out)
	}
}

func TestEmptyStepRendersNothing(t *testing.T) {
	if got := renderStep(stepBuf{}, 80); got != nil {
		t.Fatalf("an inactive/empty step should render nothing, got %v", got)
	}
}
