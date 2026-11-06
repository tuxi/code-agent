package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

func runtimeInputFixture(t *testing.T) (*Store, *session.Session) {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sess := &session.Session{ID: "owner", CreatedAt: time.Now(), UpdatedAt: time.Now(), Messages: []model.Message{{Role: model.RoleSystem, Content: "system"}}}
	if err = s.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return s, sess
}
func sampleRuntimeInput(id string) session.RuntimeInput {
	return session.RuntimeInput{ID: id, SessionID: "owner", SourceTurnID: "origin", SourceCallID: "call", Kind: "background_job_completion", Content: `{"status":"exited","exit_code":0}`, CreatedAt: time.Now()}
}

func TestRuntimeInputAtomicRecordDedupAndCheckpointReopen(t *testing.T) {
	ctx := context.Background()
	s, sess := runtimeInputFixture(t)
	in := sampleRuntimeInput("n1")
	events := []session.EventRecord{{SessionID: "child", TurnID: "origin", Kind: "job_finished", At: time.Now(), Payload: []byte(`{}`)}, {SessionID: "owner", TurnID: "origin", Kind: "job_finished", At: time.Now(), Payload: []byte(`{}`)}}
	seqs, created, err := s.RecordRuntimeInput(ctx, in, events)
	if err != nil || !created || len(seqs) != 2 {
		t.Fatal(seqs, created, err)
	}
	if _, created, err = s.RecordRuntimeInput(ctx, in, events); err != nil || created {
		t.Fatal(created, err)
	}
	for _, id := range []string{"owner", "child"} {
		records, e := s.SessionEvents(ctx, id)
		if e != nil || len(records) != 1 {
			t.Fatal(id, records, e)
		}
	}
	ids, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "consumer", Deliver: true})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if len(sess.Messages) != 2 || !strings.Contains(sess.Messages[1].Content, "Runtime results") || sess.Messages[1].OriginTurnID != "" || !sess.Messages[1].RuntimeInput {
		t.Fatal("not a runtime context", sess.Messages)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	ids, err = s.CheckpointRuntimeInputs(ctx, reloaded, session.RuntimeInputBoundary{TurnID: "next", Deliver: true})
	if err != nil || len(ids) != 0 || len(reloaded.Messages) != 2 {
		t.Fatal("replayed input", ids, err)
	}
	if !reloaded.Messages[1].RuntimeInput {
		t.Fatal("lost durable runtime provenance")
	}
	conflict := in
	conflict.Content = "different"
	if _, _, err = s.RecordRuntimeInput(ctx, conflict, events); err == nil {
		t.Fatal("changed notification accepted")
	}
}

func TestRuntimeInputCheckpointRollbackDoesNotConsumeOrMutateHistory(t *testing.T) {
	ctx := context.Background()
	s, sess := runtimeInputFixture(t)
	in := sampleRuntimeInput("rollback")
	if _, _, err := s.RecordRuntimeInput(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	// Inject a real transactional failure after input delivery has been prepared.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_runtime_messages BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'fixture checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Deliver: true, Finalize: true}); err == nil {
		t.Fatal("failed save succeeded")
	}
	if len(sess.Messages) != 1 {
		t.Fatal("rollback mutated caller history")
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM runtime_inputs WHERE id='rollback'`).Scan(&state); err != nil || state != "pending" {
		t.Fatal(state, err)
	}
	_, _ = s.db.Exec(`DROP TRIGGER fail_runtime_messages`)
	ids, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Deliver: true})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
}

func TestRuntimeInputProducerRollbackReceiptsAndForeignIsolation(t *testing.T) {
	ctx := context.Background()
	s, sess := runtimeInputFixture(t)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_runtime_events BEFORE INSERT ON session_events BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordRuntimeInput(ctx, sampleRuntimeInput("failed"), []session.EventRecord{{SessionID: "owner", Kind: "job_finished"}}); err == nil {
		t.Fatal("event write did not fail")
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM runtime_inputs`).Scan(&n)
	if n != 0 {
		t.Fatal("half-committed notification")
	}
	_, _ = s.db.Exec(`DROP TRIGGER fail_runtime_events`)
	// A terminal tool result arrives before the producer recovers its write.
	if _, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Receipts: []string{"late"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordRuntimeInput(ctx, sampleRuntimeInput("late"), nil); err != nil {
		t.Fatal(err)
	}
	ids, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Deliver: true})
	if err != nil || len(ids) != 0 {
		t.Fatal("receipt duplicated result", ids, err)
	}
	other := &session.Session{ID: "other", Messages: []model.Message{{Role: model.RoleSystem, Content: "s"}}}
	if err = s.Save(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckpointRuntimeInputs(ctx, other, session.RuntimeInputBoundary{TurnID: "other-turn", Receipts: []string{"late"}}); err == nil {
		t.Fatal("foreign receipt accepted")
	}
	in := sampleRuntimeInput("late")
	in.SessionID = "other"
	if _, _, err = s.RecordRuntimeInput(ctx, in, nil); err == nil {
		t.Fatal("foreign producer accepted")
	}
}

func TestRuntimeInputFinalBoundaryLeavesLateResultForNextTurn(t *testing.T) {
	ctx := context.Background()
	s, sess := runtimeInputFixture(t)
	if _, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "finished", Deliver: true, Finalize: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordRuntimeInput(ctx, sampleRuntimeInput("late"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "finished", Deliver: true}); !errors.Is(err, session.ErrRuntimeInputClosed) {
		t.Fatal(err)
	}
	ids, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "new-user-turn", Deliver: true})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
}

func TestRuntimeInputArchiveDeletionAndBatchBound(t *testing.T) {
	ctx := context.Background()
	s, sess := runtimeInputFixture(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		if _, _, err := s.RecordRuntimeInput(ctx, sampleRuntimeInput(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Deliver: true})
	if err != nil || len(ids) != 8 {
		t.Fatal(ids, err)
	}
	ids, err = s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Deliver: true})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if _, err = s.Archive(ctx, sess.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RecordRuntimeInput(ctx, sampleRuntimeInput("archived"), nil); err == nil {
		t.Fatal("archived input accepted")
	}
	if _, err = s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: "t", Deliver: true}); err == nil {
		t.Fatal("archived session consumed input")
	}
	if err = s.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM runtime_inputs`).Scan(&n)
	if n != 0 {
		t.Fatal("orphaned inputs")
	}
}

func TestRuntimeInputConcurrentFinishOrNextUserDelivery(t *testing.T) {
	ctx := context.Background()
	s, sess := runtimeInputFixture(t)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("concurrent-%d", i)
		var delivered []string
		errs := make(chan error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := s.RecordRuntimeInput(ctx, sampleRuntimeInput(id), nil)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			var err error
			delivered, err = s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: id, Deliver: true, Finalize: true})
			errs <- err
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		next, err := s.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{TurnID: id + "-next-user", Deliver: true})
		if err != nil || len(delivered)+len(next) != 1 {
			t.Fatalf("lost or repeated race result: %v %v %v", delivered, next, err)
		}
	}
}

func TestRuntimeInputRejectsUndeliverableEncodedBody(t *testing.T) {
	s, _ := runtimeInputFixture(t)
	in := sampleRuntimeInput("oversized-encoded")
	in.Content = strings.Repeat("\x00", 3000)
	if _, _, err := s.RecordRuntimeInput(context.Background(), in, nil); err == nil {
		t.Fatal("accepted a poison inbox entry")
	}
}
