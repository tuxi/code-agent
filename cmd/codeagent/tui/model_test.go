package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"code-agent/cmd/codeagent/tui/components/chat"
	"code-agent/cmd/codeagent/tui/components/dialog"
	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func newTestModel() *model {
	return newModel(NewBackend(), HeaderInfo{}, sessionSource{}).(*model)
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

func asModel(t *testing.T, tm tea.Model) *model {
	t.Helper()
	m, ok := tm.(*model)
	if !ok {
		t.Fatalf("Update returned %T, want model", tm)
	}
	return m
}

func TestApprovalApprove(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.Verdict, 1)
	tm, _ := m.Update(approvalMsg{tool: "create_file", input: json.RawMessage(`{"path":"x"}`), reply: reply})
	m = asModel(t, tm)

	// 'a' = allow: the dialog answers the request and emits its close message.
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = asModel(t, tm)
	if <-reply != agent.VerdictAllow {
		t.Fatal("'a' should approve the tool")
	}
	resp, ok := cmd().(dialog.PermissionResponseMsg)
	if !ok {
		t.Fatalf("'a' should yield the dialog response, got %T", cmd())
	}
	// The app closes the dialog on the response.
	tm, _ = m.Update(resp)
	m = asModel(t, tm)
	if m.dialog != nil {
		t.Fatal("answering should close the dialog")
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

// fakeGranter records the AllowAlways call for the card's "always allow" choice.
type fakeGranter struct{ tool string }

func (g *fakeGranter) AllowAlways(tool string) (string, error) {
	g.tool = tool
	return tool, nil
}

func TestApprovalDeny(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.Verdict, 1)
	tm, _ := m.Update(approvalMsg{tool: "run_command", reply: reply})
	m = asModel(t, tm)

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, tm)
	if <-reply == agent.VerdictAllow {
		t.Fatal("esc should deny the tool")
	}
	if _, ok := cmd().(dialog.PermissionResponseMsg); !ok {
		t.Fatalf("esc should yield the dialog response, got %T", cmd())
	}
}

// Submitting /mcp__server__prompt renders via PromptOps (off-UI), then feeds the
func TestSubmitMCPPromptRendersAndRuns(t *testing.T) {
	m := newTestModel()
	f := &fakePromptOps{text: "rendered prompt body"}
	m.src.promptOps = f

	cmd := m.submit("/mcp__gh__pr_review 456 deep")
	if cmd == nil {
		t.Fatal("submit should return a render cmd")
	}
	// The render cmd calls PromptOps.Render with the parsed command + positional args.
	rendered, ok := cmd().(promptRenderedMsg)
	if !ok {
		t.Fatalf("render cmd should yield promptRenderedMsg, got %T", cmd())
	}
	if f.command != "mcp__gh__pr_review" || len(f.args) != 2 || f.args[0] != "456" || f.args[1] != "deep" {
		t.Fatalf("Render got command=%q args=%v", f.command, f.args)
	}
	if rendered.text != "rendered prompt body" {
		t.Fatalf("rendered text = %q", rendered.text)
	}
	// Update starts the turn with the rendered text.
	tm, _ := m.Update(rendered)
	m = asModel(t, tm)
	if !m.busy {
		t.Fatal("promptRenderedMsg should start a turn (busy=true)")
	}
	if got := <-m.b.inputs; got != "rendered prompt body" {
		t.Fatalf("inputs got %q", got)
	}
}

// No MCP wired routes /mcp__ through the slash-command fallback (an
// unknown-command notice) instead of starting a turn.
func TestSubmitMCPPromptUnavailable(t *testing.T) {
	m := newTestModel() // promptOps nil
	cmd := m.submit("/mcp__x__y")
	if m.busy {
		t.Fatal("no promptOps: submit must not start a turn")
	}
	msgs := runLeaves(cmd)
	for _, msg := range msgs {
		if n, ok := msg.(chat.NewMessageMsg); ok && strings.Contains(n.Message.Content, "unknown command") {
			return
		}
	}
	t.Fatalf("no promptOps: /mcp__ should fall back to an unknown-command notice, got %v", msgs)
}

// 's' = allow for session: approves the call AND persists a rule via the granter.
func TestApprovalAlwaysGrants(t *testing.T) {
	m := newTestModel()
	g := &fakeGranter{}
	m.src.granter = g
	// newModel built the permission dialog before src.granter was set — rewire it
	// so the dialog's 's' (allow-for-session) key path sees the granter.
	m.permission = dialog.NewPermissionDialogCmp(g)
	reply := make(chan agent.Verdict, 1)
	tm, _ := m.Update(approvalMsg{tool: "mcp__github__list_issues", reply: reply})
	m = asModel(t, tm)

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = asModel(t, tm)
	if <-reply != agent.VerdictAllow {
		t.Fatal("'s' should approve the tool")
	}
	if g.tool != "mcp__github__list_issues" {
		t.Fatalf("'s' should persist an always-allow rule, granter saw %q", g.tool)
	}
	if _, ok := cmd().(dialog.PermissionResponseMsg); !ok {
		t.Fatalf("'s' should yield the dialog response, got %T", cmd())
	}
}

// The approval card is modal — ctrl+c denies AND quits, never falls through to
// the global quit handler.
func TestApprovalSwallowsCtrlC(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.Verdict, 1)
	tm, _ := m.Update(approvalMsg{tool: "run_command", reply: reply})
	m = asModel(t, tm)

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = asModel(t, tm)
	if <-reply == agent.VerdictAllow {
		t.Fatal("ctrl+c on the approval card should deny")
	}
	for _, msg := range runLeaves(cmd) {
		if _, ok := msg.(tea.QuitMsg); ok {
			return
		}
	}
	t.Fatal("ctrl+c on the approval card should quit")
}

func TestSubmitLocksBusyAndDeliversInput(t *testing.T) {
	m := newTestModel()
	cmd := m.submit("  fix the test  ")
	if cmd == nil {
		t.Fatal("submit should return waitForDone")
	}
	if !m.busy {
		t.Fatal("submit should lock the composer")
	}
	select {
	case got := <-m.b.inputs:
		if got != "fix the test" {
			t.Fatalf("delivered input = %q, want trimmed", got)
		}
	default:
		t.Fatal("expected the input to be delivered to the runner channel")
	}
}

func TestSubmitWhileBusySkipsDelivery(t *testing.T) {
	m := newTestModel()
	m.busy = true
	m.submit("ignored")
	if !m.busy {
		t.Fatal("busy should stay set")
	}
	select {
	case got := <-m.b.inputs:
		t.Fatalf("busy submit should not deliver input, got %q", got)
	default:
	}
}

// must unwraps the (tea.Model, tea.Cmd) pair, discarding the cmd, for terse
// chaining in tests where the cmd is not under test.
func must(tm tea.Model, _ tea.Cmd) tea.Model { return tm }
