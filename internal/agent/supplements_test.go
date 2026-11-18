package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
	sqlitestore "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
)

func runnerSupplementFixture(t *testing.T) (*sqlitestore.Store, *sqlitestore.SupplementInbox, *session.Session) {
	t.Helper()
	store, sess := runtimeFixture(t)
	ctx := context.Background()
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: "task", OriginTurnID: "target"})
	input, _, _, err := store.ReserveTurnInput(ctx, session.TurnInput{SessionID: sess.ID, RequestID: "initial", TurnID: "target", Text: "task", PayloadHash: "initial"}, session.EventRecord{SessionID: sess.ID, TurnID: "target", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
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
	if err = inbox.OpenSupplementBoundary(ctx, sess.ID, "target"); err != nil {
		t.Fatal(err)
	}
	return store, inbox, sess
}

func enqueueSupplement(t *testing.T, inbox session.SupplementStore, id string) {
	t.Helper()
	_, _, err := inbox.AcceptSupplement(context.Background(), session.Supplement{SessionID: "owner", RequestID: id, ExpectedTurnID: "target", Text: id})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSupplementsReachRealModelBoundaryAndPreventEarlyFinish(t *testing.T) {
	_, inbox, sess := runnerSupplementFixture(t)
	enqueueSupplement(t, inbox, "before")
	calls, finished, receipts := 0, 0, 0
	r := &Runner{Supplements: inbox, ReservedTurnID: "target", RequestID: "initial", MaxSteps: 4, Tools: tools.NewRegistry(), Emitter: validationEmitter(func(e Event) {
		if e.Kind == EventTurnFinished {
			finished++
		}
	})}
	r.OnSupplementsApplied = func(items []session.SupplementApplication) {
		for _, item := range items {
			saved, err := inbox.Supplement(context.Background(), "owner", item.RequestID)
			if err != nil || saved.State != "applied" || item.EventSequence == 0 {
				t.Fatal(saved, item, err)
			}
			receipts++
		}
	}
	r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
		calls++
		wanted := "before"
		if calls == 2 {
			wanted = "during"
		}
		found := false
		for _, m := range req.Messages {
			if m.Role == model.RoleUser && m.Content == wanted {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in request %d", wanted, calls)
		}
		if calls == 1 {
			enqueueSupplement(t, inbox, "during")
			return model.Response{Content: "initial answer"}, nil
		}
		return model.Response{Content: "updated answer"}, nil
	})
	result, err := r.RunPreparedTurn(context.Background(), sess)
	if err != nil || result.Final != "updated answer" || calls != 2 || finished != 1 || receipts != 2 {
		t.Fatal(result, err, calls, finished, receipts)
	}
	if _, _, err = inbox.AcceptSupplement(context.Background(), session.Supplement{SessionID: "owner", RequestID: "late", ExpectedTurnID: "target", Text: "late"}); !errors.Is(err, session.ErrSupplementTargetClosed) {
		t.Fatal(err)
	}
}

func TestSupplementsDoNotReplenishStepBudget(t *testing.T) {
	_, inbox, sess := runnerSupplementFixture(t)
	calls, finished := 0, 0
	r := &Runner{Supplements: inbox, ReservedTurnID: "target", RequestID: "initial", MaxSteps: 1, Tools: tools.NewRegistry(), Emitter: validationEmitter(func(e Event) {
		if e.Kind == EventTurnFinished {
			finished++
		}
	})}
	r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
		calls++
		enqueueSupplement(t, inbox, "pending")
		return model.Response{ToolCalls: []model.ToolCall{{ID: "call", Type: "function", Function: model.FunctionCall{Name: "unavailable_test_tool", Arguments: `{}`}}}}, nil
	})
	_, err := r.RunPreparedTurn(context.Background(), sess)
	if !errors.Is(err, session.ErrSupplementPending) || calls != 1 || finished != 0 {
		t.Fatal(err, calls, finished)
	}
	receipt, err := inbox.Supplement(context.Background(), "owner", "pending")
	if err != nil || receipt.State != "pending_apply" {
		t.Fatal(receipt, err)
	}
}

func TestSupplementsRejectIndependentFinishTransactions(t *testing.T) {
	store, inbox, sess := runnerSupplementFixture(t)
	r := &Runner{Supplements: inbox, RuntimeInputs: store, ReservedTurnID: "target", Model: runtimeProvider(func(model.Request) (model.Response, error) {
		t.Fatal("unsafe combined delivery called model")
		return model.Response{}, nil
	})}
	if _, err := r.RunPreparedTurn(context.Background(), sess); err == nil {
		t.Fatal("independent finalization silently accepted")
	}
}

func TestSharedDeliveryKeepsBothInboxesOpenUntilModelHandlesAllInputs(t *testing.T) {
	store, inbox, sess := runnerSupplementFixture(t)
	enqueueSupplement(t, inbox, "before")
	enqueueRuntime(t, store, "before-result")
	calls, finishes := 0, 0
	r := &Runner{Delivery: inbox, Supplements: inbox, RuntimeInputs: store, ReservedTurnID: "target", RequestID: "initial", MaxSteps: 4, Tools: tools.NewRegistry(), Emitter: validationEmitter(func(e Event) {
		if e.Kind == EventTurnFinished {
			finishes++
		}
	})}
	r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
		calls++
		if runtimeCount(req.Messages) != calls {
			t.Fatalf("missing runtime input in request %d", calls)
		}
		wanted := "before"
		if calls == 2 {
			wanted = "during"
		}
		found := false
		for _, m := range req.Messages {
			if m.Content == wanted {
				found = true
			}
		}
		if !found {
			t.Fatal("missing supplement", wanted)
		}
		if calls == 1 {
			enqueueRuntime(t, store, "during-result")
			enqueueSupplement(t, inbox, "during")
		}
		return model.Response{Content: "answer"}, nil
	})
	result, err := r.RunPreparedTurn(context.Background(), sess)
	if err != nil || calls != 2 || finishes != 1 || result.Final != "answer" {
		t.Fatal(result, err, calls, finishes)
	}
	if _, err = store.CheckpointRuntimeInputs(context.Background(), sess, session.RuntimeInputBoundary{TurnID: "target", Deliver: true}); !errors.Is(err, session.ErrRuntimeInputClosed) {
		t.Fatal(err)
	}
}
