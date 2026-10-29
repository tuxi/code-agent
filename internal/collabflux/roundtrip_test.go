package collabflux

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestTaskCommandReceiptRoundTripAcrossDatabases(t *testing.T) {
	runtime, adapter, seed, _ := adapterFixture(t)
	var seedRun collab.ManagedRun
	if err := json.Unmarshal(seed.Payload, &seedRun); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	if out, err := exec.CommandContext(ctx, "git", "-c", "init.defaultBranch=main", "init", "--template=", root).CombinedOutput(); err != nil {
		t.Fatalf("git init %s %v", out, err)
	}
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Save(ctx, &session.Session{ID: "executor", WorkspacePath: root}); err != nil {
		t.Fatal(err)
	}
	repo := store.TaskRepository("server")
	managed := repo.(collab.ManagedRepository)
	actor := collab.Principal{ID: "owner", LocalSessions: true}
	identity, err := repo.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := repo.Create(ctx, actor, collab.CreateInput{RequestID: "task", WorkspaceSessionID: "executor", Goal: "offline round trip"})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := repo.Participate(ctx, actor, task.ID, collab.ParticipationInput{RequestID: "executor", Role: "executor", SessionRef: collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: "executor"}})
	if err != nil {
		t.Fatal(err)
	}
	external, err := repo.RegisterExternal(ctx, actor, collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, role := range []string{"planner", "reviewer"} {
		p, err := repo.Participate(ctx, actor, task.ID, collab.ParticipationInput{RequestID: role, Role: role, SessionRef: external})
		if err != nil {
			t.Fatal(err)
		}
		parts[role] = p.ID
	}
	plan, err := repo.Report(ctx, actor, task.ID, "plan", collab.ReportInput{RequestID: "plan", ParticipationID: parts["planner"], SubmissionID: "plan", Iteration: 1, Content: "offline plan"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := managed.PrepareRun(ctx, actor, task.ID, collab.PrepareRunInput{RequestID: "run", PlanArtifactID: plan.ID, ExecutorParticipationID: executor.ID, ReviewerParticipationID: parts["reviewer"], MaxIterations: 2, Workflow: seedRun.Workflow})
	if err != nil {
		t.Fatal(err)
	}
	commands, err := managed.PendingCommands(ctx, actor, 10)
	if err != nil || len(commands) != 1 {
		t.Fatal(commands, err)
	}
	first, err := adapter.CommitSubmission(ctx, commands[0])
	if err != nil {
		t.Fatal(err)
	}
	// Lose the cross-store acknowledgement, then redeliver the original command.
	again, err := adapter.CommitSubmission(ctx, commands[0])
	if err != nil || again != first {
		t.Fatal("ack loss forked Flux task", again, err)
	}
	if err = managed.AcknowledgeSubmission(ctx, actor, again); err != nil {
		t.Fatal(err)
	}
	run, err := managed.Run(ctx, actor, prepared.ID)
	if err != nil || run.FluxTaskID != first.FluxTaskID || run.State != "submitted" {
		t.Fatal(run, err)
	}
	id, err := strconv.ParseInt(run.FluxTaskID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	fluxTask, err := runtime.Status(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var bound collab.ManagedRun
	if err = json.Unmarshal(fluxTask.InputJSON, &bound); err != nil || bound.TaskID != task.ID || bound.RunID != run.RunID || bound.PlanHash != run.PlanHash || bound.StoreID != identity.StoreID {
		t.Fatal("wrong cross-store binding", bound, err)
	}
	snapshot, err := repo.Snapshot(ctx, actor, task.ID)
	if err != nil || snapshot.ManagedRun == nil || snapshot.WorkflowRef == nil || *snapshot.WorkflowRef != run.FluxTaskID || snapshot.TaskAcceptance != "not_reviewed" {
		t.Fatal(snapshot, err)
	}
	if _, err = managed.ReserveDispatch(ctx, actor, run.RunID, collab.ReserveDispatchInput{RequestID: "dispatch", Epoch: run.Epoch, Iteration: 1, Message: "execute approved plan"}); err != nil {
		t.Fatal(err)
	}
}
