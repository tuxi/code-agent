package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

type headerOnlyRepository struct {
	*fakeConversationRepo
	fullLoads   int
	headerLoads int
}

func (r *headerOnlyRepository) Load(context.Context, string) (*session.Session, error) {
	r.fullLoads++
	return nil, fmt.Errorf("full Load forbidden")
}
func (r *headerOnlyRepository) LoadHeader(ctx context.Context, id string) (*session.Session, error) {
	r.headerLoads++
	return r.fakeConversationRepo.Load(ctx, id)
}

func TestDetailAndWSExistenceOnlyUseHeaders(t *testing.T) {
	base := newFakeConversationRepo()
	base.sessions["s"] = &session.Session{ID: "s", Name: "header name", WorkspacePath: "/workspace", Metadata: map[string]any{session.MetaForkSource: "parent", session.MetaTurnStatus: "paused"}}
	repo := &headerOnlyRepository{fakeConversationRepo: base}
	events := &conversation.StoreEventAdapter{Store: session.NewMemoryStore()}
	mux := newTestMux(repo, events)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}
	r := get("/v1/conversations/s")
	if r.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", r.Code, r.Body.String())
	}
	var detail ConversationDetail
	decodeResponse(t, r.Result(), &detail)
	if detail.Name != "header name" || detail.ForkSourceSessionID != "parent" || detail.TurnStatus != "paused" {
		t.Fatalf("detail=%+v", detail)
	}
	for _, path := range []string{"/v1/conversations/s/events?tail=2", "/v1/conversations/s/events?since=1"} {
		if r := get(path); r.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, r.Code)
		}
	}
	before := repo.headerLoads
	// WS resolves existence before rejecting the intentionally missing Upgrade.
	r = get("/v1/conversations/s/stream")
	if (r.Code != http.StatusUpgradeRequired && r.Code != http.StatusBadRequest) || repo.headerLoads != before+1 {
		t.Fatalf("WS status=%d headerLoads=%d before=%d", r.Code, repo.headerLoads, before)
	}
	if r := get("/v1/conversations/missing/stream"); r.Code != http.StatusNotFound {
		t.Fatalf("missing WS status=%d", r.Code)
	}
	if repo.fullLoads != 0 {
		t.Fatalf("full Load called %d times", repo.fullLoads)
	}
}
