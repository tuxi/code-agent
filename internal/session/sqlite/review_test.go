package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

type reviewHarness struct {
	s     *Store
	r     collab.Repository
	m     collab.ManagedRepository
	v     collab.ReviewRepository
	root  string
	run   collab.ManagedRun
	input collab.PrepareRunInput
}

func reviewFixture(t *testing.T, max int) *reviewHarness {
	t.Helper()
	s, r, m, root, id, in := managedFixture(t)
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--", "main.txt"}, {"-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "-m", "baseline"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatal(string(out), err)
		}
	}
	in.MaxIterations = max
	in.ReviewPolicy = &collab.ReviewPolicy{Files: []string{"main.txt", "new.txt"}, ValidationCommands: []string{"check"}}
	run := preparedRun(t, m, id, in)
	return &reviewHarness{s: s, r: r, m: m, v: r.(collab.ReviewRepository), root: root, run: run, input: in}
}

func (h *reviewHarness) first(t *testing.T) collab.ManagedDispatch {
	t.Helper()
	return submittedDispatch(t, h.m, h.run)
}

// Persist a real inbox/Attempt/ordered EventStore stream. This helper is not a
// model or a fake Task state machine; the Flux integration uses TurnExecutor.
func (h *reviewHarness) finish(t *testing.T, d collab.ManagedDispatch, valid bool, extra string) collab.ManagedDispatch {
	t.Helper()
	ctx := context.Background()
	release, err := h.s.AcquireTaskExecution(ctx, d.SessionID, h.root, d.RequestID, &d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	turn := fmt.Sprintf("turn-%d", d.Iteration)
	in := session.TurnInput{SessionID: d.SessionID, RequestID: d.RequestID, TurnID: turn, Text: d.Message, ManagedTask: &d.Ref, PayloadHash: "offline"}
	if _, _, _, err = h.s.ReserveTurnInput(ctx, in, session.EventRecord{SessionID: d.SessionID, TurnID: turn, Kind: "turn_accepted", Payload: json.RawMessage(`{}`), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(h.root, "main.txt"), []byte(fmt.Sprintf("after %d\n", d.Iteration)), 0600); err != nil {
		t.Fatal(err)
	}
	emit := func(kind string, body any) {
		raw, _ := json.Marshal(body)
		if _, err = h.s.RecordEvent(ctx, session.EventRecord{SessionID: d.SessionID, TurnID: turn, Kind: kind, Payload: raw, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	emit("tool_started", map[string]any{"CallID": "edit", "ToolName": "edit_file", "ToolArgs": `{"path":"main.txt"}`})
	emit("tool_finished", map[string]any{"CallID": "edit", "ToolName": "edit_file"})
	if valid {
		emit("tool_started", map[string]any{"CallID": "test", "ToolName": "run_command", "ToolArgs": `{"command":"check"}`})
		emit("tool_finished", map[string]any{"CallID": "test", "ToolName": "run_command", "Output": map[string]any{"command": "check", "exit_code": 0, "decision": "confirm", "status": "completed"}})
	}
	if extra != "" {
		emit("tool_stdout", map[string]any{"CallID": "test", "Chunk": extra})
	}
	emit("turn_finished", map[string]any{})
	if err = h.s.SetTurnInputState(ctx, d.SessionID, d.RequestID, session.TurnInputCompleted); err != nil {
		t.Fatal(err)
	}
	d, err = h.m.Dispatch(ctx, taskOwner, d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (h *reviewHarness) capture(t *testing.T, d collab.ManagedDispatch) (collab.TaskArtifactRef, collab.ReviewBundle) {
	t.Helper()
	a, err := h.v.CaptureBundle(context.Background(), taskOwner, d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.v.ReadBundle(context.Background(), taskOwner, a.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func (h *reviewHarness) review(t *testing.T, a collab.TaskArtifactRef, b collab.ReviewBundle, verdict string) collab.ReviewInput {
	t.Helper()
	return collab.ReviewInput{RequestID: fmt.Sprintf("review-%d", b.Iteration), SubmissionID: fmt.Sprintf("submission-%d", b.Iteration), RunID: h.run.RunID, Epoch: h.run.Epoch, ReviewerParticipationID: h.run.ReviewerParticipationID, AttemptID: b.AttemptID, Plan: b.Plan, Artifact: collab.ArtifactBinding{ArtifactID: a.ArtifactID, Version: a.Version, Hash: *a.ContentHash}, Verdict: verdict, Findings: []string{"cover an edge case"}}
}

func TestReviewExactBindingRepairAndFinalization(t *testing.T) {
	h := reviewFixture(t, 2)
	ctx := context.Background()
	d := h.finish(t, h.first(t), true, "")
	a, b := h.capture(t, d)
	if b.ValidationStatus != "evidence_complete" || !strings.Contains(b.Patch, "+after 1") || string(b.Baseline.Files[0].Data) != "before\n" {
		t.Fatal(b)
	}
	if _, err := h.m.ReserveDispatch(ctx, taskOwner, h.run.RunID, collab.ReserveDispatchInput{RequestID: "bypass", Epoch: "1", Iteration: 2, Message: "repair"}); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
	in := h.review(t, a, b, "request_changes")
	recorded, err := h.v.SubmitReview(ctx, taskOwner, in)
	if err != nil || recorded.Status != "recorded" {
		t.Fatal(recorded, err)
	}
	snap, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	in.RequestID = "transport-retry"
	again, err := h.v.SubmitReview(ctx, taskOwner, in)
	if err != nil || again.ID != recorded.ID {
		t.Fatal(again, err)
	}
	after, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	if after.Revision != snap.Revision {
		t.Fatal("producer retry emitted twice")
	}
	gate, err := h.v.ApplyGate(ctx, taskOwner, h.run.RunID, recorded.ID)
	if err != nil || gate.Status != "gate_accepted" {
		t.Fatal(gate, err)
	}
	if _, err = h.v.FinalizeRun(ctx, taskOwner, h.run.RunID, recorded.ID); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("request_changes finalized", err)
	}
	repair := collab.ReserveDispatchInput{RequestID: "repair", Epoch: "1", Iteration: 2, Message: "repair from review", GateDecisionID: recorded.ID}
	if _, err = h.m.ReserveDispatch(ctx, taskOwner, h.run.RunID, repair); err != nil {
		t.Fatal(err)
	}
	d2, err := h.v.DispatchAt(ctx, taskOwner, h.run.RunID, 2)
	if err != nil || d2.SessionID != d.SessionID {
		t.Fatal(d2, err)
	}
	if _, err = h.s.AcquireTaskExecution(ctx, d.SessionID, h.root, d.RequestID, &d.Ref); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("old dispatch admitted", err)
	}
	if err = h.s.RequireTaskRecovery(ctx, d.SessionID, d.RequestID); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("old recovery callback changed current iteration", err)
	}
	d2 = h.finish(t, d2, true, "")
	a2, b2 := h.capture(t, d2)
	pass, err := h.v.SubmitReview(ctx, taskOwner, h.review(t, a2, b2, "pass"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.v.ApplyGate(ctx, taskOwner, h.run.RunID, pass.ID); err != nil {
		t.Fatal(err)
	}
	snap, _ = h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	if snap.TaskAcceptance == "accepted" || snap.State == "completed" {
		t.Fatal("gate receipt became task acceptance")
	}
	final, err := h.v.FinalizeRun(ctx, taskOwner, h.run.RunID, pass.ID)
	if err != nil || final.Status != "task_accepted" {
		t.Fatal(final, err)
	}
	again, err = h.v.FinalizeRun(ctx, taskOwner, h.run.RunID, pass.ID)
	if err != nil || again != final {
		t.Fatal(again, err)
	}
	snap, err = h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	if err != nil || snap.State != "completed" || snap.TaskAcceptance != "accepted" {
		t.Fatal(snap.CodingTask, err)
	}
	if err = h.s.RequireTaskRecovery(ctx, d2.SessionID, d2.RequestID); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("completed run reopened", err)
	}
	page, err := h.r.Events(ctx, taskOwner, h.run.TaskID, 0, 200)
	if err != nil || page.NextCursor != snap.SnapshotTaskSeq || page.Events[len(page.Events)-1].TaskRevision != snap.Revision {
		t.Fatal("snapshot/event mismatch", err)
	}
	old, err := h.v.ReadBundle(ctx, taskOwner, a.ArtifactID)
	if err != nil || old.Candidate.Hash() != b.Candidate.Hash() {
		t.Fatal("old artifact changed", err)
	}
	if release, err := h.s.AcquireTaskExecution(ctx, "s2", h.root, "ordinary", nil); err != nil {
		t.Fatal("claim not released", err)
	} else {
		release()
	}
}

func TestReviewRejectsForgedBindingsAndOldReport(t *testing.T) {
	h := reviewFixture(t, 2)
	ctx := context.Background()
	d := h.finish(t, h.first(t), true, "")
	a, b := h.capture(t, d)
	base := h.review(t, a, b, "pass")
	for _, tc := range []struct {
		name   string
		modify func(*collab.ReviewInput)
		want   error
	}{
		{"epoch", func(i *collab.ReviewInput) { i.Epoch = "2" }, collab.ErrConflict}, {"attempt", func(i *collab.ReviewInput) { i.AttemptID = "foreign" }, collab.ErrConflict},
		{"plan", func(i *collab.ReviewInput) { i.Plan.Version = "2" }, collab.ErrConflict}, {"artifact", func(i *collab.ReviewInput) { i.Artifact.ArtifactID = "foreign" }, collab.ErrConflict},
		{"hash", func(i *collab.ReviewInput) { i.Artifact.Hash = strings.Repeat("a", 64) }, collab.ErrConflict}, {"role", func(i *collab.ReviewInput) { i.ReviewerParticipationID = h.run.ExecutorParticipationID }, collab.ErrForbidden},
		{"empty", func(i *collab.ReviewInput) { i.Verdict = "" }, collab.ErrInvalid}, {"unknown", func(i *collab.ReviewInput) { i.Verdict = "approved" }, collab.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.modify(&in)
			if _, err := h.v.SubmitReview(ctx, taskOwner, in); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
	if _, err := h.v.SubmitReview(ctx, collab.Principal{ID: "other", LocalSessions: true}, base); !errors.Is(err, collab.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := h.v.SubmitReview(ctx, collab.Principal{ID: taskOwner.ID}, base); !errors.Is(err, collab.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := h.v.ReadBundle(ctx, collab.Principal{ID: "other", LocalSessions: true}, a.ArtifactID); !errors.Is(err, collab.ErrNotFound) {
		t.Fatal(err)
	}
	var seq int64
	if err := h.s.db.QueryRow(`SELECT id FROM session_events WHERE session_id=? AND turn_id=? AND kind='tool_finished' LIMIT 1`, d.SessionID, d.TurnID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	legacy, err := h.r.LinkArtifact(ctx, taskOwner, h.run.TaskID, collab.ArtifactInput{RequestID: "legacy", AttemptID: d.AttemptID, Kind: "patch", SourceSeq: strconv.FormatInt(seq, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Report(ctx, taskOwner, h.run.TaskID, "review", collab.ReportInput{RequestID: "b1-review", ParticipationID: h.run.ReviewerParticipationID, SubmissionID: "old", Iteration: 1, Content: "looks fine", Verdict: "pass", ReviewedArtifactID: legacy.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.v.Decision(ctx, taskOwner, h.run.RunID, 1); !errors.Is(err, collab.ErrNotFound) {
		t.Fatal("B1 report became managed gate", err)
	}
	first, err := h.v.SubmitReview(ctx, taskOwner, base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Findings = []string{"changed"}
	if _, err = h.v.SubmitReview(ctx, taskOwner, changed); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
	changed.RequestID = "other-request"
	if _, err = h.v.SubmitReview(ctx, taskOwner, changed); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("producer payload conflict", err)
	}
	base.RequestID = "another"
	base.SubmissionID = "another"
	if _, err = h.v.SubmitReview(ctx, taskOwner, base); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("second decision same candidate", err)
	}
	if first.ID == "" {
		t.Fatal("missing receipt")
	}
}

func TestReviewIncompleteExhaustedAndInconclusiveWait(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, extra, reason string
		valid                        bool
	}{
		{"missing_checks", "pass", "", "needs_validation", false}, {"truncated", "pass", "[output truncated]", "needs_validation", true},
		{"exhausted", "request_changes", "", "iterations_exhausted", true}, {"inconclusive", "inconclusive", "", "inconclusive_review", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := reviewFixture(t, 1)
			ctx := context.Background()
			d := h.finish(t, h.first(t), tc.valid, tc.extra)
			a, b := h.capture(t, d)
			receipt, err := h.v.SubmitReview(ctx, taskOwner, h.review(t, a, b, tc.verdict))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.v.ApplyGate(ctx, taskOwner, h.run.RunID, receipt.ID); err != nil {
				t.Fatal(err)
			}
			decision, err := h.v.Decision(ctx, taskOwner, h.run.RunID, 1)
			if err != nil || decision.Action != "attention" || decision.Reason != tc.reason {
				t.Fatal(decision, err)
			}
			snap, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
			if snap.State != "waiting" || snap.TaskAcceptance == "accepted" {
				t.Fatal(snap.CodingTask)
			}
		})
	}
}

func TestReviewFinalizationInvalidatesExternalChanges(t *testing.T) {
	for _, when := range []string{"before_gate", "before_finalize"} {
		t.Run(when, func(t *testing.T) {
			h := reviewFixture(t, 2)
			ctx := context.Background()
			d := h.finish(t, h.first(t), true, "")
			a, b := h.capture(t, d)
			receipt, err := h.v.SubmitReview(ctx, taskOwner, h.review(t, a, b, "pass"))
			if err != nil {
				t.Fatal(err)
			}
			if when == "before_gate" {
				if err = os.WriteFile(filepath.Join(h.root, "main.txt"), []byte("external"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = h.v.ApplyGate(ctx, taskOwner, h.run.RunID, receipt.ID); err != nil {
				t.Fatal(err)
			}
			if when == "before_finalize" {
				if err = os.WriteFile(filepath.Join(h.root, "main.txt"), []byte("external"), 0600); err != nil {
					t.Fatal(err)
				}
				final, err := h.v.FinalizeRun(ctx, taskOwner, h.run.RunID, receipt.ID)
				if err != nil || final.Status != "needs_attention" {
					t.Fatal(final, err)
				}
			}
			snap, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
			if snap.State != "waiting" || snap.WaitReason != "candidate_invalidated" || snap.TaskAcceptance == "accepted" {
				t.Fatal(snap.CodingTask)
			}
			old, err := h.v.ReadBundle(ctx, taskOwner, a.ArtifactID)
			if err != nil || old.Candidate.Hash() != b.Candidate.Hash() {
				t.Fatal("immutable bytes changed", err)
			}
		})
	}
}

func TestReviewTransactionsImmutabilityAndRestart(t *testing.T) {
	h := reviewFixture(t, 2)
	ctx := context.Background()
	d := h.finish(t, h.first(t), true, "")
	before, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	if _, err := h.s.db.Exec(`CREATE TRIGGER fail_capture BEFORE INSERT ON coding_task_artifacts WHEN NEW.kind='review_bundle' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.v.CaptureBundle(ctx, taskOwner, d.Ref); err == nil {
		t.Fatal("failure ignored")
	}
	after, _ := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("capture changed task on rollback")
	}
	var count int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM coding_task_artifact_blobs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("partial capture", count, err)
	}
	if _, err := h.s.db.Exec(`DROP TRIGGER fail_capture`); err != nil {
		t.Fatal(err)
	}
	a, b := h.capture(t, d)
	for _, q := range []string{`UPDATE coding_task_artifact_blobs SET document='{}'`, `DELETE FROM coding_task_artifact_blobs`} {
		if _, err := h.s.db.Exec(q); err == nil {
			t.Fatal("mutable blob")
		}
	}
	down, err := os.ReadFile("../../../docs/migrations/p0b2b-down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.db.Exec(string(down)); err == nil {
		t.Fatal("discarded B2-b data")
	}
	if _, err = h.s.db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	path := h.s.path
	h.s.Close()
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v := reopened.TaskRepository("server").(collab.ReviewRepository)
	got, err := v.ReadBundle(ctx, taskOwner, a.ArtifactID)
	if err != nil || !reflect.DeepEqual(got, b) {
		t.Fatal("restart lost immutable artifact", err)
	}
	same, err := v.CaptureBundle(ctx, taskOwner, d.Ref)
	if err != nil || same.ArtifactID != a.ArtifactID {
		t.Fatal("duplicate capture forked", same, err)
	}
}

func TestReviewCaptureRejectsLiveWriterAndNonterminalTurn(t *testing.T) {
	h := reviewFixture(t, 2)
	ctx := context.Background()
	d := h.first(t)
	if _, err := h.v.CaptureBundle(ctx, taskOwner, d.Ref); err == nil {
		t.Fatal("captured nonadmitted turn")
	}
	d = h.finish(t, d, true, "")
	release, err := h.s.AcquireTaskExecution(ctx, d.SessionID, h.root, d.RequestID, &d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.v.CaptureBundle(ctx, taskOwner, d.Ref); !errors.Is(err, collab.ErrRecoveryRequired) {
		t.Fatal("captured active writer", err)
	}
	release()
	h.capture(t, d)
}

func TestReviewObserverWaitsForInboxAndWriterDrain(t *testing.T) {
	h := reviewFixture(t, 1)
	ctx := context.Background()
	d := h.first(t)
	release, err := h.s.AcquireTaskExecution(ctx, d.SessionID, h.root, d.RequestID, &d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	in := session.TurnInput{SessionID: d.SessionID, RequestID: d.RequestID, TurnID: "draining", Text: d.Message, PayloadHash: "offline", ManagedTask: &d.Ref}
	if _, _, _, err = h.s.ReserveTurnInput(ctx, in, session.EventRecord{SessionID: d.SessionID, TurnID: in.TurnID, Kind: "turn_accepted", Payload: json.RawMessage(`{}`), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	taskSource(t, h.s, d.SessionID, in.TurnID, "turn_finished")
	for _, complete := range []bool{false, true} {
		if complete {
			if err = h.s.SetTurnInputState(ctx, d.SessionID, d.RequestID, session.TurnInputCompleted); err != nil {
				t.Fatal(err)
			}
		}
		o, err := h.v.ObserveTurn(ctx, taskOwner, d.Ref)
		if err != nil || o.Terminal || o.Status != "finalizing" {
			t.Fatal("normal drain froze or completed early", o, err)
		}
	}
	release()
	o, err := h.v.ObserveTurn(ctx, taskOwner, d.Ref)
	if err != nil || !o.Terminal || o.Status != "completed" {
		t.Fatal(o, err)
	}
}

func TestReviewSubmissionGateAndFinalizeRollbackWithEvent(t *testing.T) {
	for _, stage := range []string{"review", "gate", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			h := reviewFixture(t, 1)
			ctx := context.Background()
			d := h.finish(t, h.first(t), true, "")
			a, b := h.capture(t, d)
			input := h.review(t, a, b, "pass")
			var decision collab.Receipt
			var err error
			if stage != "review" {
				decision, err = h.v.SubmitReview(ctx, taskOwner, input)
				if err != nil {
					t.Fatal(err)
				}
			}
			if stage == "finalize" {
				if _, err = h.v.ApplyGate(ctx, taskOwner, h.run.RunID, decision.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			kind := map[string]string{"review": "review.recorded", "gate": "review.gate_consumed", "finalize": "task.accepted"}[stage]
			// emitTask inserts a placeholder and then updates the canonical record.
			if _, err = h.s.db.Exec(`CREATE TRIGGER fail_review_event BEFORE UPDATE ON coding_task_events WHEN json_extract(NEW.document,'$.kind')='` + kind + `' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "review":
				_, err = h.v.SubmitReview(ctx, taskOwner, input)
			case "gate":
				_, err = h.v.ApplyGate(ctx, taskOwner, h.run.RunID, decision.ID)
			case "finalize":
				_, err = h.v.FinalizeRun(ctx, taskOwner, h.run.RunID, decision.ID)
			}
			if err == nil {
				t.Fatal("event failure ignored")
			}
			after, err := h.r.Snapshot(ctx, taskOwner, h.run.TaskID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("partial state/event commit", err)
			}
			var n int
			if err = h.s.db.QueryRow(`SELECT COUNT(*) FROM coding_task_workspace_claims WHERE run_id=?`, h.run.RunID).Scan(&n); err != nil || n != 1 {
				t.Fatal("rollback lost claim", n, err)
			}
		})
	}
}

func TestReviewDoesNotMistakeOrdinaryOutputForTruncation(t *testing.T) {
	h := reviewFixture(t, 1)
	ctx := context.Background()
	d := h.finish(t, h.first(t), true, "TestTruncate and job_id regression: PASS")
	a, b := h.capture(t, d)
	if b.ValidationStatus != "evidence_complete" {
		t.Fatal(b.ValidationStatus)
	}
	receipt, err := h.v.SubmitReview(ctx, taskOwner, h.review(t, a, b, "pass"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.v.ApplyGate(ctx, taskOwner, h.run.RunID, receipt.ID); err != nil {
		t.Fatal(err)
	}
	decision, err := h.v.Decision(ctx, taskOwner, h.run.RunID, 1)
	if err != nil || decision.Action != "pass" {
		t.Fatal(decision, err)
	}
}
