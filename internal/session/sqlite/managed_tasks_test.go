package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

func managedFixture(t *testing.T) (*Store, collab.Repository, collab.ManagedRepository, string, string, collab.PrepareRunInput) {
	t.Helper()
	s, r, root := taskFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "-c", "init.defaultBranch=main", "init", "--template=", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	id := taskCreate(t, r, "managed-task")
	executor := taskLocalPart(t, r, id, "s1", "executor")
	ext, err := r.RegisterExternal(ctx, taskOwner, collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, role := range []string{"planner", "reviewer"} {
		receipt, err := r.Participate(ctx, taskOwner, id, collab.ParticipationInput{RequestID: role, Role: role, SessionRef: ext})
		if err != nil {
			t.Fatal(err)
		}
		parts[role] = receipt.ID
	}
	plan, err := r.Report(ctx, taskOwner, id, "plan", collab.ReportInput{RequestID: "plan", ParticipationID: parts["planner"], SubmissionID: "plan-submission", Iteration: 1, Content: "offline implementation plan"})
	if err != nil {
		t.Fatal(err)
	}
	input := collab.PrepareRunInput{RequestID: "prepare", PlanArtifactID: plan.ID, ExecutorParticipationID: executor, ReviewerParticipationID: parts["reviewer"], MaxIterations: 2, Workflow: collab.WorkflowBinding{Name: "offline_probe", VersionID: "1", Hash: strings.Repeat("a", 64)}}
	return s, r, r.(collab.ManagedRepository), root, id, input
}

func preparedRun(t *testing.T, r collab.ManagedRepository, id string, in collab.PrepareRunInput) collab.ManagedRun {
	t.Helper()
	receipt, err := r.PrepareRun(context.Background(), taskOwner, id, in)
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Run(context.Background(), taskOwner, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func submittedDispatch(t *testing.T, r collab.ManagedRepository, run collab.ManagedRun) collab.ManagedDispatch {
	t.Helper()
	ctx := context.Background()
	commands, err := r.PendingCommands(ctx, taskOwner, 20)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands=%+v %v", commands, err)
	}
	if err = r.AcknowledgeSubmission(ctx, taskOwner, collab.SubmissionReceipt{StoreID: run.StoreID, CommandID: run.CommandID, PayloadHash: commands[0].PayloadHash, Workflow: run.Workflow, FluxTaskID: "123"}); err != nil {
		t.Fatal(err)
	}
	input := collab.ReserveDispatchInput{RequestID: "reserve", Epoch: run.Epoch, Iteration: 1, Message: "implement the approved plan"}
	receipt, err := r.ReserveDispatch(ctx, taskOwner, run.RunID, input)
	if err != nil {
		t.Fatal(err)
	}
	ref := collab.ManagedTurnRef{ServerID: run.ServerID, StoreID: run.StoreID, TaskID: run.TaskID, RunID: run.RunID, Epoch: run.Epoch, DispatchID: receipt.ID}
	d, err := r.Dispatch(ctx, taskOwner, ref)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestManagedRunCommandAtomicAndIdempotent(t *testing.T) {
	s, r, m, _, id, in := managedFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_command BEFORE INSERT ON coding_task_commands BEGIN SELECT RAISE(ABORT,'injected command failure'); END`); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Snapshot(ctx, taskOwner, id)
	if _, err := m.PrepareRun(ctx, taskOwner, id, in); err == nil {
		t.Fatal("injected failure was ignored")
	}
	after, _ := r.Snapshot(ctx, taskOwner, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed run creation changed snapshot")
	}
	for _, table := range []string{"coding_task_runs", "coding_task_workspace_claims", "coding_task_commands"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s %d %v", table, n, err)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_command`); err != nil {
		t.Fatal(err)
	}
	run := preparedRun(t, m, id, in)
	first, err := m.PrepareRun(ctx, taskOwner, id, in)
	if err != nil || first.ID != run.RunID {
		t.Fatalf("duplicate run %+v %v", first, err)
	}
	in.MaxIterations = 3
	if _, err = m.PrepareRun(ctx, taskOwner, id, in); !errors.Is(err, collab.ErrConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	commands, err := m.PendingCommands(ctx, taskOwner, 20)
	if err != nil || len(commands) != 1 {
		t.Fatal(commands, err)
	}
	receipt := collab.SubmissionReceipt{StoreID: run.StoreID, CommandID: run.CommandID, PayloadHash: commands[0].PayloadHash, Workflow: run.Workflow, FluxTaskID: "123"}
	bad := receipt
	bad.PayloadHash = "wrong"
	if err = m.AcknowledgeSubmission(ctx, taskOwner, bad); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
	if err = m.AcknowledgeSubmission(ctx, taskOwner, receipt); err != nil {
		t.Fatal(err)
	}
	one, _ := r.Snapshot(ctx, taskOwner, id)
	if err = m.AcknowledgeSubmission(ctx, taskOwner, receipt); err != nil {
		t.Fatal(err)
	}
	two, _ := r.Snapshot(ctx, taskOwner, id)
	if one.Revision != two.Revision || one.ManagedRun.FluxTaskID != "123" {
		t.Fatal("duplicate receipt advanced task")
	}
	if pending, err := m.PendingCommands(ctx, taskOwner, 20); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if _, err = m.Run(ctx, collab.Principal{ID: "other", LocalSessions: true}, run.RunID); !errors.Is(err, collab.ErrNotFound) {
		t.Fatal("owner escape", err)
	}
}

func TestManagedAdmissionRollbackAndExactTurnBinding(t *testing.T) {
	s, r, m, root, id, in := managedFixture(t)
	ctx := context.Background()
	run := preparedRun(t, m, id, in)
	d := submittedDispatch(t, m, run)
	if d.TurnID != "" || d.AttemptID != "" {
		t.Fatal("reservation fabricated admission")
	}
	lease, err := s.AcquireTaskExecution(ctx, d.SessionID, root, d.RequestID, &d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer lease()
	input := session.TurnInput{SessionID: d.SessionID, RequestID: d.RequestID, TurnID: "exact-turn", PayloadHash: "hash", Text: d.Message, ManagedTask: &d.Ref}
	event := session.EventRecord{SessionID: d.SessionID, TurnID: input.TurnID, Kind: "turn_accepted", At: time.Now(), Payload: json.RawMessage(`{}`)}
	badEvent := event
	badEvent.TurnID = "unrelated-turn"
	if _, _, _, err = s.ReserveTurnInput(ctx, input, badEvent); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("accepted a mismatched event", err)
	}
	before, _ := r.Snapshot(ctx, taskOwner, id)
	if _, err = s.db.Exec(`CREATE TRIGGER fail_attempt BEFORE INSERT ON coding_task_attempts BEGIN SELECT RAISE(ABORT,'injected attempt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.ReserveTurnInput(ctx, input, event); err == nil {
		t.Fatal("admission failure ignored")
	}
	after, _ := r.Snapshot(ctx, taskOwner, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial Task admission")
	}
	for _, table := range []string{"turn_inputs", "session_events"} {
		var n int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s %d %v", table, n, err)
		}
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_attempt`); err != nil {
		t.Fatal(err)
	}
	stored, created, seq, err := s.ReserveTurnInput(ctx, input, event)
	if err != nil || !created || seq <= 0 {
		t.Fatal(stored, created, seq, err)
	}
	input.TurnID = "second-proposed-turn"
	event.TurnID = input.TurnID
	again, created, _, err := s.ReserveTurnInput(ctx, input, event)
	if err != nil || created || again.TurnID != stored.TurnID || again.AcceptedSeq != seq {
		t.Fatal("duplicate turn", again, created, err)
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.CurrentAttemptID == "" || len(snap.Attempts) != 2 {
		t.Fatalf("attempts %+v", snap.Attempts)
	}
	bound, err := m.Dispatch(ctx, taskOwner, d.Ref)
	if err != nil || bound.TurnID != stored.TurnID || bound.AttemptID != snap.CurrentAttemptID {
		t.Fatal(bound, err)
	}
	other := taskCreate(t, r, "other-task")
	part := taskLocalPart(t, r, other, "s1", "executor")
	if _, err = r.LinkAttempt(ctx, taskOwner, other, collab.AttemptInput{RequestID: "steal", ParticipationID: part, TurnRequestID: d.RequestID, TurnID: stored.TurnID, Iteration: 1}); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("Turn reassigned", err)
	}
	taskSource(t, s, "s1", stored.TurnID, "turn_finished")
	finished, _ := r.Snapshot(ctx, taskOwner, id)
	if finished.State != snap.State || finished.Phase != snap.Phase || finished.TaskAcceptance != "not_reviewed" {
		t.Fatal("B1 projection advanced managed phase", finished.CodingTask)
	}
}

func TestManagedWorkspaceFencesOrdinaryNestedAndStaleSources(t *testing.T) {
	s, _, m, root, id, in := managedFixture(t)
	ctx := context.Background()
	run := preparedRun(t, m, id, in)
	d := submittedDispatch(t, m, run)
	for _, path := range []string{root, filepath.Join(root, "child"), filepath.Dir(root)} {
		if release, err := s.AcquireTaskExecution(ctx, "s2", path, "ordinary", nil); !errors.Is(err, collab.ErrManagedWorkspace) {
			if release != nil {
				release()
			}
			t.Fatalf("unfenced path %s: %v", path, err)
		}
	}
	if release, err := s.AcquireTaskExecution(ctx, d.SessionID, t.TempDir(), "rebound-plain", nil); !errors.Is(err, collab.ErrManagedWorkspace) {
		if release != nil {
			release()
		}
		t.Fatal("bound executor escaped by changing its root", err)
	}
	bad := d.Ref
	bad.Epoch = "2"
	if _, err := s.AcquireTaskExecution(ctx, d.SessionID, root, d.RequestID, &bad); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("stale epoch", err)
	}
	if _, err := m.ReserveDispatch(ctx, taskOwner, run.RunID, collab.ReserveDispatchInput{RequestID: "repair", Epoch: "1", Iteration: 2, Message: "repair"}); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatal("repair bypassed gate", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, ".git"), filepath.Join(root, ".git-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireTaskExecution(ctx, d.SessionID, root, d.RequestID, &d.Ref); !errors.Is(err, collab.ErrManagedWorkspace) {
		t.Fatal("replaced worktree accepted", err)
	}
}

func TestManagedRunDoesNotStealOrdinaryInFlightCheckout(t *testing.T) {
	s, _, m, root, id, in := managedFixture(t)
	ctx := context.Background()
	first, err := s.AcquireTaskExecution(ctx, "s1", root, "plain-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AcquireTaskExecution(ctx, "s2", root, "plain-2", nil)
	if err != nil {
		t.Fatal("ordinary concurrency changed", err)
	}
	if _, err = m.PrepareRun(ctx, taskOwner, id, in); !errors.Is(err, collab.ErrManagedWorkspace) {
		t.Fatal("claimed in-flight checkout", err)
	}
	first()
	second()
	taskAdmit(t, s, "s1", "old-inbox", "old-turn")
	if _, err = m.PrepareRun(ctx, taskOwner, id, in); !errors.Is(err, collab.ErrManagedWorkspace) {
		t.Fatal("ignored durable pending inbox", err)
	}
	if err = s.SetTurnInputState(ctx, "s1", "old-inbox", session.TurnInputCompleted); err != nil {
		t.Fatal(err)
	}
	preparedRun(t, m, id, in)
}

func TestManagedConcurrentRunClaimsOneWorkspace(t *testing.T) {
	_, r, m, _, id, in := managedFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan collab.Receipt, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := m.PrepareRun(ctx, taskOwner, id, in)
			results <- receipt
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	runID := ""
	for receipt := range results {
		if runID != "" && runID != receipt.ID {
			t.Fatal("duplicate run")
		}
		runID = receipt.ID
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil || snap.ManagedRun == nil || snap.ManagedRun.RunID != runID {
		t.Fatal(snap, err)
	}
}

func TestManagedMigrationAndGuardedDowngrade(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	id := taskCreate(t, r, "b1-task")
	taskAdmit(t, s, "s1", "plain", "plain-turn")
	down, err := os.ReadFile("../../../docs/migrations/p0b2a-down.sql")
	if err != nil {
		t.Fatal(err)
	}
	b2bdown, err := os.ReadFile("../../../docs/migrations/p0b2b-down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(string(b2bdown)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(string(down)); err != nil {
		t.Fatal(err)
	}
	ro, err := NewReadOnly(s.path)
	if err != nil {
		t.Fatal(err)
	}
	input, err := ro.TurnInput(ctx, "s1", "plain")
	if err != nil || input.TurnID != "plain-turn" || input.ManagedTask != nil {
		t.Fatal("schema-1 read compatibility", input, err)
	}
	ro.Close()
	path := s.path
	s.Close()
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version string
	if err = reopened.db.QueryRow(`SELECT value FROM coding_task_meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "3" {
		t.Fatal(version, err)
	}
	if snap, err := reopened.TaskRepository("server").Snapshot(ctx, taskOwner, id); err != nil || snap.ManagedRun != nil || snap.TaskID != id {
		t.Fatal("migration lost B1 task", snap, err)
	}
	active, _, m, _, task, in := managedFixture(t)
	preparedRun(t, m, task, in)
	if _, err = active.db.Exec(string(down)); err == nil {
		t.Fatal("downgrade discarded an active Run")
	}
	if _, err = active.db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = active.db.QueryRow(`SELECT COUNT(*) FROM coding_task_runs`).Scan(&n); err != nil || n != 1 {
		t.Fatal("failed downgrade altered data", n, err)
	}
}

func TestManagedRunRejectsReportedExecutorAndWrongRole(t *testing.T) {
	_, _, m, _, id, in := managedFixture(t)
	ctx := context.Background()
	bad := in
	bad.ExecutorParticipationID = in.ReviewerParticipationID
	if _, err := m.PrepareRun(ctx, taskOwner, id, bad); !errors.Is(err, collab.ErrForbidden) {
		t.Fatal("reported reviewer gained execution", err)
	}
	bad = in
	bad.ReviewerParticipationID = in.ExecutorParticipationID
	if _, err := m.PrepareRun(ctx, taskOwner, id, bad); !errors.Is(err, collab.ErrForbidden) {
		t.Fatal("executor gained review role", err)
	}
	if _, err := m.PrepareRun(ctx, collab.Principal{ID: taskOwner.ID}, id, in); !errors.Is(err, collab.ErrForbidden) {
		t.Fatal("reported principal gained local authority", err)
	}
	preparedRun(t, m, id, in)
}
