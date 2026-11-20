package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	db "code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
)

type queueHTTPModel struct{ effort string }

func (m *queueHTTPModel) ResolveModel(string) (*settings.ModelConfig, error) {
	c := &settings.ModelConfig{Model: "resolved", Name: "resolved", ReasoningEffort: m.effort}
	c.Catalog.SupportedReasoningEfforts = []string{"low", "high"}
	return c, nil
}

func TestUserQueueHTTPTrustedSelectionRetryAndScope(t *testing.T) {
	ctx := context.Background()
	s, err := db.New(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Save(ctx, &session.Session{ID: "s", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	queue, err := db.NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	models := &queueHTTPModel{effort: "high"}
	perms := &fakePermissionService{mode: "ask"}
	scope := "owner"
	contexts := queueContextService{repo: conversation.NewSQLiteRepository(s, 10000, 8000, "resolved", "", nil), models: models, permissions: perms, scope: func(context.Context, string) (string, error) { return scope, nil }}
	service := &queueHTTPService{store: queue, contexts: contexts, consumerReady: true}
	mux := http.NewServeMux()
	if !registerUserQueueRoutes(mux, service, true) {
		t.Fatal("not available")
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	body := `{"request_id":"r","text":"next task","model":"selected"}`
	first := request("POST", "/v1/conversations/s/queue", body)
	var receipt session.QueuedMessageReceipt
	if first.Code != 202 || json.Unmarshal(first.Body.Bytes(), &receipt) != nil || receipt.Input.Context.ReasoningEffort != "high" || receipt.Input.Context.AuthorizationScope != "owner" || receipt.Input.Context.ApprovalMode != "ask" {
		t.Fatal(first.Code, first.Body.String())
	}
	models.effort = "low"
	perms.mode = "full"
	retry := request("POST", "/v1/conversations/s/queue", body)
	if retry.Code != 202 || retry.Body.String() != first.Body.String() {
		t.Fatal(retry.Code, retry.Body.String())
	}
	if w := request("POST", "/v1/conversations/s/queue", `{"request_id":"r","text":"changed","model":"selected"}`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, extra := range []string{`"authorization_scope":"fake"`, `"approval_mode":"full"`, `"local_assets":[]`, `"workspace_path":"/other"`} {
		if w := request("POST", "/v1/conversations/s/queue", `{"request_id":"x","text":"task","model":"selected",`+extra+`}`); w.Code != 400 {
			t.Fatal(extra, w.Code)
		}
	}
	if w := request("POST", "/v1/conversations/s/queue", `{"request_id":"bad","text":"task","model":"selected","reasoning_effort":"invalid"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	wakes := 0
	service.wake = func(sid string) {
		if sid != "s" {
			t.Fatal(sid)
		}
		wakes++
	}
	if w := request("GET", "/v1/conversations/s/queue?limit=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"take_back_supported":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if wakes != 1 {
		t.Fatal("enabled queue was not woken on read", wakes)
	}
	if w := request("GET", "/v1/conversations/s/queue?revision=0&after_sequence=0", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	scope = "another-owner"
	if w := request("GET", "/v1/conversations/s/queue/r", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request("GET", "/v1/conversations/s/queue", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	const path = "/v1/conversations/s/queue/r/take-back"
	const change = `{"take_back_id":"recover-r","revision":1}`
	if w := request("POST", path, change); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	scope = "owner"
	if w := request("POST", path, `{"take_back_id":"recover-r"}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	accepted := request("POST", path, change)
	if accepted.Code != 200 || json.Unmarshal(accepted.Body.Bytes(), &receipt) != nil ||
		receipt.State != "removed" || receipt.TakeBackID != "recover-r" || receipt.Input.Text != "next task" {
		t.Fatal(accepted.Code, accepted.Body.String())
	}
	if w := request("GET", "/v1/conversations/s/queue/r", ""); w.Code != 200 || w.Body.String() != accepted.Body.String() {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", path, change); w.Code != 200 || w.Body.String() != accepted.Body.String() {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/v1/conversations/s/queue", `{"request_id":"p","text":"promote me","model":"selected"}`); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	const priority = `{"action":"prioritize","request_id":"p","operation_id":"priority-p","revision":3}`
	for i := 0; i < 2; i++ {
		if w := request("POST", "/v1/conversations/s/queue/control", priority); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request("GET", "/v1/conversations/s/queue", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"prioritize_supported":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	scope = ""
	if w := request("POST", "/v1/conversations/s/queue", body); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestUserQueueHTTPDisabledWithoutFullAssembly(t *testing.T) {
	for _, svc := range []*queueHTTPService{nil, {}, {consumerReady: true}} {
		mux := http.NewServeMux()
		if registerUserQueueRoutes(mux, svc, true) {
			t.Fatal("advertised incomplete service")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/conversations/s/queue", nil))
		if w.Code != 501 {
			t.Fatal(w.Code)
		}
	}
}

func TestLocalUserQueueNewMuxRequiresOwner(t *testing.T) {
	ctx := context.Background()
	storage, err := db.New(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err = storage.Save(ctx, &session.Session{ID: "s", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	queue, err := db.NewUserQueue(storage)
	if err != nil {
		t.Fatal(err)
	}
	repo := conversation.NewSQLiteRepository(storage, 10000, 8000, "resolved", "", nil)
	events := &conversation.StoreEventAdapter{Store: storage}
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), nil)
	local := NewLocalUserQueue(ctx, executor, queue, repo, &queueHTTPModel{effort: "high"}, &fakePermissionService{mode: "ask"}, "owner")
	if local == nil {
		t.Fatal("local queue missing")
	}
	defer local.Close()
	auth := ServerAuth{Enabled: true, Token: strings.Repeat("a", 32)}
	handler := NewMux(repo, events, executor, MuxOptions{LocalUserQueue: local, ServerAuth: auth})
	req := func(method, path, body string, owner, shared bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if owner {
			r.Header.Set("Authorization", "Bearer "+auth.Token)
		}
		if shared {
			r = withSharedDeviceAuthContext(r)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	const path = "/v1/conversations/s/queue"
	const body = `{"request_id":"a","text":"task","model":"selected"}`
	if w := req("POST", path, body, false, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := req("POST", path, body, false, true); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := req("POST", path, body, true, false); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req("GET", "/v1/runtime/capabilities", "", false, true); w.Code != 200 || !strings.Contains(w.Body.String(), `"user_queue_text_v1":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req("GET", "/v1/runtime/capabilities", "", true, false); w.Code != 200 || !strings.Contains(w.Body.String(), `"user_queue_text_v1":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

type localQueueIntegrationBuilder struct {
	supplementIntegrationBuilder
	queueHTTPModel
}

func TestLocalUserQueueDrainsAfterOrdinaryTurn(t *testing.T) {
	testLocalUserQueueDrain(t, false, false)
}

func TestLocalUserQueueResumeAfterCancellation(t *testing.T) {
	testLocalUserQueueDrain(t, true, false)
}

func TestLocalUserQueueResponseLostBeforeClientAcceptance(t *testing.T) {
	testLocalUserQueueDrain(t, false, true)
}

func testLocalUserQueueDrain(t *testing.T, cancelFirst, loseResponse bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	storage, err := db.New(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err = storage.Save(ctx, &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	queue, err := db.NewUserQueue(storage)
	if err != nil {
		t.Fatal(err)
	}
	repo := conversation.NewSQLiteRepository(storage, 10000, 8000, "resolved", "", nil)
	events := &conversation.StoreEventAdapter{Store: storage}
	provider := &supplementIntegrationProvider{started: make(chan string, 1), release: make(chan struct{})}
	builder := &localQueueIntegrationBuilder{supplementIntegrationBuilder{provider}, queueHTTPModel{effort: "high"}}
	e := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	local := NewLocalUserQueue(ctx, e, queue, repo, builder, &fakePermissionService{mode: "ask"}, "owner")
	defer local.Close()
	auth := ServerAuth{Enabled: true, Token: strings.Repeat("a", 32)}
	handler := NewMux(repo, events, e, MuxOptions{LocalUserQueue: local, ServerAuth: auth})
	done := make(chan error, 1)
	go func() { _, err := e.ExecuteWithRequestID(ctx, "s", "initial", "task", "selected", ""); done <- err }()
	select {
	case <-provider.started:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, id := range []string{"a", "b"} {
		if loseResponse && id == "a" {
			srv := deliveryResponseLossServer(t, handler)
			body := `{"request_id":"a","text":"task a","model":"selected"}`
			recoverLostDeliveryResponse(t, ctx, srv, "/v1/conversations/s/queue", "a", auth.Token, body)
			continue
		}
		r := httptest.NewRequest("POST", "/v1/conversations/s/queue", strings.NewReader(`{"request_id":"`+id+`","text":"task `+id+`","model":"selected"}`))
		r.Header.Set("Authorization", "Bearer "+auth.Token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	expectedFinished := 3
	expectedMessages := "task a,task b"
	if cancelFirst {
		e.Cancel("s")
		<-done
		page, err := queue.ListUserQueue(ctx, "s", nil, 0)
		// The ordinary executor wakes the queue after releasing its permit.
		// Await that consumer's persisted pause, not the runner's return alone.
		for err == nil && page.State != "paused" {
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			page, err = queue.ListUserQueue(ctx, "s", nil, 0)
		}
		if err != nil || page.State != "paused" || page.PauseReason != "cancelled" || len(page.Items) != 2 {
			t.Fatal(page, err)
		}
		control := func(body string, want int) {
			t.Helper()
			r := httptest.NewRequest("POST", "/v1/conversations/s/queue/control", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+auth.Token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		control(fmt.Sprintf(`{"action":"remove","request_id":"a","revision":%d}`, page.Revision), 200)
		control(fmt.Sprintf(`{"action":"resume","revision":%d}`, page.Revision), 409)
		page, err = queue.ListUserQueue(ctx, "s", nil, 0)
		if err != nil || page.State != "paused" || len(page.Items) != 1 || page.Items[0].Input.RequestID != "b" {
			t.Fatal(page, err)
		}
		removed, err := queue.QueuedUserMessage(ctx, "s", "a")
		if err != nil || removed.State != "removed" {
			t.Fatal(removed, err)
		}
		control(fmt.Sprintf(`{"action":"resume","revision":%d}`, page.Revision), 200)
		expectedFinished = 1
		expectedMessages = "task b"
		close(provider.release)
	} else {
		close(provider.release)
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		records, err := storage.SessionEvents(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		finished := 0
		for _, r := range records {
			if r.Kind == "turn_finished" {
				finished++
			}
		}
		if finished == expectedFinished {
			saved, err := storage.Load(ctx, "s")
			if err != nil {
				t.Fatal(err)
			}
			var queued []string
			for _, m := range saved.Messages {
				if m.Role == model.RoleUser && strings.HasPrefix(m.Content, "task ") {
					queued = append(queued, m.Content)
				}
			}
			if strings.Join(queued, ",") != expectedMessages {
				t.Fatal(queued)
			}
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("queue did not finish", ctx.Err())
		}
	}
}
