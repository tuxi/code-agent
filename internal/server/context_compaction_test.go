package server

import (
	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type contextTestBuilder struct{}

func (contextTestBuilder) Build(conversation.RuntimeContext) conversation.TurnRunner {
	return &agent.Runner{Compactor: &session.SlidingWindowCompactor{KeepRecentMessages: 1}}
}
func TestCompactHTTPAndPendingSnapshot(t *testing.T) {
	repo := newFakeConversationRepo()
	s, _ := repo.Create(context.Background(), "", "")
	s.ContextWindow = 1000000
	s.CompactThreshold = 750000
	s.PromptTokens = 957379
	s.Messages = []model.Message{{Role: model.RoleSystem, Content: "sys"}, {Role: model.RoleUser, Content: "old"}, {Role: model.RoleAssistant, Content: "answer"}, {Role: model.RoleUser, Content: "latest"}}
	e := conversation.NewTurnExecutor(repo, &fakeEventStore{}, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), contextTestBuilder{})
	mux := http.NewServeMux()
	registerContextCompaction(mux, repo, e)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/conversations/"+s.ID+"/compact", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "compacted_pending_measurement") || !strings.Contains(w.Body.String(), `"after_tokens":-1`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	s.SetTurnStatus(session.TurnStatusRunning)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/conversations/"+s.ID+"/compact", nil))
	if w.Code != 409 {
		t.Fatalf("active turn accepted: %d", w.Code)
	}
}
