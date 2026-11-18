package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
	db "code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
)

type queueTestBuilder struct{ fakeRunBuilder }

func (b *queueTestBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "resolved", Model: "resolved"}, nil
}

func TestQueueExecutorFrozenEffortAndSecondValidation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "frozen", true: "changed_after_permit"}[changed], func(t *testing.T) {
			ctx := context.Background()
			s, err := db.New(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sess := &session.Session{ID: "s", ReasoningEffort: "low", Messages: []model.Message{{Role: model.RoleSystem, Content: "system"}}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			if err = s.Save(ctx, sess); err != nil {
				t.Fatal(err)
			}
			in, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "s", RequestID: "first", TurnID: "first-turn", PayloadHash: "h", Text: "task"}, session.EventRecord{SessionID: "s", TurnID: "first-turn", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.StartTurnInput(ctx, in, sess); err != nil {
				t.Fatal(err)
			}
			q, err := db.NewUserQueue(s)
			if err != nil {
				t.Fatal(err)
			}
			frozen := session.QueueExecutionContext{WireModel: "selected", ResolvedModel: "resolved", ReasoningEffort: "high", AuthorizationScope: "owner", ApprovalMode: "ask"}
			if _, _, err = q.EnqueueUserMessage(ctx, session.QueuedMessage{SessionID: "s", RequestID: "next", Text: "next task", Context: frozen}); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"Kind": "turn_finished", "SessionID": "s", "TurnID": "first-turn"})
			if _, err = q.FinishUserQueueTurn(ctx, session.UserQueueTurnEnd{SessionID: "s", RequestID: "first", TurnID: "first-turn", State: session.TurnInputCompleted, Event: session.EventRecord{SessionID: "s", TurnID: "first-turn", Kind: "turn_finished", At: time.Now(), Payload: body}}); err != nil {
				t.Fatal(err)
			}
			builder := &queueTestBuilder{}
			e := NewTurnExecutor(NewSQLiteRepository(s, 10000, 8000, "resolved", "", nil), &StoreEventAdapter{Store: s}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
			calls := 0
			_, err = e.ExecuteUserQueueClaim(ctx, "s", "next", q, func(ctx context.Context, sid string, f session.QueueExecutionContext) (session.QueueExecutionContext, error) {
				calls++
				if calls == 2 && changed {
					f.ReasoningEffort = "low"
				}
				return f, nil
			})
			if changed {
				if !errors.Is(err, session.ErrUserQueueContextChanged) {
					t.Fatal(err)
				}
				page, pe := q.ListUserQueue(ctx, "s", nil, 0)
				if pe != nil || page.State != "paused" || len(page.Items) != 1 {
					t.Fatal(page, pe)
				}
			} else {
				if err != nil || builder.lastCtx.ReasoningEffort != "high" || calls != 2 {
					t.Fatal(err, builder.lastCtx.ReasoningEffort, calls)
				}
				page, pe := q.ListUserQueue(ctx, "s", nil, 0)
				if pe != nil || len(page.Items) != 0 {
					t.Fatal(page, pe)
				}
				events, pe := s.SessionEvents(ctx, "s")
				if pe != nil {
					t.Fatal(pe)
				}
				count := 0
				for _, ev := range events {
					if ev.Kind == "user_queue_started" {
						count++
					}
				}
				if count != 1 {
					t.Fatal(count)
				}
			}
		})
	}
}

type queueTerminalRunner struct {
	ctx  RuntimeContext
	fail bool
}

func (r queueTerminalRunner) RunTurn(ctx context.Context, s *session.Session, text string) (agent.TurnResult, error) {
	return r.ResumeTurn(ctx, s)
}
func (r queueTerminalRunner) ResumeTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	if r.fail {
		return agent.TurnResult{}, errors.New("controlled queue failure")
	}
	r.ctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: s.ID, TurnID: r.ctx.TurnID, At: time.Now()})
	return agent.TurnResult{}, nil
}
func (b *queueTestBuilder) Build(ctx RuntimeContext) TurnRunner {
	b.lastCtx = ctx
	return queueTerminalRunner{ctx: ctx}
}

