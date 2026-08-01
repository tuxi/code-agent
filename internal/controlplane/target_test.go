package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

type targetEventStore struct{ records []session.EventRecord }

func (s *targetEventStore) Append(context.Context, session.EventRecord) (int64, error) { return 0, nil }
func (s *targetEventStore) Replay(context.Context, string) ([]session.EventRecord, error) {
	return append([]session.EventRecord(nil), s.records...), nil
}
func (s *targetEventStore) ReplaySince(_ context.Context, _ string, cursor int64) ([]session.EventRecord, error) {
	var out []session.EventRecord
	for _, record := range s.records {
		if record.Seq > cursor {
			out = append(out, record)
		}
	}
	return out, nil
}

func TestRuntimeTargetWaitIsTurnAndCursorScoped(t *testing.T) {
	oldPayload, _ := json.Marshal(agent.Event{Kind: agent.EventTurnFinished, SessionID: "s", TurnID: "old"})
	newPayload, _ := json.Marshal(agent.Event{Kind: agent.EventTurnFinished, SessionID: "s", TurnID: "wanted"})
	store := &targetEventStore{records: []session.EventRecord{
		{Seq: 4, SessionID: "s", TurnID: "wanted", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: newPayload},
		{Seq: 6, SessionID: "s", TurnID: "old", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: oldPayload},
		{Seq: 9, SessionID: "s", TurnID: "wanted", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: newPayload},
	}}
	target := &RuntimeTarget{events: store}
	completion, latest, err := target.EventsSince(context.Background(), "s", "wanted", 5)
	if err != nil {
		t.Fatal(err)
	}
	if completion == nil || completion.Cursor != 9 || completion.TurnID != "wanted" || latest != 9 {
		t.Fatalf("completion=%+v latest=%d", completion, latest)
	}
	completion, latest, err = target.EventsSince(context.Background(), "s", "wanted", 9)
	if err != nil || completion != nil || latest != 9 {
		t.Fatalf("post-terminal completion=%+v latest=%d err=%v", completion, latest, err)
	}
}
