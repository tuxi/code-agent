package conversation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/followup"
	"code-agent/internal/session"
)

func appendFollowUpEvent(t *testing.T, store *fakeEventStore, event agent.Event) {
	t.Helper()
	event.SessionID = "s"
	event.At = time.Now()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), session.EventRecord{SessionID: "s", TurnID: event.TurnID, Kind: string(event.Kind), At: event.At, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func testOffer() *followup.Offer {
	return &followup.Offer{
		ID: "fu_1", TurnID: "turn_1", Detail: "done", Question: "next?", Dismissible: true,
		Options: []followup.Option{
			{ID: "opt_1", Label: "改严", Message: "改严", Recommended: true},
			{ID: "opt_2", Label: "保持", Message: "保持现状"},
		},
	}
}

func TestValidateFollowUpReplyRequiresActiveMatchingOption(t *testing.T) {
	store := &fakeEventStore{}
	appendFollowUpEvent(t, store, agent.Event{Kind: agent.EventTurnStarted, TurnID: "turn_1", Text: "task"})
	appendFollowUpEvent(t, store, agent.Event{Kind: agent.EventTurnFinished, TurnID: "turn_1", Text: "done", FollowUp: testOffer()})
	executor := &TurnExecutor{events: store}

	reply := &session.FollowUpReply{TurnID: "turn_1", FollowUpID: "fu_1", OptionID: "opt_1"}
	if err := executor.validateFollowUpReply(context.Background(), "s", "改严", reply); err != nil {
		t.Fatalf("valid selection rejected: %v", err)
	}
	if err := executor.validateFollowUpReply(context.Background(), "s", "别的", reply); err == nil {
		t.Fatal("mismatched message was accepted")
	}
	wrong := &session.FollowUpReply{TurnID: "turn_1", FollowUpID: "fu_1", OptionID: "missing"}
	if err := executor.validateFollowUpReply(context.Background(), "s", "改严", wrong); err == nil {
		t.Fatal("unknown option was accepted")
	}
}

func TestValidateFollowUpReplyRejectsSupersededAndDismissedCards(t *testing.T) {
	store := &fakeEventStore{}
	appendFollowUpEvent(t, store, agent.Event{Kind: agent.EventTurnFinished, TurnID: "turn_1", FollowUp: testOffer()})
	executor := &TurnExecutor{events: store}
	reply := &session.FollowUpReply{TurnID: "turn_1", FollowUpID: "fu_1", OptionID: "opt_1"}

	appendFollowUpEvent(t, store, agent.Event{Kind: agent.EventTurnStarted, TurnID: "turn_2", Text: "newer input"})
	if err := executor.validateFollowUpReply(context.Background(), "s", "改严", reply); err == nil {
		t.Fatal("superseded card was accepted")
	}

	store = &fakeEventStore{}
	appendFollowUpEvent(t, store, agent.Event{Kind: agent.EventTurnFinished, TurnID: "turn_1", FollowUp: testOffer()})
	appendFollowUpEvent(t, store, agent.Event{Kind: agent.EventFollowUpDismissed, TurnID: "turn_1", FollowUpID: "fu_1"})
	executor = &TurnExecutor{events: store}
	if err := executor.validateFollowUpReply(context.Background(), "s", "改严", reply); err == nil {
		t.Fatal("dismissed card was accepted")
	}
}

func TestFollowUpSelectionPersistsReplyAndWiresRuntimeContext(t *testing.T) {
	store := session.NewMemoryStore()
	repo := NewSQLiteRepository(store, 128000, 90000, "model", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	events := &StoreEventAdapter{Store: store}
	offer := testOffer()
	offer.TurnID = "turn_prev"
	payload, err := json.Marshal(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID, TurnID: "turn_prev", Text: "done", FollowUp: offer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(context.Background(), session.EventRecord{SessionID: sess.ID, TurnID: "turn_prev", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: payload}); err != nil {
		t.Fatal(err)
	}

	builder := &fakeRunBuilder{}
	executor := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
	reply := session.FollowUpReply{TurnID: "turn_prev", FollowUpID: offer.ID, OptionID: "opt_1"}
	if _, err := executor.ExecuteWithRequestIDAndFollowUp(context.Background(), sess.ID, "req_follow", "改严", "", "", reply); err != nil {
		t.Fatal(err)
	}
	if builder.lastCtx.ReplyTo == nil || *builder.lastCtx.ReplyTo != reply {
		t.Fatalf("runtime reply = %+v", builder.lastCtx.ReplyTo)
	}
	input, err := repo.(TurnInputRepository).TurnInput(context.Background(), sess.ID, "req_follow")
	if err != nil {
		t.Fatal(err)
	}
	if input.ReplyTo == nil || *input.ReplyTo != reply {
		t.Fatalf("durable reply = %+v", input.ReplyTo)
	}
}

func TestDismissFollowUpPersistsNonTurnEvent(t *testing.T) {
	store := session.NewMemoryStore()
	repo := NewSQLiteRepository(store, 128000, 90000, "model", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	events := &StoreEventAdapter{Store: store}
	offer := testOffer()
	offer.TurnID = "turn_prev"
	payload, err := json.Marshal(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID, TurnID: "turn_prev", FollowUp: offer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(context.Background(), session.EventRecord{SessionID: sess.ID, TurnID: "turn_prev", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: payload}); err != nil {
		t.Fatal(err)
	}

	executor := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})
	reply := session.FollowUpReply{TurnID: "turn_prev", FollowUpID: offer.ID}
	if err := executor.DismissFollowUp(context.Background(), sess.ID, reply); err != nil {
		t.Fatal(err)
	}
	if err := executor.DismissFollowUp(context.Background(), sess.ID, reply); err != nil {
		t.Fatalf("idempotent dismissal: %v", err)
	}
	records, err := events.Replay(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 2 || records[len(records)-1].Kind != string(agent.EventFollowUpDismissed) {
		t.Fatalf("records = %+v", records)
	}
	var dismissed agent.Event
	if err := json.Unmarshal(records[len(records)-1].Payload, &dismissed); err != nil {
		t.Fatal(err)
	}
	if dismissed.FollowUpID != offer.ID || dismissed.TurnID != "turn_prev" {
		t.Fatalf("dismissed = %+v", dismissed)
	}
}
