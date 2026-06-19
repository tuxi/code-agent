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
