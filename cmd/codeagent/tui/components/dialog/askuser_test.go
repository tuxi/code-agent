package dialog

import (
	"testing"

	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func newAskUserDialog(t *testing.T) (AskUserDialogCmp, chan agent.AskUserAnswer) {
	t.Helper()
	reply := make(chan agent.AskUserAnswer, 1)
	d := NewAskUserDialogCmp()
	q := agent.AskUserQuestion{
		ID:       "q1",
		Header:   "approach",
		Question: "which approach?",
		Options: []agent.AskOption{
			{Label: "A", Description: "option a"},
			{Label: "B", Description: "option b"},
		},
	}
	d.SetQuestion(AskUserRequest{Question: q, Reply: reply})
	return d, reply
}

func expectCloseAskUser(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("answering should return a command")
	}
	if _, ok := cmd().(CloseAskUserMsg); !ok {
		t.Fatalf("expected CloseAskUserMsg, got %T", cmd())
	}
}

func TestAskUserNavigatesAndConfirms(t *testing.T) {
	d, reply := newAskUserDialog(t)

	// Default selection is option 0.
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	expectCloseAskUser(t, cmd)
	ans := <-reply
	if len(ans.Selected) != 1 || ans.Selected[0] != "A" {
		t.Fatalf("default selection answer = %v", ans.Selected)
	}

	// Down then confirm picks option 1.
	u, _ := d.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd = u.Update(tea.KeyMsg{Type: tea.KeyEnter})
	expectCloseAskUser(t, cmd)
	ans = <-reply
	if len(ans.Selected) != 1 || ans.Selected[0] != "B" {
		t.Fatalf("after down, answer = %v", ans.Selected)
	}
}

func TestAskUserEscSendsEmptyAnswer(t *testing.T) {
	d, reply := newAskUserDialog(t)
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	expectCloseAskUser(t, cmd)
	ans := <-reply
	if len(ans.Selected) != 0 {
		t.Fatalf("cancel should send an empty answer, got %v", ans.Selected)
	}
}

func TestAskUserCustomSelectedMarksOther(t *testing.T) {
	reply := make(chan agent.AskUserAnswer, 1)
	d := NewAskUserDialogCmp()
	q := agent.AskUserQuestion{
		Header:      "h",
		Question:    "q",
		AllowCustom: true,
		Options:     []agent.AskOption{{Label: "X"}},
	}
	d.SetQuestion(AskUserRequest{Question: q, Reply: reply})

	// Selection starts at 0, which is the custom-input row when AllowCustom is on.
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	expectCloseAskUser(t, cmd)
	ans := <-reply
	if len(ans.Selected) != 1 || ans.Selected[0] != "Other" {
		t.Fatalf("custom selection answer = %v", ans.Selected)
	}
}
