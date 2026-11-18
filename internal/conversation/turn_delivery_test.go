package conversation

import (
	"code-agent/internal/agent"
	"code-agent/internal/session"
	sqlitestore "code-agent/internal/session/sqlite"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestExecutorDeliveryPublishesPersistedApplicationWithoutReappend(t *testing.T) {
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
	executor := &TurnExecutor{}
	executor.SetTurnDeliveryInbox(inbox)
	live := []agent.Event{}
	pub := &sequencingEmitter{live: emitterFunc(func(e agent.Event) { live = append(live, e) })}
	runner := &agent.Runner{}
	if err = executor.configureTurnDelivery(ctx, "s", "t", runner, pub); err != nil {
		t.Fatal(err)
	}
	if _, _, err = inbox.AcceptSupplement(ctx, session.Supplement{SessionID: "s", RequestID: "supplement", ExpectedTurnID: "t", Text: "more"}); err != nil {
		t.Fatal(err)
	}
	result, err := runner.Delivery.CheckpointTurnDelivery(ctx, sess, session.TurnDeliveryBoundary{TurnID: "t", Deliver: true})
	if err != nil {
		t.Fatal(err)
	}
	runner.OnSupplementsApplied(result.Applications)
	records, err := store.SessionEvents(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || len(records) != 2 {
		t.Fatal(live, records)
	}
	var replay agent.Event
	if err = json.Unmarshal(records[1].Payload, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.SupplementApplication == nil || live[0].Seq != records[1].Seq || live[0].RequestID != replay.RequestID || !live[0].At.Equal(replay.At) {
		t.Fatal(live, replay)
	}
}
