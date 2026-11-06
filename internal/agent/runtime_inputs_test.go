package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
	sqlitestore "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
)

type runtimeProvider func(model.Request) (model.Response, error)

func (p runtimeProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	return p(req)
}

func runtimeFixture(t *testing.T) (*sqlitestore.Store, *session.Session) {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess := newSession()
	sess.ID = "owner"
	sess.CreatedAt = time.Now()
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return store, sess
}

func enqueueRuntime(t *testing.T, store session.RuntimeInputStore, id string) {
	t.Helper()
	_, _, err := store.RecordRuntimeInput(context.Background(), session.RuntimeInput{
		ID: id, SessionID: "owner", SourceTurnID: "origin", SourceCallID: "call",
		Kind: "background_job_completion", Content: `{"status":"exited","exit_code":0}`, CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func runtimeCount(messages []model.Message) int {
	n := 0
	for _, m := range messages {
		if strings.HasPrefix(m.Content, "[Runtime results;") {
			n++
		}
	}
	return n
}

func TestRuntimeInputAtModelBoundaryAndFinalRace(t *testing.T) {
	store, sess := runtimeFixture(t)
	enqueueRuntime(t, store, "before")
	calls, starts, finishes, intermediate := 0, 0, 0, 0
	r := &Runner{Tools: tools.NewRegistry(), MaxSteps: 4, RuntimeInputs: store,
		Emitter: validationEmitter(func(e Event) {
			switch e.Kind {
			case EventTurnStarted:
				starts++
			case EventTurnFinished:
				finishes++
			case EventAssistantText:
				intermediate++
			}
		}),
	}
	r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
		calls++
		if got := runtimeCount(req.Messages); got != calls {
			t.Fatalf("request %d: runtime messages=%d", calls, got)
		}
		if calls == 1 {
			// Completion commits while the request is in flight; the proposed
			// final answer must become intermediate narration instead.
			enqueueRuntime(t, store, "during")
			return model.Response{Content: "initial finding", FinishReason: "stop"}, nil
		}
		return model.Response{Content: "reviewed completion", FinishReason: "stop"}, nil
	})
	res, err := r.RunTurn(context.Background(), sess, "work")
	if err != nil || res.Final != "reviewed completion" || calls != 2 || starts != 1 || finishes != 1 || intermediate != 1 {
		t.Fatalf("result=%+v err=%v calls=%d lifecycle=%d/%d intermediate=%d", res, err, calls, starts, finishes, intermediate)
	}
	loaded, err := store.Load(context.Background(), sess.ID)
	if err != nil || runtimeCount(loaded.Messages) != 2 {
		t.Fatalf("durable history: %+v %v", loaded, err)
	}
	enqueueRuntime(t, store, "after")
	if _, err := store.CheckpointRuntimeInputs(context.Background(), sess, session.RuntimeInputBoundary{TurnID: res.TurnID, Deliver: true}); !errors.Is(err, session.ErrRuntimeInputClosed) {
		t.Fatalf("finished turn reopened: %v", err)
	}
	r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
		if runtimeCount(req.Messages) != 3 {
			t.Fatal("next user turn lost late completion")
		}
		return model.Response{Content: "next", FinishReason: "stop"}, nil
	})
	if _, err = r.RunTurn(context.Background(), sess, "next user input"); err != nil {
		t.Fatal(err)
	}
}

type receiptFailureStore struct{ session.RuntimeInputStore }

func (s receiptFailureStore) CheckpointRuntimeInputs(ctx context.Context, sess *session.Session, b session.RuntimeInputBoundary) ([]string, error) {
	if len(b.Receipts) > 0 {
		return nil, errors.New("receipt checkpoint unavailable")
	}
	return s.RuntimeInputStore.CheckpointRuntimeInputs(ctx, sess, b)
}

