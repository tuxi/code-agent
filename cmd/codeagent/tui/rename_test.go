package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"code-agent/internal/agent"
)

func TestConversationPreviewAndRenameWhileTurnIsPending(t *testing.T) {
	var saveErr error = errors.New("save failed")
	ui := newModel(NewBackend(), HeaderInfo{}, sessionSource{
		rename: func(id, name string) (int64, error) {
			if id != "s" || name != "我的标题" {
				t.Fatalf("unexpected rename %q %q", id, name)
			}
			return 3, saveErr
		},
	}).(*model)
	ui.sessionID = "s"
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 25})
	ui.startTurn("你好啊，你会做什么")
	if !strings.Contains(ui.status.View().Content, "你好啊，你会做什么") {
		t.Fatal("missing preview while the turn is pending")
	}
	ui.handleEvent(agent.Event{Kind: agent.EventConversationNameUpdated, SessionID: "s", Name: "助手能力咨询", Seq: 1})
	if !strings.Contains(ui.status.View().Content, "助手能力咨询") {
		t.Fatal("missing generated title while the turn is pending")
	}
	ui.openRename("我的标题")
	press := tea.KeyPressMsg{Code: tea.KeyEnter}
	_, cmd := ui.Update(press)
	ui.Update(cmd())
	if ui.sessionName != "助手能力咨询" || ui.renameDialog.input.Value() != "我的标题" || ui.renameDialog.err != "save failed" {
		t.Fatal("failed rename did not preserve title and edit")
	}
	saveErr = nil
	_, cmd = ui.Update(press)
	ui.Update(cmd())
	ui.handleEvent(agent.Event{Kind: agent.EventConversationNameUpdated, SessionID: "s", Name: "迟到的标题", Seq: 2})
	if ui.dialog != nil || ui.sessionName != "我的标题" {
		t.Fatal("late event replaced the confirmed rename")
	}
}
