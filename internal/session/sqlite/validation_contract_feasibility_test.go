package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/session"
)

// This verifies the existing transaction boundary, not a new wire feature.
// JSON metadata must survive restart without introducing another datastore.
func TestValidationContractEnvelopeExistingTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contract-feasibility.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Save(ctx, &session.Session{ID: "s", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: "s", RequestID: "r", TurnID: "t", PayloadHash: "fixture-identity", Text: "offline transaction fixture"}
	contract := &session.ValidationContract{Version: 1, ValidationCommands: []string{"go version"}}
	payload := validationAcceptance(t, input, contract)
	accepted := session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", At: time.Now().UTC(), Payload: payload}
	stored, created, seq, err := store.ReserveTurnInput(ctx, input, accepted)
	if err != nil || !created || stored.AcceptedSeq != seq {
		t.Fatalf("reserve: %+v %v %d %v", stored, created, seq, err)
	}
	// A duplicate cannot overwrite the original event/contract, even if the
	// caller supplies a different candidate Turn and acceptance payload.
	input.TurnID = "different-candidate"
	accepted.TurnID = input.TurnID
	accepted.Payload = json.RawMessage(`{"validation_contract":{"version":1,"validation_commands":["must not replace"]}}`)
	duplicate, created, _, err := store.ReserveTurnInput(ctx, input, accepted)
	if err != nil || created || duplicate.TurnID != "t" || duplicate.AcceptedSeq != seq {
		t.Fatalf("duplicate: %+v %v %v", duplicate, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.TurnInput(ctx, "s", "r")
	if err != nil || loaded.AcceptedSeq != seq || loaded.TurnID != "t" {
		t.Fatal(loaded, err)
	}
	events, err := store.SessionEvents(ctx, "s")
	if err != nil || len(events) != 1 || events[0].Seq != seq || string(events[0].Payload) != string(payload) {
		t.Fatalf("event/contract changed: %+v %v", events, err)
	}
}

func TestValidationContractEnvelopeRollsBackWithAcceptance(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "contract-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Save(ctx, &session.Session{ID: "s", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	// Abort exactly between inserting the inbox row and inserting acceptance.
	_, err = store.db.ExecContext(ctx, `CREATE TRIGGER reject_fixture_acceptance BEFORE INSERT ON session_events WHEN NEW.kind='turn_accepted' BEGIN SELECT RAISE(ABORT, 'fixture acceptance failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: "s", RequestID: "r", TurnID: "t", PayloadHash: "fixture-identity", Text: "fixture"}
	input.ValidationContract = &session.ValidationContract{Version: 1, ValidationCommands: []string{"go version"}}
	event := session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", At: time.Now().UTC(), Payload: validationAcceptance(t, input, input.ValidationContract)}
	if _, _, _, err := store.ReserveTurnInput(ctx, input, event); err == nil {
		t.Fatal("injected acceptance failure was ignored")
	}
	var inputs, events int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM turn_inputs WHERE session_id='s'`).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_events WHERE session_id='s'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if inputs != 0 || events != 0 {
		t.Fatalf("partial admission persisted: inputs=%d events=%d", inputs, events)
	}
}

func validationAcceptance(t *testing.T, input session.TurnInput, contract *session.ValidationContract) json.RawMessage {
	t.Helper()
	hash, err := contract.Hash()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"kind": "turn_accepted", "session_id": input.SessionID, "turn_id": input.TurnID, "request_id": input.RequestID, "validation_contract": contract, "validation_contract_hash": hash})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
