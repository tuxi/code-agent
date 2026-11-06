package sqlite

import (
	"context"
	"errors"
	"testing"

	"code-agent/internal/session"
)

func TestContinuationRegisterFinishRaceAndRevision(t *testing.T) {
	for _, before := range []bool{false, true} {
		s, sess := runtimeInputFixture(t)
		ctx := context.Background()
		if err := s.AdmitContinuationWork(ctx, sess.ID, "work"); err != nil {
			t.Fatal(err)
		}
		if before {
			if _, _, err := s.RecordRuntimeInput(ctx, sampleRuntimeInput("done"), nil); err != nil {
				t.Fatal(err)
			}
		}
		c, err := s.ParkContinuation(ctx, sess, session.Continuation{SessionID: sess.ID, TurnID: "work", Await: []string{"done"}, Binding: []byte(`{}`), Checkpoint: []byte(`{"budget":2}`)})
		if err != nil {
			t.Fatal(err)
		}
		if !before {
			if c.State != "waiting" {
				t.Fatal(c)
			}
			if _, _, err = s.RecordRuntimeInput(ctx, sampleRuntimeInput("done"), nil); err != nil {
				t.Fatal(err)
			}
		}
		c, err = s.Continuation(ctx, sess.ID)
		if err != nil || c.State != "ready" {
			t.Fatal(c, err)
		}
		if ok, err := s.ClaimContinuation(ctx, c); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if ok, err := s.ClaimContinuation(ctx, c); err != nil || ok {
			t.Fatal("duplicate worker", ok, err)
		}
		next, err := s.ParkContinuation(ctx, sess, session.Continuation{SessionID: sess.ID, TurnID: "work", Await: []string{"second"}, Binding: c.Binding, Checkpoint: c.Checkpoint})
		if err != nil || next.Revision != c.Revision+1 {
			t.Fatal(next, err)
		}
		if ok, err := s.ClaimContinuation(ctx, c); err != nil || ok {
			t.Fatal("stale revision acquired", ok, err)
		}
		if _, err = s.EndContinuation(ctx, sess.ID, "work", "stopped"); err != nil {
			t.Fatal(err)
		}
		if err = s.reopen(); err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.RecordRuntimeInput(ctx, sampleRuntimeInput("second"), nil); err != nil {
			t.Fatal(err)
		}
		stopped, err := s.Continuation(ctx, sess.ID)
		if err != nil || stopped.State != "stopped" {
			t.Fatal("stop fence lost", stopped, err)
		}
	}
}

func TestContinuationRollbackSupersedeAndArchive(t *testing.T) {
	s, sess := runtimeInputFixture(t)
	ctx := context.Background()
	if err := s.AdmitContinuationWork(ctx, sess.ID, "old"); err != nil {
		t.Fatal(err)
	}
	c := session.Continuation{SessionID: sess.ID, TurnID: "old", Await: []string{"done"}, Binding: []byte(`{}`), Checkpoint: []byte(`{}`)}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_park BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParkContinuation(ctx, sess, c); err == nil {
		t.Fatal("partial park committed")
	}
	if got, _ := s.Continuation(ctx, sess.ID); got.State != "active" || sess.TurnStatus() == session.TurnStatusWaitingBackground {
		t.Fatal("rollback changed wait state", got)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_park`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParkContinuation(ctx, sess, c); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitContinuationWork(ctx, sess.ID, "new-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParkContinuation(ctx, sess, c); !errors.Is(err, session.ErrContinuationStale) {
		t.Fatal("old work reclaimed session", err)
	}
	if ok, err := s.EndContinuation(ctx, sess.ID, "old", "stopped"); err != nil || ok {
		t.Fatal("old stop hit new work", ok, err)
	}
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Continuation(ctx, sess.ID); err == nil {
		t.Fatal("orphaned continuation")
	}
}
