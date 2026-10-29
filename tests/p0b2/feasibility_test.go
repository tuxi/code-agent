// Package p0b2_test probes pinned dependencies, not a production collaboration service.
// All executions use temporary databases and in-process tools; no model is called.
package p0b2_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"

	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	flux "github.com/tuxi/flux-workflow/runtime"
	"github.com/tuxi/flux-workflow/tool"
	"gorm.io/gorm"
)

func openFlux(t *testing.T, path string) *flux.Runtime {
	t.Helper()
	r, err := flux.NewLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	return r
}

func simpleDefinition() *definition.WorkflowDefinition {
	return &definition.WorkflowDefinition{Name: "b2_probe", Nodes: []definition.NodeDefinition{
		{Name: "start", Type: definition.NodeStart}, {Name: "end", Type: definition.NodeEnd},
	}, Edges: []definition.EdgeDefinition{{From: "start", To: "end", Type: definition.EdgeNormal}}}
}

func TestSubmitHasNoBusinessDeduplication(t *testing.T) {
	r := openFlux(t, filepath.Join(t.TempDir(), "flux.db"))
	ctx := context.Background()
	if err := r.RegisterWorkflow(ctx, simpleDefinition()); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"command_id": "same-command"}
	a, err := r.Submit(ctx, "b2_probe", input)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Submit(ctx, "b2_probe", input)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("probe changed: Submit now deduplicates business input")
	}
	for _, id := range []int64{a, b} {
		if task, err := r.Status(ctx, id); err != nil || task.Status != domain.TaskPending {
			t.Fatalf("task %d: %+v %v", id, task, err)
		}
	}
}

