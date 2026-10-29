package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/jobs"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestManagedAdmissionWithRealJobEventSinkHistory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "--template="}, {"-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "baseline"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for id, path := range map[string]string{"target": root, "other": t.TempDir()} {
		if err = store.Save(ctx, &session.Session{ID: id, WorkspacePath: path, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	sink := NewJobEventSink(ctx, store)
	job := jobs.Snapshot{ID: "job_history", Command: "offline fixture only", Owner: jobs.Owner{SessionID: "other", TurnID: "old-turn", CallID: "old-call"}}
	sink.JobStarted(job)
	job.Status = jobs.Exited
	sink.JobFinished(job)
	child, err := store.SessionEvents(ctx, job.ID)
	if err != nil || len(child) != 2 {
		t.Fatal(child, err)
	}
	parent, err := store.SessionEvents(ctx, "other")
	if err != nil || len(parent) != 2 {
		t.Fatal(parent, err)
	}
	r := store.TaskRepository("server")
	a := collab.Principal{ID: "owner", LocalSessions: true}
	task, err := r.Create(ctx, a, collab.CreateInput{RequestID: "task", WorkspaceSessionID: "target", Goal: "offline ownership admission"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := r.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ext, err := r.RegisterExternal(ctx, a, collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, role := range []string{"planner", "reviewer", "executor"} {
		ref := ext
		if role == "executor" {
			ref = collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: "target"}
		}
		part, e := r.Participate(ctx, a, task.ID, collab.ParticipationInput{RequestID: role, Role: role, SessionRef: ref})
		if e != nil {
			t.Fatal(e)
		}
		parts[role] = part.ID
	}
	plan, err := r.Report(ctx, a, task.ID, "plan", collab.ReportInput{RequestID: "plan", SubmissionID: "plan", ParticipationID: parts["planner"], Iteration: 1, Content: "offline plan"})
	if err != nil {
		t.Fatal(err)
	}
	in := collab.PrepareRunInput{RequestID: "run", PlanArtifactID: plan.ID, ExecutorParticipationID: parts["executor"], ReviewerParticipationID: parts["reviewer"], MaxIterations: 1, Workflow: collab.WorkflowBinding{Name: "offline", VersionID: "1", Hash: strings.Repeat("a", 64)}, ReviewPolicy: &collab.ReviewPolicy{Files: []string{"main.txt"}, ValidationCommands: []string{"git diff --check"}}}
	m := r.(collab.ManagedRepository)
	first, err := m.PrepareRun(ctx, a, task.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.PrepareRun(ctx, a, task.ID, in)
	if err != nil || first.ID != again.ID {
		t.Fatal(first, again, err)
	}
	check, err := r.(collab.RecoveryRepository).ReconcileRun(ctx, a, first.ID)
	if err != nil || !check.Safe {
		t.Fatal(check, err)
	}
}
