package collabflux

import (
	"code-agent/internal/collab"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestExecutorModelRetainedAcrossRepairAndFinalize(t *testing.T) {
	const selected = "offline/selected"
	var checks atomic.Int32
	h := workflowModelFixture(t, 2, "", selected, func(_ context.Context, id string) error {
		if id != selected {
			return collab.ErrInvalid
		}
		checks.Add(1)
		return nil
	})
	for n, verdict := range []string{"request_changes", "pass"} {
		h.resolve(t, h.binding(t, fmt.Sprintf("wait_turn_%d", n+1)))
		binding := h.binding(t, fmt.Sprintf("wait_review_%d", n+1))
		d, err := h.reviews.DispatchAt(context.Background(), h.actor, h.run.RunID, n+1)
		if err != nil || d.Model != selected {
			t.Fatal(d, err)
		}
		h.submit(t, n+1, verdict)
		h.resolve(t, binding)
	}
	waitWorkflowSuccess(t, h)
	snap, err := h.tasks.Snapshot(context.Background(), h.actor, h.run.TaskID)
	if err != nil || snap.TaskAcceptance != "accepted" || snap.ManagedRun.ExecutorModel != selected || h.builder.badConfig.Load() || h.builder.calls.Load() != 2 || checks.Load() != 2 {
		t.Fatal(snap, err, checks.Load(), h.builder.badConfig.Load())
	}
}

func TestExecutorModelUnavailableBeforeRepairDoesNotFallback(t *testing.T) {
	var available atomic.Bool
	available.Store(true)
	h := workflowModelFixture(t, 2, "", "offline/selected", func(context.Context, string) error {
		if !available.Load() {
			return errors.New("removed")
		}
		return nil
	})
	h.resolve(t, h.binding(t, "wait_turn_1"))
	b := h.binding(t, "wait_review_1")
	available.Store(false)
	h.submit(t, 1, "request_changes")
	h.resolve(t, b)
	waitWorkflowSuccess(t, h)
	snap, err := h.tasks.Snapshot(context.Background(), h.actor, h.run.TaskID)
	if err != nil || snap.WaitReason != "executor_model_unavailable" || snap.TaskAcceptance == "accepted" || h.builder.calls.Load() != 1 {
		t.Fatal(snap, err, h.builder.calls.Load())
	}
	if _, err = h.reviews.DispatchAt(context.Background(), h.actor, h.run.RunID, 2); !errors.Is(err, collab.ErrNotFound) {
		t.Fatal("reserved a repair after model became unavailable", err)
	}
}

type modelPreflightRepository struct{ collab.Repository }

func (modelPreflightRepository) Snapshot(context.Context, collab.Principal, string) (collab.Snapshot, error) {
	return collab.Snapshot{}, nil
}

func TestExecutorModelPreflightRejectsBeforePreparingRun(t *testing.T) {
	a := collab.Principal{ID: "owner", LocalSessions: true}
	for _, supported := range []bool{false, true} {
		s := &Service{opts: ServiceOptions{Tasks: modelPreflightRepository{}, Actor: a}}
		s.ready.Store(true)
		want := collab.ErrUnavailable
		if supported {
			want = collab.ErrExecutorModelUnavailable
			s.opts.ValidateExecutorModel = func(_ context.Context, id string) error {
				if id != "offline/missing" {
					t.Fatal(id)
				}
				return errors.New("private resolver error")
			}
		}
		// No runtime or repository writer is installed: preflight must stop first.
		_, err := s.StartRun(context.Background(), a, "task", collab.StartRunInput{MaxIterations: 1, ExecutorModel: "offline/missing"})
		if !errors.Is(err, want) || s.SupportsExecutorModel() != supported {
			t.Fatal(err, supported)
		}
	}
}

func TestExecutorModelIdempotentRetryIgnoresChangedAvailability(t *testing.T) {
	h := workflowModelFixture(t, 1, "", "offline/selected", func(context.Context, string) error { return nil })
	// Use the real durable prepared request and original workflow binding. A
	// retry must return its receipt even after credentials/configuration change.
	s := &Service{opts: ServiceOptions{Tasks: h.tasks, Actor: h.actor, ValidateExecutorModel: func(context.Context, string) error { t.Fatal("revalidated admitted Run"); return collab.ErrUnavailable }}, managed: h.managed, adapter: h.adapter}
	s.ready.Store(true)
	in := collab.StartRunInput{RequestID: "prepare", ExecutorModel: h.run.ExecutorModel, PlanArtifactID: h.run.PlanArtifactID, ExecutorParticipationID: h.run.ExecutorParticipationID, ReviewerParticipationID: h.run.ReviewerParticipationID, MaxIterations: h.run.MaxIterations, Files: h.run.ReviewPolicy.Files, ValidationCommands: h.run.ReviewPolicy.ValidationCommands}
	r, err := s.StartRun(context.Background(), h.actor, h.run.TaskID, in)
	if err != nil || r.ID != h.run.RunID {
		t.Fatal(r, err)
	}
	in.ExecutorModel = "offline/other"
	if _, err = s.StartRun(context.Background(), h.actor, h.run.TaskID, in); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
}