func waitStatus(t *testing.T, r *flux.Runtime, id int64, status domain.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err := r.Status(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == status {
			return
		}
		if task.Status == domain.TaskFailed {
			t.Fatalf("Flux failed: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	task, _ := r.Status(context.Background(), id)
	t.Fatalf("expected %s, got %+v", status, task)
}

func startWorker(t *testing.T, r *flux.Runtime) {
	t.Helper()
	if err := r.Start(context.Background(), flux.WithTaskWorkers(1), flux.WithAsyncWorkers(0),
		flux.WithAwaitPollWorkers(0), flux.WithRecoveryScanner(false)); err != nil {
		t.Fatal(err)
	}
}

func TestRestartNeedsExplicitPendingTaskRedelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flux.db")
	r := openFlux(t, path)
	ctx := context.Background()
	if err := r.RegisterWorkflow(ctx, simpleDefinition()); err != nil {
		t.Fatal(err)
	}
	id, err := r.Submit(ctx, "b2_probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Shutdown(); err != nil {
		t.Fatal(err)
	}
	r = openFlux(t, path)
	startWorker(t, r)
	// No recovery scanner: a persisted pending row is not a new queue entry.
	time.Sleep(100 * time.Millisecond)
	if task, err := r.Status(ctx, id); err != nil || task.Status != domain.TaskPending {
		t.Fatalf("pending persistence probe: %+v %v", task, err)
	}
	if err = r.Engine().TaskRepo().Enqueue(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, id, domain.TaskSuccess)
}

type recordingTool struct {
	mu    sync.Mutex
	calls []string
}

func (*recordingTool) Name() string        { return "record_stage" }
func (*recordingTool) Description() string { return "offline stage recorder" }
func (*recordingTool) InputSchema() tool.DataSchema {
	return tool.DataSchema{Fields: map[string]tool.FieldSchema{"stage": {Type: "string", Required: true}}}
}
func (*recordingTool) OutputSchema() tool.DataSchema { return tool.DataSchema{} }
func (*recordingTool) Mode() tool.ExecutionMode      { return tool.SyncExecution }
func (s *recordingTool) Execute(_ context.Context, in map[string]any, _ tool.ToolEmitter) (*tool.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, in["stage"].(string))
	return tool.Success(map[string]any{}), nil
}
func (s *recordingTool) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func reviewDefinition() *definition.WorkflowDefinition {
	d := &definition.WorkflowDefinition{Name: "b2_two_round_probe", Nodes: []definition.NodeDefinition{{Name: "start", Type: definition.NodeStart}}}
	for _, stage := range []string{"implement1", "implement2", "accepted1", "accepted2", "attention1", "attention2"} {
		d.Nodes = append(d.Nodes, definition.NodeDefinition{Name: stage, Type: definition.NodeTool,
			Config: map[string]any{"tool": "record_stage"}, InputMapping: map[string]string{"stage": "'" + stage + "'"}})
	}
	for _, name := range []string{"review1", "review2"} {
		d.Nodes = append(d.Nodes, definition.NodeDefinition{Name: name, Type: definition.NodeAwait,
			Config: map[string]any{"await_type": "external", "source": "webhook_or_poll", "provider": "review_provider",
				"correlation": map[string]any{"task_id": "task_id", "attempt_id": "attempt_id"}},
			InputMapping: map[string]string{"review_provider": "'coding_task_review_v1'", "task_id": "input.task_id", "attempt_id": "'" + name + "-attempt'"}})
	}
	d.Nodes = append(d.Nodes, definition.NodeDefinition{Name: "end", Type: definition.NodeEnd})
	d.Edges = []definition.EdgeDefinition{
		{From: "start", To: "implement1", Type: definition.EdgeNormal},
		{From: "implement1", To: "review1", Type: definition.EdgeNormal},
		{From: "review1", To: "accepted1", Type: definition.EdgeCondition, Condition: "review1.verdict == 'pass'"},
		{From: "review1", To: "implement2", Type: definition.EdgeCondition, Condition: "review1.verdict == 'request_changes'"},
		{From: "review1", To: "attention1", Type: definition.EdgeCondition, Condition: "review1.verdict != 'pass' && review1.verdict != 'request_changes'"},
		{From: "implement2", To: "review2", Type: definition.EdgeNormal},
		{From: "review2", To: "accepted2", Type: definition.EdgeCondition, Condition: "review2.verdict == 'pass'"},
		{From: "review2", To: "attention2", Type: definition.EdgeCondition, Condition: "review2.verdict != 'pass'"},
	}
	for _, name := range []string{"accepted1", "accepted2", "attention1", "attention2"} {
		d.Edges = append(d.Edges, definition.EdgeDefinition{From: name, To: "end", Type: definition.EdgeNormal})
	}
	return d
}

func TestTypedReviewAwaitFiniteBranchesAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name           string
		verdicts, want []string
	}{
		{"pass", []string{"pass"}, []string{"implement1", "accepted1"}},
		{"repair", []string{"request_changes", "pass"}, []string{"implement1", "implement2", "accepted2"}},
		{"exhausted", []string{"request_changes", "request_changes"}, []string{"implement1", "implement2", "attention2"}},
		{"inconclusive", []string{"inconclusive"}, []string{"implement1", "attention1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "flux.db")
			r := openFlux(t, path)
			recorder := &recordingTool{}
			r.ToolRegistry().Register(recorder)
			ctx := context.Background()
			def := reviewDefinition()
			if err := r.RegisterWorkflow(ctx, def); err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(ctx, def, map[string]any{"task_id": "coding-task"})
			if err != nil || result.Err != nil || result.Status != "suspended" {
				t.Fatalf("first await: %+v %v", result, err)
			}
			id := result.TaskID
			if err = r.Shutdown(); err != nil {
				t.Fatal(err)
			}
			r = openFlux(t, path)
			r.ToolRegistry().Register(recorder)
			startWorker(t, r)
			for n, verdict := range tc.verdicts {
				name := []string{"review1", "review2"}[n]
				waitStatus(t, r, id, domain.TaskSuspended)
				binding, err := r.AwaitBinding(ctx, id, name)
				if err != nil || binding == nil || binding.Provider == nil || *binding.Provider != "coding_task_review_v1" || binding.Correlation["task_id"] != "coding-task" || binding.Correlation["attempt_id"] != name+"-attempt" {
					t.Fatalf("typed binding: %+v %v", binding, err)
				}
				// This proves Flux routing only. A production ReviewGate must validate
				// authorization, evidence and epochs before delivering this payload.
				completion, err := r.CompleteAwait(ctx, id, name, map[string]any{"verdict": verdict}, "")
				if err != nil || completion.Err != nil {
					t.Fatalf("complete: %+v %v", completion, err)
				}
			}
			waitStatus(t, r, id, domain.TaskSuccess)
			before := recorder.snapshot()
			if !reflect.DeepEqual(before, tc.want) {
				t.Fatalf("calls=%v want=%v", before, tc.want)
			}
			duplicate, err := r.CompleteAwait(ctx, id, "review1", map[string]any{"verdict": tc.verdicts[0]}, "")
			if err != nil || duplicate.Err != nil {
				t.Fatalf("duplicate: %+v %v", duplicate, err)
			}
			if !reflect.DeepEqual(recorder.snapshot(), before) {
				t.Fatal("duplicate completion replayed work")
			}
		})
	}
}

func TestTurnReservationKeepsExactIdentityAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "session.db")
	s, err := sessionsqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, &session.Session{ID: "executor"}); err != nil {
		t.Fatal(err)
	}
	in := session.TurnInput{SessionID: "executor", RequestID: "stable-dispatch", TurnID: "original-turn", PayloadHash: "original-hash", Text: "offline probe"}
	event := session.EventRecord{SessionID: in.SessionID, TurnID: in.TurnID, Kind: "turn_accepted", At: time.Now(), Payload: json.RawMessage(`{}`)}
	first, created, _, err := s.ReserveTurnInput(ctx, in, event)
	if err != nil || !created {
		t.Fatalf("reserve: %+v %v %v", first, created, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sessionsqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in.TurnID, in.PayloadHash = "new-proposed-turn", "different-hash"
	stored, created, _, err := s.ReserveTurnInput(ctx, in, event)
	if err != nil || created || stored.TurnID != first.TurnID || stored.AcceptedSeq != first.AcceptedSeq || stored.PayloadHash != "original-hash" {
		t.Fatalf("dedup reservation: %+v %v %v", stored, created, err)
	}
	// The store returns the original hash; TurnExecutor, not this store, rejects
	// mismatched payloads. This is not evidence of an atomic TaskAttempt binding.
}

func TestCompletedReviewNeedsRedeliveryAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flux.db")
	r := openFlux(t, path)
	recorder := &recordingTool{}
	r.ToolRegistry().Register(recorder)
	def := reviewDefinition()
	if err := r.RegisterWorkflow(ctx, def); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(ctx, def, map[string]any{"task_id": "coding-task"})
	if err != nil || result.Err != nil || result.Status != "suspended" {
		t.Fatalf("run: %+v %v", result, err)
	}
	completed, err := r.CompleteAwait(ctx, result.TaskID, "review1", map[string]any{"verdict": "pass"}, "")
	if err != nil || completed.Err != nil {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	// CompleteAwait acknowledges binding completion and queues continuation;
	// without a worker it has not run the accepted stage yet.
	binding, err := r.AwaitBinding(ctx, result.TaskID, "review1")
	if err != nil || binding.Status != domain.AwaitBindingCompleted {
		t.Fatalf("binding: %+v %v", binding, err)
	}
	if err = r.Shutdown(); err != nil {
		t.Fatal(err)
	}
	r = openFlux(t, path)
	r.ToolRegistry().Register(recorder)
	startWorker(t, r)
	if err = r.Engine().TaskRepo().Enqueue(ctx, result.TaskID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, result.TaskID, domain.TaskSuccess)
	if calls := recorder.snapshot(); !reflect.DeepEqual(calls, []string{"implement1", "accepted1"}) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestFluxTransactionCanBindCommandAndTaskAtomically(t *testing.T) {
	r := openFlux(t, filepath.Join(t.TempDir(), "flux.db"))
	db := r.DB()
	if err := db.Exec(`CREATE TABLE b2_probe_commands(command_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL, flux_task_id INTEGER UNIQUE NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected crash boundary")
	var committedID int64
	write := func(fail bool) error {
		return db.Transaction(func(tx *gorm.DB) error {
			// Let SQLite reserve the primary key, then persist RootID and the
			// command receipt in this transaction before any worker can see it.
			repo := query.NewTaskRepository(tx, nil)
			task := &domain.Task{Status: domain.TaskPending}
			if err := repo.Create(context.Background(), task); err != nil {
				return err
			}
			task.RootID = task.ID
			if err := repo.Update(context.Background(), task); err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO b2_probe_commands VALUES ('command-1','hash-1',?)`, task.ID).Error; err != nil {
				return err
			}
			if fail {
				return injected
			}
			committedID = task.ID
			return nil
		})
	}
	if err := write(true); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("tasks").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rollback task=%d %v", count, err)
	}
	if err := db.Table("b2_probe_commands").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rollback command=%d %v", count, err)
	}
	if err := write(false); err != nil {
		t.Fatal(err)
	}
	if err := write(false); err == nil {
		t.Fatal("duplicate command unexpectedly created a second task")
	}
	if err := db.Table("tasks").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("committed tasks=%d %v", count, err)
	}
	if task, err := r.Status(context.Background(), committedID); err != nil || task.Status != domain.TaskPending || task.RootID != task.ID || task.ID == 0 {
		t.Fatalf("committed task=%+v %v", task, err)
	}
}
