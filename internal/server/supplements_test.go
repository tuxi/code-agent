package server

import (
	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sqlitestore "code-agent/internal/session/sqlite"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTextSupplementHTTPAdmissionQueryAndIsolation(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &session.Session{ID: "s", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	input, _, _, err := store.ReserveTurnInput(ctx, session.TurnInput{SessionID: "s", RequestID: "initial", TurnID: "t", PayloadHash: "h", Text: "task"}, session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", Payload: []byte(`{}`), At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StartTurnInput(ctx, input, sess); err != nil {
		t.Fatal(err)
	}
	inbox, err := sqlitestore.NewSupplementInbox(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = inbox.OpenSupplementBoundary(ctx, "s", "t"); err != nil {
		t.Fatal(err)
	}
	executor := &conversation.TurnExecutor{}
	executor.SetTurnDeliveryInbox(inbox)
	mux := http.NewServeMux()
	if !registerSupplementRoutes(mux, executor, true) {
		t.Fatal("capability missing")
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	body := `{"request_id":"r","expected_turn_id":"t","text":"more"}`
	first := request("POST", "/v1/conversations/s/supplements", body)
	second := request("POST", "/v1/conversations/s/supplements", body)
	if first.Code != 202 || second.Code != 202 || first.Body.String() != second.Body.String() {
		t.Fatal(first, second)
	}
	if w := request("POST", "/v1/conversations/s/supplements", `{"request_id":"r","expected_turn_id":"t","text":"changed"}`); w.Code != 409 {
		t.Fatal(w)
	}
	if w := request("POST", "/v1/conversations/s/supplements", `{"request_id":"media","expected_turn_id":"t","text":"more","local_assets":[]}`); w.Code != 400 {
		t.Fatal(w)
	}
	if w := request("GET", "/v1/conversations/other/supplements/r", ""); w.Code != 404 {
		t.Fatal(w)
	}
	if _, err = inbox.ApplySupplements(ctx, sess, "t", false); err != nil {
		t.Fatal(err)
	}
	w := request("GET", "/v1/conversations/s/supplements/r", "")
	var receipt session.SupplementReceipt
	if err = json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || w.Code != 200 || receipt.State != "applied" {
		t.Fatal(w, receipt, err)
	}
}

func TestTextSupplementCapabilityIsFailClosed(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		mux := http.NewServeMux()
		if registerSupplementRoutes(mux, (*conversation.TurnExecutor)(nil), enabled) {
			t.Fatal("advertised without executor")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/conversations/s/supplements/r", nil))
		if w.Code != 501 {
			t.Fatal(w.Code)
		}
	}
}

func TestSupplementApplicationWireKeepsReceiptAndEventSequence(t *testing.T) {
	receipt := &session.SupplementApplication{RequestID: "r", TurnID: "t", Sequence: 4, EventSequence: 99, AppliedAt: time.Now().UTC()}
	wire := toWire(agent.Event{Kind: agent.EventKind("user_supplement_applied"), SessionID: "s", TurnID: "t", RequestID: "r", Seq: 99, At: receipt.AppliedAt, SupplementApplication: receipt})
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded wireEvent
	if err = json.Unmarshal(data, &decoded); err != nil || decoded.Seq != 99 || decoded.SupplementApplication == nil || decoded.SupplementApplication.Sequence != 4 {
		t.Fatal(string(data), err)
	}
	if toWire(agent.Event{Kind: agent.EventTurnStarted, SupplementApplication: receipt}).SupplementApplication != nil {
		t.Fatal("receipt leaked to unrelated event")
	}
}
