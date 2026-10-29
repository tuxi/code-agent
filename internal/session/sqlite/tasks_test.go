package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

var taskOwner = collab.Principal{ID: "owner", LocalSessions: true}

func TestTaskCreateWithoutAnyAgentSession(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := s.TaskRepository("server")
	receipt, err := r.Create(context.Background(), taskOwner, collab.CreateInput{RequestID: "task-only", WorkspacePath: t.TempDir(), Goal: "agent-independent task"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.Snapshot(context.Background(), taskOwner, receipt.ID)
	if err != nil || len(snap.Participations) != 0 || len(snap.Attempts) != 0 {
		t.Fatalf("unexpected participation: %+v %v", snap, err)
	}
	for _, table := range []string{"sessions", "messages", "turn_inputs"} {
		var count int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("task created Agent context", table, err)
		}
	}
}

func taskFixture(t *testing.T) (*Store, collab.Repository, string) {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	root := t.TempDir()
	for _, id := range []string{"s1", "s2"} {
		if err = s.Save(context.Background(), &session.Session{ID: id, WorkspacePath: root, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return s, s.TaskRepository("server"), root
}
func taskCreate(t *testing.T, r collab.Repository, key string) string {
	t.Helper()
	receipt, err := r.Create(context.Background(), taskOwner, collab.CreateInput{RequestID: key, WorkspaceSessionID: "s1", Goal: "real goal", Acceptance: []string{"test passes"}})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}
func taskLocalPart(t *testing.T, r collab.Repository, id, sid, role string) string {
	t.Helper()
	identity, err := r.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := r.Participate(context.Background(), taskOwner, id, collab.ParticipationInput{RequestID: sid + role, Role: role, SessionRef: collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: sid}})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}
func taskAdmit(t *testing.T, s *Store, sid, request, turn string) {
	t.Helper()
	_, _, _, err := s.ReserveTurnInput(context.Background(), session.TurnInput{SessionID: sid, RequestID: request, TurnID: turn, PayloadHash: "hash", Text: "work"}, session.EventRecord{SessionID: sid, TurnID: turn, Kind: "turn_accepted", Payload: json.RawMessage(`{}`), At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
}
func taskSource(t *testing.T, s *Store, sid, turn, kind string) int64 {
	t.Helper()
	seq, err := s.RecordEvent(context.Background(), session.EventRecord{SessionID: sid, TurnID: turn, Kind: kind, Payload: json.RawMessage(`{}`), At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestTaskSessionReuseRolesAndReportedBoundary(t *testing.T) {
	_, r, _ := taskFixture(t)
	ctx := context.Background()
	one, two := taskCreate(t, r, "one"), taskCreate(t, r, "two")
	ref, err := r.RegisterExternal(ctx, taskOwner, collab.ExternalInput{RequestID: "register", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.RegisterExternal(ctx, taskOwner, collab.ExternalInput{RequestID: "register", RuntimeKind: "codex"})
	if err != nil || ref.SessionHandle != again.SessionHandle {
		t.Fatalf("registration retry: %+v %v", again, err)
	}
	for _, pair := range []struct{ task, role string }{{one, "planner"}, {one, "reviewer"}, {two, "reviewer"}} {
		p, err := r.Participate(ctx, taskOwner, pair.task, collab.ParticipationInput{RequestID: pair.role, Role: pair.role, SessionRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.LinkAttempt(ctx, taskOwner, pair.task, collab.AttemptInput{RequestID: "forge-turn", ParticipationID: p.ID, TurnID: "x", TurnRequestID: "x", Iteration: 1}); !errors.Is(err, collab.ErrForbidden) {
			t.Fatalf("reported session accepted local turn: %v", err)
		}
	}
	snap, err := r.Snapshot(ctx, taskOwner, one)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Participations) != 2 || snap.Participations[0].AgentID != snap.Participations[1].AgentID || snap.Participations[0].Role == snap.Participations[1].Role {
		t.Fatalf("roles=%+v", snap.Participations)
	}
	for _, p := range snap.Participations {
		if p.ObservationLevel != "reported" || p.SessionRef.SessionID != "" {
			t.Fatal(p)
		}
		for _, c := range p.Capabilities {
			if c == "link_attempt" || c == "cancel_turn" {
				t.Fatal("reported gained managed capability")
			}
		}
	}
	for _, id := range []string{one, two} {
		taskLocalPart(t, r, id, "s1", "executor")
	}
	if _, err = r.Snapshot(ctx, collab.Principal{ID: "other"}, one); !errors.Is(err, collab.ErrNotFound) {
		t.Fatalf("cross actor read: %v", err)
	}
	forged := ref
	forged.ServerID = "server"
	if _, err = r.Participate(ctx, taskOwner, one, collab.ParticipationInput{RequestID: "forged", Role: "executor", SessionRef: forged}); !errors.Is(err, collab.ErrForbidden) {
		t.Fatalf("forged ref: %v", err)
	}
}

func TestTaskExactTurnLateBindingAndReconnection(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	id, other := taskCreate(t, r, "one"), taskCreate(t, r, "two")
	part := taskLocalPart(t, r, id, "s1", "executor")
	otherPart := taskLocalPart(t, r, other, "s1", "executor")
	taskAdmit(t, s, "s1", "req1", "t1")
	taskAdmit(t, s, "s1", "req2", "t2")
	taskSource(t, s, "s1", "t2", "turn_failed")
	taskSource(t, s, "s1", "t1", "turn_started")
	terminal := taskSource(t, s, "s1", "t1", "turn_finished")
	in := collab.AttemptInput{RequestID: "bind", ParticipationID: part, TurnID: "t1", TurnRequestID: "req1", Iteration: 1}
	bound, err := r.LinkAttempt(ctx, taskOwner, id, in)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := r.LinkAttempt(ctx, taskOwner, id, in)
	if err != nil || retry != bound {
		t.Fatalf("idempotency=%+v %v", retry, err)
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Attempts) != 1 || snap.Attempts[0].Status != "completed" || snap.State != "waiting" || snap.TaskAcceptance != "not_reviewed" {
		t.Fatalf("snapshot=%+v", snap)
	}
	for _, change := range []struct{ task, part, req, turn, key string }{{other, otherPart, "req1", "t1", "duplicate"}, {id, part, "req2", "t1", "mismatch"}, {id, part, "absent", "absent", "missing"}} {
		_, err = r.LinkAttempt(ctx, taskOwner, change.task, collab.AttemptInput{RequestID: change.key, ParticipationID: change.part, TurnID: change.turn, TurnRequestID: change.req, Iteration: 1})
		if err == nil {
			t.Fatal("invalid turn binding accepted")
		}
	}
	page, err := r.Events(ctx, taskOwner, id, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		for _, e := range page.Events {
			if seen[e.EventID] {
				t.Fatal("duplicate event")
			}
			seen[e.EventID] = true
			if e.Source.TurnID == "t2" {
				t.Fatal("other turn projected")
			}
			if e.Kind == "attempt.completed" && e.Source.Seq != strconv.FormatInt(terminal, 10) {
				t.Fatal("wrong terminal provenance")
			}
		}
		if !page.HasMore {
			break
		}
		cursor, _ := strconv.ParseInt(page.NextCursor, 10, 64)
		page, err = r.Events(ctx, taskOwner, id, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if page.NextCursor != snap.SnapshotTaskSeq {
		t.Fatal("snapshot and event watermarks differ")
	}
	// Reopen a second connection: only persisted association/checkpoints matter.
	s2, err := New(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	r2 := s2.TaskRepository("server")
	cursor, _ := strconv.ParseInt(page.NextCursor, 10, 64)
	empty, err := r2.Events(ctx, taskOwner, id, cursor, 100)
	if err != nil || len(empty.Events) != 0 || empty.NextCursor != page.NextCursor {
		t.Fatalf("reconnect=%+v %v", empty, err)
	}
	i1, _ := r.Identity(ctx)
	i2, _ := r2.Identity(ctx)
	if i1 != i2 {
		t.Fatal("store identity changed")
	}
}

func TestTaskWorkspaceScopeAndNoSingleAgentMutation(t *testing.T) {
	s, r, root := taskFixture(t)
	ctx := context.Background()
	id := taskCreate(t, r, "create")
	if _, err := r.Create(ctx, taskOwner, collab.CreateInput{RequestID: "create", WorkspaceSessionID: "s1", Goal: "changed"}); !errors.Is(err, collab.ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Save(ctx, &session.Session{ID: "outside", WorkspacePath: filepath.Join(root, "other")}); err != nil {
		t.Fatal(err)
	}
	i, _ := r.Identity(ctx)
	_, err := r.Participate(ctx, taskOwner, id, collab.ParticipationInput{RequestID: "outside", Role: "executor", SessionRef: collab.SessionRef{Kind: "local", ServerID: i.ServerID, StoreID: i.StoreID, SessionID: "outside"}})
	if !errors.Is(err, collab.ErrForbidden) {
		t.Fatalf("outside scope: %v", err)
	}
	var turns, messages int
	s.db.QueryRow(`SELECT COUNT(*) FROM turn_inputs`).Scan(&turns)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages)
	if turns != 0 || messages != 0 {
		t.Fatal("Task metadata started execution or created history")
	}
	metas, err := s.List(ctx)
	if err != nil || len(metas) != 3 {
		t.Fatalf("task appeared in Session list: %d %v", len(metas), err)
	}
}

func TestTaskTransactionRollbackAndProjectionCheckpoint(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	fault := func() {
		t.Helper()
		if _, err := s.db.Exec(`CREATE TRIGGER b1_fail BEFORE INSERT ON coding_task_events BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
			t.Fatal(err)
		}
	}
	repair := func() {
		t.Helper()
		if _, err := s.db.Exec(`DROP TRIGGER b1_fail`); err != nil {
			t.Fatal(err)
		}
	}
	fault()
	_, err := r.Create(ctx, taskOwner, collab.CreateInput{RequestID: "fault", WorkspaceSessionID: "s1", Goal: "goal"})
	if err == nil {
		t.Fatal("injected fault ignored")
	}
	for _, table := range []string{"coding_tasks", "coding_task_events", "coding_task_requests"} {
		var count int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial commit in %s", table)
		}
	}
	repair()
	id := taskCreate(t, r, "fault")
	part := taskLocalPart(t, r, id, "s1", "executor")
	taskAdmit(t, s, "s1", "req", "turn")
	if _, err = r.LinkAttempt(ctx, taskOwner, id, collab.AttemptInput{RequestID: "bind", ParticipationID: part, TurnID: "turn", TurnRequestID: "req", Iteration: 1}); err != nil {
		t.Fatal(err)
	}
	before, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	taskSource(t, s, "s1", "turn", "turn_finished")
	fault()
	if _, err = r.Snapshot(ctx, taskOwner, id); err == nil {
		t.Fatal("projection fault ignored")
	}
	var checkpoint int64
	s.db.QueryRow(`SELECT checkpoint FROM coding_task_attempts WHERE task_id=?`, id).Scan(&checkpoint)
	if checkpoint != 0 {
		t.Fatal("checkpoint advanced without event")
	}
	repair()
	after, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil || after.Revision == before.Revision || after.Attempts[0].Status != "completed" {
		t.Fatalf("projection retry=%+v %v", after, err)
	}
}

func TestTaskReportsArtifactsAndNoReviewGate(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	id := taskCreate(t, r, "create")
	executor := taskLocalPart(t, r, id, "s1", "executor")
	ref, err := r.RegisterExternal(ctx, taskOwner, collab.ExternalInput{RequestID: "register", RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	planner, err := r.Participate(ctx, taskOwner, id, collab.ParticipationInput{RequestID: "planner", Role: "planner", SessionRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := r.Participate(ctx, taskOwner, id, collab.ParticipationInput{RequestID: "reviewer", Role: "reviewer", SessionRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	plan := collab.ReportInput{RequestID: "plan", ParticipationID: planner.ID, SubmissionID: "plan-one", Iteration: 1, Content: "small plan"}
	p, err := r.Report(ctx, taskOwner, id, "plan", plan)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Report(ctx, taskOwner, id, "plan", plan)
	if err != nil || p != again {
		t.Fatal("report retry duplicated")
	}
	taskAdmit(t, s, "s1", "req", "turn")
	attempt, err := r.LinkAttempt(ctx, taskOwner, id, collab.AttemptInput{RequestID: "bind", ParticipationID: executor, TurnID: "turn", TurnRequestID: "req", Iteration: 1})
	if err != nil {
		t.Fatal(err)
	}
	seq := taskSource(t, s, "s1", "turn", "tool_finished")
	patch, err := r.LinkArtifact(ctx, taskOwner, id, collab.ArtifactInput{RequestID: "patch", AttemptID: attempt.ID, Kind: "patch", SourceSeq: strconv.FormatInt(seq, 10)})
	if err != nil {
		t.Fatal(err)
	}
	report := collab.ReportInput{RequestID: "review", ParticipationID: reviewer.ID, SubmissionID: "review-one", Iteration: 1, Content: "looks good", Verdict: "pass", ReviewedArtifactID: patch.ID}
	result, err := r.Report(ctx, taskOwner, id, "review", report)
	if err != nil || result.Status != "recorded" {
		t.Fatalf("review=%+v %v", result, err)
	}
	report.RequestID = "wrong-role"
	report.ParticipationID = planner.ID
	if _, err = r.Report(ctx, taskOwner, id, "review", report); !errors.Is(err, collab.ErrForbidden) {
		t.Fatal("wrong role allowed review")
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ArtifactRefs) != 3 || snap.TaskAcceptance != "not_reviewed" || snap.State == "completed" || snap.WorkflowRef != nil {
		t.Fatalf("reported pass became task completion: %+v", snap)
	}
	if snap.State != "active" || snap.Phase != "implementing" {
		t.Fatal("reported review hid a queued implementation")
	}
}

func TestTaskSessionDeleteKeepsSingleAgentBehavior(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	id := taskCreate(t, r, "task")
	part := taskLocalPart(t, r, id, "s1", "executor")
	taskAdmit(t, s, "s1", "req", "turn")
	if _, err := r.LinkAttempt(ctx, taskOwner, id, collab.AttemptInput{RequestID: "bind", ParticipationID: part, TurnID: "turn", TurnRequestID: "req", Iteration: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatal("Task blocked ordinary Session deletion", err)
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil || snap.Attempts[0].Status != "source_unavailable" || snap.WaitReason != "source_unavailable" {
		t.Fatalf("deleted source misrepresented: %+v %v", snap, err)
	}
	again, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil || again.Revision != snap.Revision {
		t.Fatal("source removal projected more than once")
	}
}

func TestTaskConcurrentIdempotencyAndConsistentSnapshots(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	second, err := New(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	r2 := second.TaskRepository("server")
	var wg sync.WaitGroup
	receipts := make(chan collab.Receipt, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := r
			if i%2 == 1 {
				repo = r2
			}
			v, e := repo.Create(ctx, taskOwner, collab.CreateInput{RequestID: "same", WorkspaceSessionID: "s1", Goal: "goal"})
			if e != nil {
				errs <- e
			} else {
				receipts <- v
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(receipts)
	var id string
	for v := range receipts {
		if id != "" && v.ID != id {
			t.Fatal("duplicate task")
		}
		id = v.ID
	}
	snap, err := r.Snapshot(ctx, taskOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	page, err := r2.Events(ctx, taskOwner, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.NextCursor != snap.SnapshotTaskSeq || page.Events[0].TaskRevision != snap.Revision {
		t.Fatal("inconsistent state/events")
	}
}

func TestTaskConcurrentTurnOwnership(t *testing.T) {
	s, r, _ := taskFixture(t)
	ctx := context.Background()
	other, err := New(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	r2 := other.TaskRepository("server")
	one, two := taskCreate(t, r, "one"), taskCreate(t, r, "two")
	p1, p2 := taskLocalPart(t, r, one, "s1", "executor"), taskLocalPart(t, r, two, "s1", "executor")
	taskAdmit(t, s, "s1", "request", "turn")
	results := make(chan error, 2)
	go func() {
		_, err := r.LinkAttempt(ctx, taskOwner, one, collab.AttemptInput{RequestID: "bind", ParticipationID: p1, TurnID: "turn", TurnRequestID: "request", Iteration: 1})
		results <- err
	}()
	go func() {
		_, err := r2.LinkAttempt(ctx, taskOwner, two, collab.AttemptInput{RequestID: "bind", ParticipationID: p2, TurnID: "turn", TurnRequestID: "request", Iteration: 1})
		results <- err
	}()
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, collab.ErrConflict)) || (second == nil && errors.Is(first, collab.ErrConflict))) {
		t.Fatalf("ownership outcomes %v / %v", first, second)
	}
}

func TestTaskSnapshotsRemainConsistentDuringReports(t *testing.T) {
	_, r, _ := taskFixture(t)
	ctx := context.Background()
	id := taskCreate(t, r, "task")
	ref, err := r.RegisterExternal(ctx, taskOwner, collab.ExternalInput{RequestID: "external", RuntimeKind: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	part, err := r.Participate(ctx, taskOwner, id, collab.ParticipationInput{RequestID: "planner", Role: "planner", SessionRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for n := 0; n < 15; n++ {
			key := strconv.Itoa(n)
			if _, err := r.Report(ctx, taskOwner, id, "plan", collab.ReportInput{RequestID: key, SubmissionID: key, ParticipationID: part.ID, Iteration: 1, Content: "plan"}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for n := 0; n < 20; n++ {
		snap, err := r.Snapshot(ctx, taskOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		rev, _ := strconv.Atoi(snap.Revision)
		if len(snap.ArtifactRefs) != rev-2 || len(snap.Attempts) != rev-2 || len(snap.Participations) != 1 {
			t.Fatalf("partial snapshot revision=%d artifacts=%d attempts=%d", rev, len(snap.ArtifactRefs), len(snap.Attempts))
		}
		page, err := r.Events(ctx, taskOwner, id, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, event := range page.Events {
			if event.TaskSeq == snap.SnapshotTaskSeq {
				matched = true
				if event.TaskRevision != snap.Revision {
					t.Fatal("cursor/revision disagreement")
				}
			}
		}
		if !matched {
			t.Fatal("snapshot watermark has no durable event")
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTaskAdditiveMigrationReadOnlyAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO sessions(id,model,summary,prompt_tokens,context_window,compact_threshold,workspace_path,metadata,created_at,updated_at) VALUES('legacy','m','',0,32000,24000,'/tmp','{}','','')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(context.Background(), "legacy"); err != nil {
		t.Fatal("migration lost Session", err)
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM coding_tasks`).Scan(&count)
	if count != 0 {
		t.Fatal("migration created tasks for old sessions")
	}
	identity, err := s.TaskRepository("server").Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, err := s.TaskRepository("server").Identity(context.Background())
	if err != nil || identity != again {
		t.Fatal("migration not idempotent")
	}
	ro, err := NewReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if ro.TaskRepository("server") != nil {
		t.Fatal("read-only DB advertised mutating projection")
	}
	ro.Close()
	down, err := os.ReadFile("../../../docs/migrations/p0b1-down.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Destructive cleanup is tested only against this disposable database.
	if _, err = s.db.Exec(string(down)); err != nil {
		t.Fatal(err)
	}
	ro, err = NewReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.Load(context.Background(), "legacy"); err != nil {
		t.Fatal("rollback changed Session data", err)
	}
}
