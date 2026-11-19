package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	db "code-agent/internal/session/sqlite"
)

func TestLocalUserQueueExplicitInterruptedResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	storage, err := db.New(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = storage.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	queue, err := db.NewUserQueue(storage)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _, err := storage.ReserveTurnInput(ctx, session.TurnInput{SessionID: "s", RequestID: "initial", TurnID: "initial-turn", Text: "first", PayloadHash: "h"}, session.EventRecord{SessionID: "s", TurnID: "initial-turn", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.StartTurnInput(ctx, first, sess); err != nil {
		t.Fatal(err)
	}
	frozen := session.QueueExecutionContext{WireModel: "selected", ResolvedModel: "resolved", ReasoningEffort: "high", AuthorizationScope: "owner", ApprovalMode: "ask"}
	for _, id := range []string{"a", "b"} {
		if _, _, err = queue.EnqueueUserMessage(ctx, session.QueuedMessage{SessionID: "s", RequestID: id, Text: "task " + id, SelectionEffort: "high", Context: frozen}); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(map[string]any{"Kind": "turn_finished", "SessionID": "s", "TurnID": first.TurnID})
	if _, err = queue.FinishUserQueueTurn(ctx, session.UserQueueTurnEnd{SessionID: "s", RequestID: first.RequestID, TurnID: first.TurnID, State: session.TurnInputCompleted, Event: session.EventRecord{SessionID: "s", TurnID: first.TurnID, Kind: "turn_finished", At: time.Now(), Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	input, _, _, err := queue.ReserveUserQueueClaim(ctx, "s", "a", frozen)
	if err != nil {
		t.Fatal(err)
	}
	sess, err = storage.Load(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: input.Text, OriginTurnID: input.TurnID})
	if _, err = queue.StartUserQueueClaim(ctx, input, sess, frozen); err != nil {
		t.Fatal(err)
	}
	if err = storage.PauseInterruptedSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = queue.ReconcileUserQueue(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	repo := conversation.NewSQLiteRepository(storage, 10000, 8000, "resolved", "", nil)
	events := &conversation.StoreEventAdapter{Store: storage}
	provider := &supplementIntegrationProvider{started: make(chan string, 1), release: make(chan struct{})}
	builder := &localQueueIntegrationBuilder{supplementIntegrationBuilder{provider}, queueHTTPModel{effort: "high"}}
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	local := NewLocalUserQueue(ctx, executor, queue, repo, builder, &fakePermissionService{mode: "ask"}, "owner")
	defer local.Close()
	auth := ServerAuth{Enabled: true, Token: strings.Repeat("a", 32)}
	handler := NewMux(repo, events, executor, MuxOptions{LocalUserQueue: local, ServerAuth: auth})
	page, err := queue.ListUserQueue(ctx, "s", nil, 0)
	if err != nil || page.Interrupted == nil || !page.ResumeSupported {
		t.Fatal(page, err)
	}
	body := fmt.Sprintf(`{"action":"resume_interrupted","request_id":"a","revision":%d}`, page.Revision)
	r := httptest.NewRequest("POST", "/v1/conversations/s/queue/control", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+auth.Token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case turn := <-provider.started:
		if turn != input.TurnID {
			t.Fatal("identity changed", turn, input.TurnID)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The resumed model is in flight, so the next item must stay queued.
	b, err := queue.QueuedUserMessage(ctx, "s", "b")
	if err != nil || b.State != "queued" {
		t.Fatal(b, err)
	}
	close(provider.release)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err = queue.ListUserQueue(ctx, "s", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 && page.Interrupted == nil {
			records, err := storage.SessionEvents(ctx, "s")
			if err != nil {
				t.Fatal(err)
			}
			resumes, started, finished := 0, 0, 0
			for _, r := range records {
				if r.TurnID == input.TurnID {
					if r.Kind == "user_queue_resumed" {
						resumes++
					}
					if r.Kind == "user_queue_started" {
						started++
					}
				}
				if r.Kind == "turn_finished" {
					finished++
				}
			}
			// Leaving the pending list proves start, not completion. Wait
			// for the final durable event before asserting the whole drain.
			if finished == 3 {
				if resumes != 1 || started != 1 {
					t.Fatal(resumes, started, finished)
				}
				return
			}
			if finished > 3 {
				t.Fatal("duplicate completion", finished)
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