type queueDrainBuilder struct {
	queueTestBuilder
	fail  bool
	calls int
}

func (b *queueDrainBuilder) Build(ctx RuntimeContext) TurnRunner {
	b.calls++
	return queueTerminalRunner{ctx: ctx, fail: b.fail}
}

type queueSaveFailureRepo struct {
	ConversationRepository
	TurnInputRepository
}

func (r queueSaveFailureRepo) Save(ctx context.Context, s *session.Session) error {
	if s.TurnStatus() == session.TurnStatusDone {
		return errors.New("controlled final checkpoint failure")
	}
	return r.ConversationRepository.Save(ctx, s)
}

func TestQueueDrainSequentialFailureAndCheckpoint(t *testing.T) {
	for _, scenario := range []string{"success", "runner_failure", "save_failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, err := db.New(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "system"}}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			if err = s.Save(ctx, sess); err != nil {
				t.Fatal(err)
			}
			in, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "s", RequestID: "first", TurnID: "first-turn", PayloadHash: "h", Text: "task"}, session.EventRecord{SessionID: "s", TurnID: "first-turn", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.StartTurnInput(ctx, in, sess); err != nil {
				t.Fatal(err)
			}
			q, err := db.NewUserQueue(s)
			if err != nil {
				t.Fatal(err)
			}
			frozen := session.QueueExecutionContext{WireModel: "selected", ResolvedModel: "resolved", ReasoningEffort: "high", AuthorizationScope: "owner", ApprovalMode: "ask"}
			for _, id := range []string{"a", "b"} {
				if _, _, err = q.EnqueueUserMessage(ctx, session.QueuedMessage{SessionID: "s", RequestID: id, Text: "task " + id, Context: frozen}); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(map[string]any{"Kind": "turn_finished", "SessionID": "s", "TurnID": "first-turn"})
			if _, err = q.FinishUserQueueTurn(ctx, session.UserQueueTurnEnd{SessionID: "s", RequestID: "first", TurnID: "first-turn", State: session.TurnInputCompleted, Event: session.EventRecord{SessionID: "s", TurnID: "first-turn", Kind: "turn_finished", At: time.Now(), Payload: body}}); err != nil {
				t.Fatal(err)
			}
			builder := &queueDrainBuilder{fail: scenario == "runner_failure"}
			repo := NewSQLiteRepository(s, 10000, 8000, "resolved", "", nil)
			if scenario == "save_failure" {
				repo = queueSaveFailureRepo{repo, repo.(TurnInputRepository)}
			}
			e := NewTurnExecutor(repo, &StoreEventAdapter{Store: s}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
			count, err := e.DrainUserQueue(ctx, "s", q, func(_ context.Context, _ string, f session.QueueExecutionContext) (session.QueueExecutionContext, error) {
				return f, nil
			}, 10)
			page, pe := q.ListUserQueue(ctx, "s", nil, 0)
			if pe != nil {
				t.Fatal(pe)
			}
			if scenario == "success" {
				if err != nil || count != 2 || builder.calls != 2 || len(page.Items) != 0 {
					t.Fatal(count, builder.calls, page, err)
				}
				records, re := s.SessionEvents(ctx, "s")
				if re != nil {
					t.Fatal(re)
				}
				started := 0
				for _, ev := range records {
					if ev.Kind == "user_queue_started" {
						started++
					}
				}
				if started != 2 {
					t.Fatal(started)
				}
				again, re := e.DrainUserQueue(ctx, "s", q, func(_ context.Context, _ string, f session.QueueExecutionContext) (session.QueueExecutionContext, error) {
					return f, nil
				}, 10)
				if re != nil || again != 0 || builder.calls != 2 {
					t.Fatal(again, re)
				}
			} else {
				if err == nil || count != 0 || builder.calls != 1 || page.State != "paused" || len(page.Items) != 1 || page.Items[0].Input.RequestID != "b" || page.Items[0].State != "queued" {
					t.Fatal(count, builder.calls, page, err)
				}
			}
		})
	}
}
