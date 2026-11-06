package conversation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

func TestRuntimeInputsAreNotHumanTurns(t *testing.T) {
	sess := &session.Session{Messages: []model.Message{
		{Role: model.RoleSystem, Content: "system"},
		{Role: model.RoleUser, Content: "result data", RuntimeInput: true},
		{Role: model.RoleUser, Content: "user request", OriginTurnID: "turn"},
	}}
	if turnCount(sess) != 1 || firstUserMessage(sess) != "user request" {
		t.Fatal("runtime facts counted as human input")
	}
}

type runtimeInputApprover struct{}

func (runtimeInputApprover) Approve(string, json.RawMessage) agent.Verdict { return agent.VerdictAllow }

func TestRuntimeInputPortOnlyForInteractiveExecutor(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := NewSQLiteRepository(store, 128000, 90000, "test", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	builder := &fakeRunBuilder{}
	executor := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
	defer executor.Shutdown()
	executor.SetApprover(sess.ID, runtimeInputApprover{})
	if _, err = executor.ExecuteWithSession(context.Background(), sess, "interactive", ""); err != nil {
		t.Fatal(err)
	}
	if builder.lastCtx.RuntimeInputs != store {
		t.Fatal("interactive repository port not wired")
	}
	executor.SetApprovalMode(sess.ID, "auto")
	if _, err = executor.ExecuteWithSession(context.Background(), sess, "unattended", ""); err != nil {
		t.Fatal(err)
	}
	if builder.lastCtx.RuntimeInputs != nil {
		t.Fatal("unattended executor gained notifications")
	}
}
