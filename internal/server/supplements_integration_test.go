package server

import (
	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	sqlitestore "code-agent/internal/session/sqlite"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type supplementIntegrationProvider struct {
	started       chan string
	release       chan struct{}
	calls         int
	sawSupplement bool
}

func (p *supplementIntegrationProvider) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	p.calls++
	if p.calls == 1 {
		p.started <- req.TurnID
		select {
		case <-p.release:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
		return model.Response{Content: "initial"}, nil
	}
	for _, m := range req.Messages {
		if m.Role == model.RoleUser && m.Content == "include supplement" {
			p.sawSupplement = true
		}
	}
	return model.Response{Content: "supplement included"}, nil
}

type supplementIntegrationBuilder struct {
	provider *supplementIntegrationProvider
}

func (b supplementIntegrationBuilder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	return &agent.Runner{Model: b.provider, MaxSteps: 4, RequestID: rc.RequestID, ReservedTurnID: rc.TurnID, Emitter: rc.Publisher, RuntimeInputs: rc.RuntimeInputs}
}

func TestTextSupplementRealExecutorHTTPModelAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &session.Session{ID: "s", WorkspacePath: t.TempDir(), Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	repo := conversation.NewSQLiteRepository(store, 128000, 96000, "model", "", nil)
	events := &conversation.StoreEventAdapter{Store: store}
	provider := &supplementIntegrationProvider{started: make(chan string, 1), release: make(chan struct{})}
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), supplementIntegrationBuilder{provider})
	inbox, err := sqlitestore.NewSupplementInbox(store)
	if err != nil {
		t.Fatal(err)
	}
	executor.SetTurnDeliveryInbox(inbox)
	handler := NewMux(repo, events, executor, MuxOptions{EnableTextSupplements: true})
	done := make(chan error, 1)
	go func() { _, err := executor.ExecuteWithRequestID(ctx, "s", "initial", "task", "", ""); done <- err }()
	var target string
	select {
	case target = <-provider.started:
	case err = <-done:
		t.Fatal("execution failed before model", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	payload, _ := json.Marshal(map[string]string{"request_id": "extra", "expected_turn_id": target, "text": "include supplement"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/conversations/s/supplements", strings.NewReader(string(payload))))
	close(provider.release)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if provider.calls != 2 || !provider.sawSupplement {
		t.Fatal(provider.calls, provider.sawSupplement)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/v1/conversations/s/supplements/extra", nil))
	var receipt session.SupplementReceipt
	if err = json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.State != "applied" {
		t.Fatal(response.Body.String(), err)
	}
	records, err := store.SessionEvents(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	applied, finished := 0, 0
	for _, r := range records {
		if r.Kind == "user_supplement_applied" {
			applied++
		}
		if r.Kind == "turn_finished" {
			finished++
		}
	}
	if applied != 1 || finished != 1 {
		t.Fatal(applied, finished)
	}
}
