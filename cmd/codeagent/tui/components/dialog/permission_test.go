package dialog

import (
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

type stubGranter struct{ tool string }

func (g *stubGranter) AllowAlways(tool string) (string, error) {
	g.tool = tool
	return tool, nil
}

func newPermissionDialog(t *testing.T, granter PermissionGranter) (PermissionDialogCmp, chan agent.Verdict) {
	t.Helper()
	reply := make(chan agent.Verdict, 1)
	d := NewPermissionDialogCmp(granter)
	req := ApprovalRequest{Tool: "run_command", Input: json.RawMessage(`{"command":"go test ./..."}`), Reply: reply}
	d.SetPermissions(req)
	return d, reply
}

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func expectResponse(t *testing.T, cmd tea.Cmd, want PermissionAction) {
	t.Helper()
	if cmd == nil {
		t.Fatal("answering should return a command")
	}
	msg, ok := cmd().(PermissionResponseMsg)
	if !ok {
		t.Fatalf("expected PermissionResponseMsg, got %T", cmd())
	}
	if msg.Action != want {
		t.Fatalf("action = %q, want %q", msg.Action, want)
	}
}

func TestAllowKeyApproves(t *testing.T) {
	d, reply := newPermissionDialog(t, nil)
	u, cmd := d.Update(keyRune('a'))
	if _, ok := u.(PermissionDialogCmp); !ok {
		t.Fatalf("Update returned %T, want the dialog", u)
	}
	if <-reply != agent.VerdictAllow {
		t.Fatal("'a' should allow the tool")
	}
	expectResponse(t, cmd, PermissionAllow)
}

func TestDenyKeyDenies(t *testing.T) {
	d, reply := newPermissionDialog(t, nil)
	_, cmd := d.Update(keyRune('d'))
	if <-reply != agent.VerdictDeny {
		t.Fatal("'d' should deny the tool")
	}
	expectResponse(t, cmd, PermissionDeny)
}

func TestEscDenies(t *testing.T) {
	d, reply := newPermissionDialog(t, nil)
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if <-reply != agent.VerdictDeny {
		t.Fatal("esc should deny the tool")
	}
	expectResponse(t, cmd, PermissionDeny)
}

func TestEnterConfirmsSelectedAllow(t *testing.T) {
	d, reply := newPermissionDialog(t, nil)
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter}) // selection starts at Allow
	if <-reply != agent.VerdictAllow {
		t.Fatal("enter on the default selection should allow")
	}
	expectResponse(t, cmd, PermissionAllow)
}

func TestAllowForSessionGrantsAndAllows(t *testing.T) {
	g := &stubGranter{}
	d, reply := newPermissionDialog(t, g)
	_, cmd := d.Update(keyRune('s'))
	if <-reply != agent.VerdictAllow {
		t.Fatal("'s' should allow this call")
	}
	if g.tool != "run_command" {
		t.Fatalf("'s' should persist an always-allow rule, granter saw %q", g.tool)
	}
	expectResponse(t, cmd, PermissionAllowForSession)
}

func TestAllowForSessionWithoutGranterIsNoop(t *testing.T) {
	d, reply := newPermissionDialog(t, nil)
	_, cmd := d.Update(keyRune('s'))
	if cmd != nil {
		t.Fatal("'s' without a granter should be a no-op")
	}
	select {
	case v := <-reply:
		t.Fatalf("no verdict expected, got %v", v)
	default:
	}
}

func TestTabMovesSelectionToDeny(t *testing.T) {
	d, reply := newPermissionDialog(t, nil)
	u, cmd := d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if cmd != nil {
		t.Fatal("tab should only move the selection")
	}
	_, cmd = u.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if <-reply != agent.VerdictDeny {
		t.Fatal("after one tab the selection should be deny (2 options, no granter)")
	}
	expectResponse(t, cmd, PermissionDeny)
}
