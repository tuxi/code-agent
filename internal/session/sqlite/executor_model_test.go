package sqlite

import (
	"code-agent/internal/collab"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestExecutorModelPersistsAndConflictingRequestsDoNotDispatch(t *testing.T) {
	s, r, m, _, id, in := managedFixture(t)
	ctx := context.Background()
	in.ExecutorModel = "offline/selected"
	run := preparedRun(t, m, id, in)
	if run.ExecutorModel != in.ExecutorModel {
		t.Fatal(run)
	}
	if receipt, err := m.PrepareRun(ctx, taskOwner, id, in); err != nil || receipt.ID != run.RunID {
		t.Fatal(receipt, err)
	}
	changed := in
	changed.ExecutorModel = "offline/other"
	if _, err := m.PrepareRun(ctx, taskOwner, id, changed); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
	d := submittedDispatch(t, m, run)
	if d.Model != in.ExecutorModel {
		t.Fatal(d)
	}
	if _, err := m.ReserveDispatch(ctx, taskOwner, run.RunID, collab.ReserveDispatchInput{RequestID: "wrong-model", Epoch: run.Epoch, Iteration: 1, Message: "implement", Model: "offline/other"}); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil || snap.ManagedRun.ExecutorModel != in.ExecutorModel {
		t.Fatal(snap, err)
	}
	path := s.path
	s.Close()
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.TaskRepository("server").(collab.ManagedRepository).Run(ctx, taskOwner, run.RunID)
	if err != nil || saved.ExecutorModel != in.ExecutorModel {
		t.Fatal(saved, err)
	}
}

func TestDefaultExecutorModelPreservesLegacyIdempotencyPayload(t *testing.T) {
	// The old internal DTO had exported Go field names, including nil policy.
	legacy := struct {
		RequestID, PlanArtifactID, ExecutorParticipationID, ReviewerParticipationID string
		MaxIterations                                                               int
		Workflow                                                                    collab.WorkflowBinding
		ReviewPolicy                                                                *collab.ReviewPolicy
	}{RequestID: "legacy", MaxIterations: 1}
	current := collab.PrepareRunInput{RequestID: "legacy", MaxIterations: 1}
	a, _ := json.Marshal(legacy)
	b, _ := json.Marshal(current)
	if string(a) != string(b) {
		t.Fatalf("default payload changed: %s / %s", a, b)
	}
}
