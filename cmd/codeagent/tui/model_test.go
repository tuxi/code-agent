package tui

import (
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

func newTestModel() model {
	return newModel(NewBackend(), HeaderInfo{}, sessionSource{})
}

// fakePromptOps records the Render call for the MCP prompt submit path.
type fakePromptOps struct {
	command string
	args    []string
	text    string
	err     error
}

func (f *fakePromptOps) Help() string { return "PROMPT HELP" }
func (f *fakePromptOps) Render(command string, args []string) (string, error) {
	f.command, f.args = command, args
	return f.text, f.err
}

func asModel(t *testing.T, tm tea.Model) model {
	t.Helper()
	m, ok := tm.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", tm)
	}
	return m
}

func TestModelStartFinishTogglesThinking(t *testing.T) {
	m := newTestModel()
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted}))))
	if !m.thinking {
		t.Fatal("EventModelStarted should set thinking")
	}
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelFinished}))))
	if m.thinking {
		t.Fatal("EventModelFinished should clear thinking")
	}
}

func TestDoneClearsBusy(t *testing.T) {
	m := newTestModel()
	m.busy = true
	m = asModel(t, must(m.Update(doneMsg{})))
	if m.busy {
		t.Fatal("doneMsg should free the composer (busy=false)")
	}
}

func TestApprovalApprove(t *testing.T) {
	m := newTestModel()
	reply := make(chan bool, 1)
	req := approvalReq{tool: "create_file", input: json.RawMessage(`{"path":"x"}`), reply: reply}

	m = asModel(t, must(m.Update(approvalMsg(req))))
	if m.pending == nil {
		t.Fatal("approvalMsg should set a pending request")
	}
	m = asModel(t, must(m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})))
	if m.pending != nil {
		t.Fatal("answering should clear pending")
	}
	if !<-reply {
		t.Fatal("'y' should approve the tool")
	}
}

func TestApprovalDeny(t *testing.T) {
	m := newTestModel()
	reply := make(chan bool, 1)
	m.pending = &approvalReq{tool: "run_command", reply: reply}
	m = asModel(t, must(m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEsc})))
	if m.pending != nil {
		t.Fatal("esc should clear pending")
	}
	if <-reply {
		t.Fatal("esc should deny the tool")
	}
}

// fakeGranter records the AllowAlways call for the card's "always allow" choice.
type fakeGranter struct{ tool string }

func (g *fakeGranter) AllowAlways(tool string) (string, error) {
	g.tool = tool
	return tool, nil
}

// Submitting /mcp__server__prompt renders via PromptOps (off-UI), then feeds the
// rendered text into the turn path.
func TestSubmitMCPPromptRendersAndRuns(t *testing.T) {
	m := newTestModel()
	f := &fakePromptOps{text: "rendered prompt body"}
	m.src.promptOps = f
	m.composer.SetValue("/mcp__gh__pr_review 456 deep")

	tm, cmd := m.submit()
	m = asModel(t, tm)
	if !m.busy || cmd == nil {
		t.Fatalf("submit should lock busy and return a render cmd (busy=%v cmd=%v)", m.busy, cmd != nil)
	}
	// The render cmd calls PromptOps.Render with the parsed command + positional args.
	rendered, ok := cmd().(promptRenderedMsg)
	if !ok {
		t.Fatalf("render cmd should yield promptRenderedMsg")
	}
	if f.command != "mcp__gh__pr_review" || len(f.args) != 2 || f.args[0] != "456" || f.args[1] != "deep" {
		t.Fatalf("Render got command=%q args=%v", f.command, f.args)
	}
	if rendered.text != "rendered prompt body" {
		t.Fatalf("rendered text = %q", rendered.text)
	}
	// Update feeds the rendered text into b.inputs (buffered, cap 1).
	_, cmd2 := m.Update(rendered)
	if cmd2 == nil {
		t.Fatal("promptRenderedMsg should return a cmd feeding inputs")
	}
	cmd2()
	if got := <-m.b.inputs; got != "rendered prompt body" {
		t.Fatalf("inputs got %q", got)
	}
}

// A render error unlocks busy and reports; no MCP wired shows a notice.
func TestSubmitMCPPromptErrorAndUnavailable(t *testing.T) {
	m := newTestModel() // promptOps nil
	m.composer.SetValue("/mcp__x__y")
	tm, _ := m.submit()
	if asModel(t, tm).busy {
		t.Fatal("no promptOps: submit must not leave the composer busy")
	}
	if m.promptHelp() != "(no MCP prompts available)" {
		t.Fatalf("nil promptOps help = %q", m.promptHelp())
	}
}

func TestApprovalAlwaysGrants(t *testing.T) {
	m := newTestModel()
	g := &fakeGranter{}
	m.src.granter = g
	reply := make(chan bool, 1)
	m.pending = &approvalReq{tool: "mcp__github__list_issues", reply: reply}

	// 'a' = always allow: approves this call AND persists a rule via the granter.
	m = asModel(t, must(m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})))
	if !<-reply {
		t.Fatal("'a' should approve the tool")
	}
	if g.tool != "mcp__github__list_issues" {
		t.Fatalf("'a' should persist an always-allow rule, granter saw %q", g.tool)
	}
}

func TestSubmitLocksBusyAndDeliversInput(t *testing.T) {
	m := newTestModel()
	m.composer.SetValue("  fix the test  ")
	tm, cmd := m.submit()
	m = asModel(t, tm)
	if !m.busy {
		t.Fatal("submit should lock the composer")
	}
	if cmd == nil {
		t.Fatal("submit should return a cmd that delivers the input")
	}
	cmd() // delivers to the (buffered) inputs channel
	select {
	case got := <-m.b.inputs:
		if got != "fix the test" {
			t.Fatalf("delivered input = %q, want trimmed", got)
		}
	default:
		t.Fatal("expected the input to be delivered to the runner channel")
	}
}

func TestSubmitNoopWhenBusy(t *testing.T) {
	m := newTestModel()
	m.busy = true
	m.composer.SetValue("ignored")
	_, cmd := m.submit()
	if cmd != nil {
		t.Fatal("submit while a turn is running should be a no-op")
	}
}

func TestComposerPromptHasStableWidth(t *testing.T) {
	if got, want := runewidth.StringWidth(composerPrompt), len(composerPrompt); got != want {
		t.Fatalf("composerPrompt display width = %d, want byte/rune width %d for stable IME placement", got, want)
	}
}

func TestComposerWidthLeavesRightPaddingOnly(t *testing.T) {
	if got := composerWidth(80); got != 79 {
		t.Fatalf("composerWidth(80) = %d, want 79", got)
	}
	if got := composerWidth(1); got != 1 {
		t.Fatalf("composerWidth(1) = %d, want 1", got)
	}
}

func TestComposerCursorColumnUsesCJKDisplayWidth(t *testing.T) {
	m := readyModel(t)
	m.composer.SetValue("你好啊,")
	if got, want := m.composerCursorColumn(), 10; got != want {
		t.Fatalf("composerCursorColumn() = %d, want %d", got, want)
	}
}

func TestComposerCursorColumnUsesLastLine(t *testing.T) {
	m := readyModel(t)
	m.composer.SetValue("first\n好")
	if got, want := m.composerCursorColumn(), 5; got != want {
		t.Fatalf("composerCursorColumn() = %d, want %d", got, want)
	}
}

// must unwraps the (tea.Model, tea.Cmd) pair, discarding the cmd, for terse
// chaining in tests where the cmd is not under test.
func must(tm tea.Model, _ tea.Cmd) tea.Model { return tm }
