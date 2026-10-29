package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

func TestManagedHostLockExcludesSecondOwner(t *testing.T) {
	h := reviewFixture(t, 1)
	ctx := context.Background()
	rec := h.r.(collab.RecoveryRepository)
	release, err := rec.LockManagedHost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if other, err := rec.LockManagedHost(ctx); !errors.Is(err, collab.ErrManagedWorkspace) {
		if other != nil {
			other()
		}
		t.Fatal("second owner entered", err)
	}
	release()
	again, err := rec.LockManagedHost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestManagedRecoveryFencesUnknownSideEffectAndRetainsLease(t *testing.T) {
	h := reviewFixture(t, 1)
	ctx := context.Background()
	d := h.first(t)
	release, err := h.s.AcquireTaskExecution(ctx, d.SessionID, h.root, d.RequestID, &d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	in := session.TurnInput{SessionID: d.SessionID, RequestID: d.RequestID, TurnID: "interrupted", Text: d.Message, PayloadHash: "hash", ManagedTask: &d.Ref}
	if _, _, _, err = h.s.ReserveTurnInput(ctx, in, session.EventRecord{SessionID: d.SessionID, TurnID: in.TurnID, Kind: "turn_accepted", Payload: json.RawMessage(`{}`), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(h.root, "main.txt"), []byte("side effect already happened"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := h.r.(collab.RecoveryRepository)
	check, err := rec.ReconcileRun(ctx, taskOwner, h.run.RunID)
	if err != nil || check.Safe || check.Run.Epoch != "2" || check.Reason != "writer_lease_survived_restart" {
		t.Fatal(check, err)
	}
	var leases, claims int
	if err = h.s.db.QueryRow(`SELECT COUNT(*) FROM coding_task_execution_leases WHERE run_id=?`, h.run.RunID).Scan(&leases); err != nil || leases != 1 {
		t.Fatal("stale lease discarded", leases, err)
	}
	if err = h.s.db.QueryRow(`SELECT COUNT(*) FROM coding_task_workspace_claims WHERE run_id=?`, h.run.RunID).Scan(&claims); err != nil || claims != 1 {
		t.Fatal(claims, err)
	}
	first, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	again, err := rec.ReconcileRun(ctx, taskOwner, h.run.RunID)
	if err != nil || again.Run.Epoch != "2" {
		t.Fatal(again, err)
	}
	second, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	if first.Revision != second.Revision {
		t.Fatal("repeated recovery emitted or incremented epoch")
	}
	if _, err = h.m.Dispatch(ctx, taskOwner, d.Ref); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("old callback survived fence", err)
	}
	if _, err = h.s.AcquireTaskExecution(ctx, "s2", h.root, "plain", nil); !errors.Is(err, collab.ErrManagedWorkspace) {
		t.Fatal("claim lost", err)
	}
}

func TestManagedRecoveryUsesCanonicalPathsForBackgroundHistory(t *testing.T) {
	h := reviewFixture(t, 1)
	ctx := context.Background()
	raw := json.RawMessage(`{"ToolName":"run_command","ToolArgs":"{\"command\":\"sleep 1\",\"background\":true}"}`)
	if _, err := h.s.RecordEvent(ctx, session.EventRecord{SessionID: "s1", TurnID: "old", Kind: "tool_started", At: time.Now(), Payload: raw}); err != nil {
		t.Fatal(err)
	}
	check, err := h.r.(collab.RecoveryRepository).ReconcileRun(ctx, taskOwner, h.run.RunID)
	if err != nil || check.Safe || check.Reason != "unproven_background_or_client_process" {
		t.Fatal(check, err)
	}
}

func TestManagedRecoveryOnlyReusesUnadmittedUnchangedReservation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "changed"
		}
		t.Run(name, func(t *testing.T) {
			h := reviewFixture(t, 1)
			h.first(t)
			if changed {
				if err := os.WriteFile(filepath.Join(h.root, "main.txt"), []byte("external"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			check, err := h.r.(collab.RecoveryRepository).ReconcileRun(context.Background(), taskOwner, h.run.RunID)
			if err != nil || check.Safe == changed {
				t.Fatal(check, err)
			}
			if changed && check.Reason != "unadmitted_candidate_changed" {
				t.Fatal(check)
			}
		})
	}
}

func TestManagedRecoveryObservesCompletedTurnWithoutReset(t *testing.T) {
	h := reviewFixture(t, 1)
	d := h.finish(t, h.first(t), true, "")
	check, err := h.r.(collab.RecoveryRepository).ReconcileRun(context.Background(), taskOwner, h.run.RunID)
	if err != nil || !check.Safe || check.Run.Epoch != "1" {
		t.Fatal(check, err)
	}
	d2, err := h.v.DispatchAt(context.Background(), taskOwner, h.run.RunID, 1)
	if err != nil || d2.TurnID != d.TurnID || d2.AttemptID != d.AttemptID {
		t.Fatal(d2, err)
	}
}
