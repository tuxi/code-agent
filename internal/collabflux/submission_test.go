package collabflux

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"code-agent/internal/collab"
	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	flux "github.com/tuxi/flux-workflow/runtime"
)

func adapterFixture(t *testing.T) (*flux.Runtime, *Adapter, collab.ManagedCommand, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flux.db")
	r, err := flux.NewLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	def := &definition.WorkflowDefinition{Name: "b2_a_probe", Nodes: []definition.NodeDefinition{{Name: "start", Type: definition.NodeStart}, {Name: "end", Type: definition.NodeEnd}}, Edges: []definition.EdgeDefinition{{From: "start", To: "end", Type: definition.EdgeNormal}}}
	if err = r.RegisterWorkflow(ctx, def); err != nil {
		t.Fatal(err)
	}
	version, err := query.NewWorkflowVersionRepository(r.DB()).GetLatestByWorkflowName(ctx, def.Name)
	if err != nil {
		t.Fatal(err)
	}
	run := collab.ManagedRun{RunID: "run", TaskID: "task", StoreID: "store", ServerID: "server", CommandID: "command", Epoch: "1", State: "prepared", Workflow: collab.WorkflowBinding{Name: def.Name, VersionID: strconv.FormatInt(version.ID, 10), Hash: version.Hash}}
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	c := collab.ManagedCommand{CommandID: run.CommandID, RunID: run.RunID, StoreID: run.StoreID, Kind: "submit_workflow", Payload: payload, PayloadHash: collab.PayloadHash(payload)}
	a, err := New(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, a, c, path
}

func TestSubmissionReceiptSurvivesAckLossAndRestart(t *testing.T) {
	r, a, c, path := adapterFixture(t)
	ctx := context.Background()
	first, err := a.CommitSubmission(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Shutdown(); err != nil {
		t.Fatal(err)
	}
	r, err = flux.NewLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown()
	a, err = New(r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.CommitSubmission(ctx, c)
	if err != nil || again != first {
		t.Fatal("ack loss forked task", first, again, err)
	}
	var n int64
	if err = r.DB().Table("tasks").Count(&n).Error; err != nil || n != 1 {
		t.Fatal(n, err)
	}
	id, _ := strconv.ParseInt(first.FluxTaskID, 10, 64)
	task, err := r.Status(ctx, id)
	if err != nil || task.RootID != id || task.WorkflowVersionID == 0 || string(task.InputJSON) != string(c.Payload) {
		t.Fatal(task, err)
	}
	if err = a.EnqueueKnownPending(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = r.Start(ctx, flux.WithTaskWorkers(1), flux.WithAsyncWorkers(0), flux.WithAwaitPollWorkers(0), flux.WithRecoveryScanner(false)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err = r.Status(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == domain.TaskSuccess {
			return
		}
		if task.Status == domain.TaskFailed {
			t.Fatalf("task failed %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pending task was not delivered")
}

func TestSubmissionRollbackWhenReceiptWriteFails(t *testing.T) {
	r, a, c, _ := adapterFixture(t)
	ctx := context.Background()
	if err := r.DB().Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON codeagent_submission_receipts BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommitSubmission(ctx, c); err == nil {
		t.Fatal("injected failure ignored")
	}
	for _, table := range []string{"tasks", "codeagent_submission_receipts"} {
		var n int64
		if err := r.DB().Table(table).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("partial %s %d %v", table, n, err)
		}
	}
	if err := r.DB().Exec(`DROP TRIGGER fail_receipt`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommitSubmission(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func TestSubmissionConflictAndFixedWorkflowVersion(t *testing.T) {
	r, a, c, _ := adapterFixture(t)
	ctx := context.Background()
	first, err := a.CommitSubmission(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var run collab.ManagedRun
	if err = json.Unmarshal(c.Payload, &run); err != nil {
		t.Fatal(err)
	}
	run.PlanHash = "changed"
	c.Payload, _ = json.Marshal(run)
	c.PayloadHash = collab.PayloadHash(c.Payload)
	if _, err = a.CommitSubmission(ctx, c); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("payload mismatch accepted", err)
	}
	run.CommandID, c.CommandID = "other-command", "other-command"
	run.Workflow.Hash = "wrong"
	c.Payload, _ = json.Marshal(run)
	c.PayloadHash = collab.PayloadHash(c.Payload)
	if _, err = a.CommitSubmission(ctx, c); !errors.Is(err, collab.ErrConflict) {
		t.Fatal("workflow mismatch accepted", err)
	}
	id, _ := strconv.ParseInt(first.FluxTaskID, 10, 64)
	if err = r.DB().Table("tasks").Where("id=?", id).Update("status", string(domain.TaskRunning)).Error; err != nil {
		t.Fatal(err)
	}
	if err = a.EnqueueKnownPending(ctx, first); !errors.Is(err, collab.ErrRecoveryRequired) {
		t.Fatal("running task requeued", err)
	}
}

func TestConcurrentSubmissionCreatesOneFluxTask(t *testing.T) {
	r, a, c, _ := adapterFixture(t)
	other, err := New(r)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan collab.SubmissionReceipt, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			adapter := a
			if i%2 == 1 {
				adapter = other
			}
			receipt, err := adapter.CommitSubmission(context.Background(), c)
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
	id := ""
	for receipt := range results {
		if id != "" && id != receipt.FluxTaskID {
			t.Fatal("duplicate task")
		}
		id = receipt.FluxTaskID
	}
	var n int64
	if err = r.DB().Table("tasks").Count(&n).Error; err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
