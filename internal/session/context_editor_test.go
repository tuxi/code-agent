package session

import (
	"testing"

	"code-agent/internal/model"
)

func TestContextEditorClearsOldToolResults(t *testing.T) {
	sess := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a test agent."},
			{Role: model.RoleUser, Content: "find the bug"},
			{Role: model.RoleAssistant, Content: "Let me search."},
			{Role: model.RoleTool, Content: "50 matches for 'error'", ToolCallID: "t1"},
			{Role: model.RoleAssistant, Content: "Now reading..."},
			{Role: model.RoleTool, Content: "func main() { ... }", ToolCallID: "t2"},
			{Role: model.RoleAssistant, Content: "I found a bug!"},
			{Role: model.RoleTool, Content: "fixed", ToolCallID: "t3"}, // recent turn — preserved
			{Role: model.RoleAssistant, Content: "Done."},
		},
	}

	// KeepTurns: 2 — keep the last 2 assistant turns (index 6-8, assistants at 6 & 8)
	editor := ContextEditor{KeepTurns: 2}
	editor.Edit(sess)

	// Recent turn's tool result (index 7, belongs to assistant at index 6) preserved.
	if sess.Messages[7].Content != "fixed" {
		t.Errorf("recent tool result was cleared: %q", sess.Messages[7].Content)
	}

	// Old tool result 1 cleared.
	if sess.Messages[3].Content != "[tool result cleared]" {
		t.Errorf("old tool result not cleared: %q", sess.Messages[3].Content)
	}

	// Old tool result 2 cleared.
	if sess.Messages[5].Content != "[tool result cleared]" {
		t.Errorf("old tool result not cleared: %q", sess.Messages[5].Content)
	}

	// User message preserved.
	if sess.Messages[1].Content != "find the bug" {
		t.Errorf("user message was modified: %q", sess.Messages[1].Content)
	}

	// Assistant messages preserved.
	if sess.Messages[2].Content != "Let me search." {
		t.Errorf("assistant message was modified: %q", sess.Messages[2].Content)
	}
}

func TestContextEditorPreservesErrors(t *testing.T) {
	// 5 assistant turns, KeepTurns: 2 → the edit window covers turns 1-3
	// (indices < 11), so error-marked results genuinely enter the window and
	// exercise every isError branch, not just the keep-window logic.
	sess := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a test agent."},
			{Role: model.RoleUser, Content: "first task"},
			{Role: model.RoleAssistant, Content: "Turn 1"},
			{Role: model.RoleTool, Content: "error: connection refused", ToolCallID: "t1"},
			{Role: model.RoleUser, Content: "second task"},
			{Role: model.RoleAssistant, Content: "Turn 2"},
			{Role: model.RoleTool, Content: "failure=test: assertion failed", ToolCallID: "t2"},
			{Role: model.RoleUser, Content: "third task"},
			{Role: model.RoleAssistant, Content: "Turn 3"},
			{Role: model.RoleTool, Content: "normal output", ToolCallID: "t3"},
			{Role: model.RoleUser, Content: "fourth task"},
			{Role: model.RoleAssistant, Content: "Turn 4"},
			{Role: model.RoleTool, Content: "recent result", ToolCallID: "t4"},
			{Role: model.RoleAssistant, Content: "Turn 5"},
		},
	}

	editor := ContextEditor{KeepTurns: 2} // keep last 2 turns, edit turns 1-3
	editor.Edit(sess)

	// Turn 1's error: — preserved (isError "error:").
	if sess.Messages[3].Content != "error: connection refused" {
		t.Errorf("error: result should be preserved: %q", sess.Messages[3].Content)
	}
	// Turn 2's failure= marker — preserved (isError "failure=").
	if sess.Messages[6].Content != "failure=test: assertion failed" {
		t.Errorf("failure= result should be preserved: %q", sess.Messages[6].Content)
	}
	// Turn 3's normal output — cleared.
	if sess.Messages[9].Content != "[tool result cleared]" {
		t.Errorf("normal output should be cleared: %q", sess.Messages[9].Content)
	}
	// Turn 4's recent result — preserved (inside keep window).
	if sess.Messages[12].Content != "recent result" {
		t.Errorf("recent result should be preserved: %q", sess.Messages[12].Content)
	}
}

func TestContextEditorIdempotent(t *testing.T) {
	sess := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a test agent."},
			{Role: model.RoleUser, Content: "hi"},
			{Role: model.RoleAssistant, Content: "ok"},
			{Role: model.RoleTool, Content: "result", ToolCallID: "t1"},
		},
	}

	editor := ContextEditor{KeepTurns: 0}
	first := editor.Edit(sess) // KeepTurns=0 → default 3; 1 turn → nothing edited
	if first != 0 {
		t.Errorf("within keep window: want 0 edits, got %d", first)
	}
	if sess.Messages[3].Content != "result" {
		t.Errorf("result was modified: %q", sess.Messages[3].Content)
	}

	sess2 := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a test agent."},
			{Role: model.RoleUser, Content: "first"},
			{Role: model.RoleAssistant, Content: "Turn 1"},
			{Role: model.RoleTool, Content: "old result", ToolCallID: "t1"},
			{Role: model.RoleAssistant, Content: "Turn 2"},
		},
	}
	e2 := ContextEditor{KeepTurns: 1}
	n1 := e2.Edit(sess2)
	n2 := e2.Edit(sess2) // second pass — must be a true no-op
	if n1 != 1 || n2 != 0 {
		t.Errorf("first pass = %d, second pass = %d; want 1 then 0", n1, n2)
	}
	if sess2.Messages[3].Content != "[tool result cleared]" {
		t.Errorf("old tool result not cleared: %q", sess2.Messages[3].Content)
	}
}

func TestContextEditorCountsEdits(t *testing.T) {
	// KeepTurns=3 (default), 1 assistant turn -> nothing edited.
	sess := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a test agent."},
			{Role: model.RoleUser, Content: "do it"},
			{Role: model.RoleAssistant, Content: "ok"},
			{Role: model.RoleTool, Content: "output 1", ToolCallID: "t1"},
			{Role: model.RoleTool, Content: "output 2", ToolCallID: "t2"},
		},
	}
	n := ContextEditor{}.Edit(sess) // default KeepTurns=3
	if n != 0 {
		t.Errorf("within keep window: want 0 edits, got %d", n)
	}

	// 2 assistant turns, KeepTurns=1 -> 1 old turn edited.
	sess2 := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a test agent."},
			{Role: model.RoleUser, Content: "first"},
			{Role: model.RoleAssistant, Content: "Turn 1"},
			{Role: model.RoleTool, Content: "old result", ToolCallID: "t1"},
			{Role: model.RoleUser, Content: "second"},
			{Role: model.RoleAssistant, Content: "Turn 2"},
			{Role: model.RoleTool, Content: "recent result", ToolCallID: "t2"},
		},
	}
	n2 := ContextEditor{KeepTurns: 1}.Edit(sess2)
	if n2 != 1 {
		t.Errorf("1 old turn, want 1 edit, got %d", n2)
	}
	if sess2.Messages[3].Content != "[tool result cleared]" {
		t.Errorf("old tool result not cleared: %q", sess2.Messages[3].Content)
	}
	if sess2.Messages[6].Content != "recent result" {
		t.Errorf("recent tool result was cleared: %q", sess2.Messages[6].Content)
	}
}