func TestRuntimeInputToolReceiptBeforeDelayedProducer(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			store, sess := runtimeFixture(t)
			probe := &outcomeTool{result: tools.ToolResult{Content: "terminal result", RuntimeInputReceipts: []string{"finished-job"}}}
			reg := tools.NewRegistry()
			mustRegister(t, reg, probe)
			var port session.RuntimeInputStore = store
			if fail {
				port = receiptFailureStore{store}
			}
			calls := 0
			r := &Runner{Tools: reg, RuntimeInputs: port, MaxSteps: 3}
			r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
				calls++
				if calls == 1 {
					return toolCallResp(probe.Name(), `{}`), nil
				}
				// Simulate the producer recovering storage only after the wait
				// result and its receipt have been durably checkpointed.
				enqueueRuntime(t, store, "finished-job")
				if runtimeCount(req.Messages) != 0 {
					t.Fatal("duplicate tool result")
				}
				return model.Response{Content: "done", FinishReason: "stop"}, nil
			})
			_, err := r.RunTurn(context.Background(), sess, "work")
			if fail {
				if err == nil || calls != 1 {
					t.Fatalf("continued past failed receipt: calls=%d err=%v", calls, err)
				}
				// TurnExecutor's error path always saves balanced history. Its
				// receipt must survive that independent save and a fresh runner.
				if err = store.Save(context.Background(), sess); err != nil {
					t.Fatal(err)
				}
				loaded, loadErr := store.Load(context.Background(), sess.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				enqueueRuntime(t, store, "finished-job")
				recovered := &Runner{Tools: reg, RuntimeInputs: store, MaxSteps: 1, Model: runtimeProvider(func(req model.Request) (model.Response, error) {
					if runtimeCount(req.Messages) != 0 {
						t.Fatal("error recovery duplicated terminal result")
					}
					return model.Response{Content: "recovered", FinishReason: "stop"}, nil
				})}
				if _, err = recovered.RunTurn(context.Background(), loaded, "continue"); err != nil {
					t.Fatal(err)
				}
				if len(runtimeInputReceipts(loaded)) != 0 {
					t.Fatal("recovered receipt not cleared")
				}
				return
			}
			if err != nil || calls != 2 || runtimeCount(sess.Messages) != 0 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			loaded, err := store.Load(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			var toolResults int
			for _, m := range loaded.Messages {
				if m.Role == model.RoleTool {
					toolResults++
				}
			}
			if toolResults != 1 {
				t.Fatal("receipt was not saved with tool history")
			}
		})
	}
}

func TestRuntimeInputDoesNotRefreshStepBudget(t *testing.T) {
	store, sess := runtimeFixture(t)
	calls := 0
	r := &Runner{Tools: tools.NewRegistry(), RuntimeInputs: store, MaxSteps: 2}
	r.Model = runtimeProvider(func(req model.Request) (model.Response, error) {
		calls++
		enqueueRuntime(t, store, fmt.Sprintf("n%d", calls))
		return model.Response{Content: "bounded answer", FinishReason: "stop"}, nil
	})
	res, err := r.RunTurn(context.Background(), sess, "work")
	if err != nil || !res.HitStepLimit || calls != 3 {
		t.Fatalf("unbounded continuation: %+v calls=%d err=%v", res, calls, err)
	}
	ids, err := store.CheckpointRuntimeInputs(context.Background(), sess, session.RuntimeInputBoundary{TurnID: "next-user", Deliver: true})
	if err != nil || len(ids) != 1 || ids[0] != "n3" {
		t.Fatalf("summary-time result lost: %v %v", ids, err)
	}
}

type receiptCancelTool struct {
	outcomeTool
	cancel context.CancelFunc
}

func (p *receiptCancelTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	p.cancel()
	return p.result, nil
}

func TestRuntimeInputReceiptPersistsWithCanceledBatch(t *testing.T) {
	store, sess := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &receiptCancelTool{outcomeTool: outcomeTool{result: tools.ToolResult{Content: "completed", RuntimeInputReceipts: []string{"done-before-cancel"}}}, cancel: cancel}
	reg := tools.NewRegistry()
	mustRegister(t, reg, probe)
	response := toolCallResp(probe.Name(), `{}`)
	response.ToolCalls = append(response.ToolCalls, response.ToolCalls[0])
	r := &Runner{Tools: reg, RuntimeInputs: store, MaxSteps: 3, Model: &scriptedProvider{responses: []model.Response{response}}}
	_, err := r.RunTurn(ctx, sess, "work")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertBalancedToolCalls(t, loaded.Messages)
	if len(runtimeInputReceipts(loaded)) != 0 {
		t.Fatal("receipt not committed during cancellation")
	}
	enqueueRuntime(t, store, "done-before-cancel")
	ids, err := store.CheckpointRuntimeInputs(context.Background(), loaded, session.RuntimeInputBoundary{TurnID: "next-user", Deliver: true})
	if err != nil || len(ids) != 0 {
		t.Fatal("cancel lost completed tool receipt", ids, err)
	}
}
