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
)

type interruptedQueueRunner struct{ ctx RuntimeContext }

func (r interruptedQueueRunner) RunTurn(context.Context, *session.Session, string) (agent.TurnResult, error) {
	return agent.TurnResult{}, errors.New("interrupted input must resume")
}
func (r interruptedQueueRunner) ResumeTurn(_ context.Context, s *session.Session) (agent.TurnResult, error) {
	users := 0
	for _, m := range s.Messages {
		if m.Role == model.RoleUser && m.OriginTurnID == r.ctx.TurnID {
			users++
		}
	}
	if users != 1 {
		return agent.TurnResult{}, errors.New("resume duplicated user input")
	}
	r.ctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnResumed, SessionID: s.ID, TurnID: r.ctx.TurnID})
	r.ctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: s.ID, TurnID: r.ctx.TurnID, At: time.Now()})
	return agent.TurnResult{Final: "resumed"}, nil
}

type interruptedQueueBuilder struct {
	queueTestBuilder
	builds int
}

func (b *interruptedQueueBuilder) Build(ctx RuntimeContext) TurnRunner {
	b.builds++
	b.lastCtx = ctx
	return interruptedQueueRunner{ctx}
}

func TestInterruptedQueueUsesOriginalIdentityAndRevalidatesAfterPermit(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "settings_changed"}[changed], func(t *testing.T) {
			ctx := context.Background()
			s, err := db.New(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			if err = s.Save(ctx, sess); err != nil {
				t.Fatal(err)
			}
			first, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "s", RequestID: "first", TurnID: "first-turn", PayloadHash: "h", Text: "first"}, session.EventRecord{SessionID: "s", TurnID: "first-turn", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.StartTurnInput(ctx, first, sess); err != nil {
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
			payload, _ := json.Marshal(map[string]any{"Kind": "turn_finished", "SessionID": "s", "TurnID": first.TurnID})
			if _, err = q.FinishUserQueueTurn(ctx, session.UserQueueTurnEnd{SessionID: "s", RequestID: first.RequestID, TurnID: first.TurnID, State: session.TurnInputCompleted, Event: session.EventRecord{SessionID: "s", TurnID: first.TurnID, Kind: "turn_finished", At: time.Now(), Payload: payload}}); err != nil {
				t.Fatal(err)
			}
			input, _, _, err := q.ReserveUserQueueClaim(ctx, "s", "a", frozen)
			if err != nil {
				t.Fatal(err)
			}
			sess, err = s.Load(ctx, "s")
			if err != nil {
				t.Fatal(err)
			}
			sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: input.Text, OriginTurnID: input.TurnID})
			if _, err = q.StartUserQueueClaim(ctx, input, sess, frozen); err != nil {
				t.Fatal(err)
			}
			if err = s.PauseInterruptedSessions(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = q.ReconcileUserQueue(ctx, "s"); err != nil {
				t.Fatal(err)
			}
			page, err := q.ListUserQueue(ctx, "s", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err = q.ControlUserQueue(ctx, "s", "owner", session.UserQueueControl{Action: "resume_interrupted", RequestID: "a", Revision: &page.Revision}); err != nil {
				t.Fatal(err)
			}
			builder := &interruptedQueueBuilder{}
			e := NewTurnExecutor(NewSQLiteRepository(s, 10000, 8000, "resolved", "", nil), &StoreEventAdapter{Store: s}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
			validations := 0
			result, err := e.ResumeInterruptedUserQueue(ctx, "s", q, func(_ context.Context, _ string, current session.QueueExecutionContext) (session.QueueExecutionContext, error) {
				validations++
				if validations == 2 && changed {
					current.ApprovalMode = "full"
				}
				return current, nil
			})
			if changed {
				if !errors.Is(err, session.ErrUserQueueContextChanged) || builder.builds != 0 {
					t.Fatal(err, builder.builds)
				}
				page, err = q.ListUserQueue(ctx, "s", nil, 0)
				if err != nil || page.PauseReason != "resume_validation_failed" {
					t.Fatal(page, err)
				}
			} else {
				if err != nil || result.Final != "resumed" || builder.builds != 1 || builder.lastCtx.ReasoningEffort != "high" || builder.lastCtx.TurnID != input.TurnID || validations != 2 {
					t.Fatal(result, err, builder.builds, builder.lastCtx.ReasoningEffort, validations)
				}
				if _, err = e.ResumeInterruptedUserQueue(ctx, "s", q, func(_ context.Context, _ string, c session.QueueExecutionContext) (session.QueueExecutionContext, error) {
					return c, nil
				}); err == nil {
					t.Fatal("resume request reused")
				}
				records, err := s.SessionEvents(ctx, "s")
				if err != nil {
					t.Fatal(err)
				}
				starts, resumes := 0, 0
				for _, record := range records {
					if record.TurnID == input.TurnID {
						if record.Kind == "user_queue_started" {
							starts++
						}
						if record.Kind == "user_queue_resumed" {
							resumes++
						}
					}
				}
				if starts != 1 || resumes != 1 {
					t.Fatal(starts, resumes)
				}
			}
		})
	}
}
