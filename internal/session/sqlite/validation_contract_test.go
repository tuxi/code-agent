package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"code-agent/internal/session"
)

func TestValidationContractTypedStoreReloadAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "typed-contract.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	if err := store.Save(ctx, &session.Session{ID: "s", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: "s", TurnID: "t", RequestID: "r", PayloadHash: "fixture", ValidationContract: &session.ValidationContract{Version: 1, ValidationCommands: []string{"go version"}, ManualChecks: []string{"检查横屏"}}}
	event := session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", At: time.Now().UTC(), Payload: validationAcceptance(t, input, input.ValidationContract)}
	stored, created, seq, err := store.ReserveTurnInput(ctx, input, event)
	if err != nil || !created || stored.AcceptedSeq != seq {
		t.Fatal(stored, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.TurnInput(ctx, "s", "r")
	if err != nil || got.ValidationContract == nil || got.ValidationContract.ManualChecks[0] != "检查横屏" {
		t.Fatal(got, err)
	}
	pending, err := store.RecoverableTurnInputs(ctx)
	if err != nil || len(pending) != 1 || pending[0].ValidationContract == nil {
		t.Fatal(pending, err)
	}
	got.ValidationContract.ValidationCommands[0] = "mutated caller"
	again, err := store.TurnInput(ctx, "s", "r")
	if err != nil || again.ValidationContract.ValidationCommands[0] != "go version" {
		t.Fatal("mutable read changed stored fact", again, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE session_events SET payload='{"validation_contract":null,"validation_contract_hash":"broken"}' WHERE id=?`, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TurnInput(ctx, "s", "r"); !errors.Is(err, session.ErrInvalidValidationContract) {
		t.Fatal("corruption hidden", err)
	}
}

func TestValidationContractStoreRejectsMismatchedEnvelope(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "mismatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Save(ctx, &session.Session{ID: "s", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: "s", TurnID: "t", RequestID: "r", ValidationContract: &session.ValidationContract{Version: 1, ValidationCommands: []string{"wanted"}}}
	other := &session.ValidationContract{Version: 1, ValidationCommands: []string{"different"}}
	event := session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", At: time.Now().UTC(), Payload: validationAcceptance(t, input, other)}
	if _, _, _, err := store.ReserveTurnInput(ctx, input, event); !errors.Is(err, session.ErrInvalidValidationContract) {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM turn_inputs`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestValidationContractConcurrentStoresKeepOneAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-contract.db")
	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	if err := a.Save(ctx, &session.Session{ID: "s", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: "s", TurnID: "t", RequestID: "r", PayloadHash: "same-contract", ValidationContract: &session.ValidationContract{Version: 1, ValidationCommands: []string{"go version"}}}
	event := session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", At: time.Now().UTC(), Payload: validationAcceptance(t, input, input.ValidationContract)}
	type result struct {
		stored  session.TurnInput
		created bool
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, store := range []*Store{a, b} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			<-start
			stored, created, _, err := store.ReserveTurnInput(ctx, input, event)
			results <- result{stored, created, err}
		}(store)
	}
	close(start)
	wg.Wait()
	close(results)
	created := 0
	for result := range results {
		if result.err != nil || result.stored.ValidationContract == nil {
			t.Fatal(result)
		}
		if result.created {
			created++
		}
	}
	events, err := a.SessionEvents(ctx, "s")
	if err != nil || created != 1 || len(events) != 1 {
		t.Fatal(created, len(events), err)
	}
}
