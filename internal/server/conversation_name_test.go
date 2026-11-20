package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session/sqlite"
)

func TestConversationNamePreviewAndRenameWire(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := conversation.NewSQLiteRepository(store, 1000, 800, "test", "", nil)
	sess, err := repo.Create(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{
		Role: model.RoleUser, Assets: []model.GatewayAssetRef{{Kind: "image"}},
	})
	if err := repo.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	eventStore := &conversation.StoreEventAdapter{Store: store}
	ex := conversation.NewTurnExecutor(repo, eventStore, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), nil)
	defer ex.Shutdown()
	events, unsubscribe := ex.Subscribe(sess.ID)
	defer unsubscribe()
	mux := NewMux(repo, eventStore, ex, MuxOptions{})
	path := "/v1/conversations/" + sess.ID
	request := func(method, target, body string, dst any) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, target, strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d response=%s", method, target, w.Code, w.Body)
		}
		decodeResponse(t, w.Result(), dst)
	}

	var refs []ConversationRef
	request("GET", "/v1/conversations?limit=1", "", &refs)
	if len(refs) != 1 || refs[0].Name != "" || refs[0].Preview != "图片附件" {
		t.Fatalf("unexpected list preview: %+v", refs)
	}
	var renamed ConversationRef
	request("PATCH", path, `{"name":"截图问题"}`, &renamed)
	if renamed.Name != "截图问题" || renamed.NameSeq <= 0 {
		t.Fatalf("rename result=%+v", renamed)
	}
	var detail ConversationDetail
	request("GET", path, "", &detail)
	if detail.Name != renamed.Name || detail.NameSeq != renamed.NameSeq || detail.Preview != refs[0].Preview {
		t.Fatalf("detail=%+v rename=%+v", detail, renamed)
	}
	select {
	case ev := <-events:
		if ev.Kind != agent.EventConversationNameUpdated || ev.Name != renamed.Name || ev.Seq != renamed.NameSeq {
			t.Fatalf("name event=%+v", ev)
		}
	default:
		t.Fatal("missing live rename event")
	}
	var replay []wireEvent
	request("GET", path+"/events", "", &replay)
	if len(replay) != 1 || replay[0].Kind != string(agent.EventConversationNameUpdated) ||
		replay[0].Name != renamed.Name || replay[0].Seq != renamed.NameSeq {
		t.Fatalf("name replay=%+v", replay)
	}
}
