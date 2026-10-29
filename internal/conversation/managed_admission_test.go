package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/collab"
	"code-agent/internal/model"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
)

type managedApproval struct {
	decisions chan agent.Verdict
	requested chan struct{}
}

func (g *managedApproval) Approve(string, json.RawMessage) agent.Verdict {
	g.requested <- struct{}{}
	return <-g.decisions
}

type managedScriptBuilder struct {
	builds   atomic.Int32
	contexts chan RuntimeContext
	marker   string
}

func (b *managedScriptBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "runtime-default", Model: "resolved-default"}, nil
}
func (b *managedScriptBuilder) Build(rctx RuntimeContext) TurnRunner {
	b.builds.Add(1)
	b.contexts <- rctx
	return &managedScriptRunner{rctx: rctx, marker: b.marker}
}

type managedScriptRunner struct {
	rctx   RuntimeContext
	marker string
}

func (r *managedScriptRunner) RunTurn(ctx context.Context, sess *session.Session, _ string) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, sess)
}
func (r *managedScriptRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, sess)
}
func (r *managedScriptRunner) RunPreparedTurn(_ context.Context, sess *session.Session) (agent.TurnResult, error) {
	r.rctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnStarted, SessionID: sess.ID, TurnID: r.rctx.TurnID})
	if r.rctx.Approver == nil || r.rctx.Approver.Approve("edit_file", json.RawMessage(`{"path":"marker.txt"}`)) != agent.VerdictAllow {
		return agent.TurnResult{}, errors.New("denied")
	}
	if err := os.WriteFile(r.marker, []byte("approved"), 0600); err != nil {
		return agent.TurnResult{}, err
	}
	r.rctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID, TurnID: r.rctx.TurnID})
	return agent.TurnResult{}, nil
}

type managedHarness struct {
	store        *sessionsqlite.Store
	repo         ConversationRepository
	tasks        collab.Repository
	managed      collab.ManagedRepository
	executor     *TurnExecutor
	run          collab.ManagedRun
	dispatch     collab.ManagedDispatch
	actor        collab.Principal
	builder      *managedScriptBuilder
	gate         *managedApproval
	root         string
	otherSession string
}

func newManagedHarness(t *testing.T) *managedHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	gitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(gitCtx, "git", "-c", "init.defaultBranch=main", "init", "--template=", root).CombinedOutput(); err != nil {
		t.Fatalf("git init %s %v", out, err)
	}
	s, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	repo := NewSQLiteRepository(s, 32000, 24000, "runtime-default", "", nil)
	target, err := repo.Create(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdateName(ctx, target.ID, "executor"); err != nil {
		t.Fatal(err)
	}
	other, err := repo.Create(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	r := repo.(collab.RepositoryProvider).TaskRepository("server")
	a := collab.Principal{ID: "owner", LocalSessions: true}
	identity, err := r.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := r.Create(ctx, a, collab.CreateInput{RequestID: "task", WorkspaceSessionID: target.ID, Goal: "offline task"})
	if err != nil {
		t.Fatal(err)
	}
	local, err := r.Participate(ctx, a, task.ID, collab.ParticipationInput{RequestID: "executor", Role: "executor", SessionRef: collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: target.ID}})
	if err != nil {
		t.Fatal(err)
	}
	external, err := r.RegisterExternal(ctx, a, collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, role := range []string{"planner", "reviewer"} {
		p, err := r.Participate(ctx, a, task.ID, collab.ParticipationInput{RequestID: role, Role: role, SessionRef: external})
		if err != nil {
			t.Fatal(err)
		}
		parts[role] = p.ID
	}
	plan, err := r.Report(ctx, a, task.ID, "plan", collab.ReportInput{RequestID: "plan", ParticipationID: parts["planner"], SubmissionID: "plan", Content: "write marker only after approval", Iteration: 1})
	if err != nil {
		t.Fatal(err)
	}
	m := r.(collab.ManagedRepository)
	prepared, err := m.PrepareRun(ctx, a, task.ID, collab.PrepareRunInput{RequestID: "run", PlanArtifactID: plan.ID, ExecutorParticipationID: local.ID, ReviewerParticipationID: parts["reviewer"], MaxIterations: 2, Workflow: collab.WorkflowBinding{Name: "offline", VersionID: "1", Hash: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.Run(ctx, a, prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := m.PendingCommands(ctx, a, 10)
	if err != nil || len(commands) != 1 {
		t.Fatal(commands, err)
	}
	if err = m.AcknowledgeSubmission(ctx, a, collab.SubmissionReceipt{StoreID: run.StoreID, CommandID: run.CommandID, PayloadHash: commands[0].PayloadHash, Workflow: run.Workflow, FluxTaskID: "123"}); err != nil {
		t.Fatal(err)
	}
	reserved, err := m.ReserveDispatch(ctx, a, run.RunID, collab.ReserveDispatchInput{RequestID: "dispatch", Epoch: run.Epoch, Iteration: 1, Message: "implement plan"})
	if err != nil {
		t.Fatal(err)
	}
	ref := collab.ManagedTurnRef{ServerID: run.ServerID, StoreID: run.StoreID, TaskID: run.TaskID, RunID: run.RunID, Epoch: run.Epoch, DispatchID: reserved.ID}
	d, err := m.Dispatch(ctx, a, ref)
	if err != nil {
		t.Fatal(err)
	}
	builder := &managedScriptBuilder{contexts: make(chan RuntimeContext, 8), marker: filepath.Join(root, "marker.txt")}
	gate := &managedApproval{decisions: make(chan agent.Verdict, 8), requested: make(chan struct{}, 8)}
	e := NewTurnExecutor(repo, &StoreEventAdapter{Store: s}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
	e.SetApprover(d.SessionID, gate)
	e.SetApprovalMode(d.SessionID, "ask")
	t.Cleanup(func() { gate.decisions <- agent.VerdictDeny; e.Shutdown() })
	return &managedHarness{store: s, repo: repo, tasks: r, managed: m, executor: e, run: run, dispatch: d, actor: a, builder: builder, gate: gate, root: root, otherSession: other.ID}
}

func TestManagedDispatchPreservesApprovalAndDeduplicates(t *testing.T) {
	h := newManagedHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := h.executor.AcceptManagedDispatch(ctx, ctx, h.actor, h.dispatch.Ref)
	if err != nil || first.TurnID == "" || first.Cursor <= 0 {
		t.Fatal(first, err)
	}
	var rctx RuntimeContext
	select {
	case rctx = <-h.builder.contexts:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if rctx.ApprovalMode != "ask" || rctx.Approver != h.gate || rctx.Model != "runtime-default" || rctx.ResolvedModel != "resolved-default" {
		t.Fatalf("target configuration changed %+v", rctx)
	}
	select {
	case <-h.gate.requested:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err = os.Stat(h.builder.marker); !os.IsNotExist(err) {
		t.Fatal("mutation before approval", err)
	}
	again, err := h.executor.AcceptManagedDispatch(ctx, ctx, h.actor, h.dispatch.Ref)
	if err != nil || again != first {
		t.Fatal("duplicate acceptance", again, err)
	}
	_, err = h.executor.AcceptCrossSessionMessage(ctx, ctx, h.dispatch.SessionID, CrossSessionEnvelope{Text: "bypass", SenderSessionID: "source", MessageID: "other", CorrelationID: "other", Intent: "request"})
	if !errors.Is(err, collab.ErrManagedWorkspace) || h.executor.active.ApprovalMode(h.dispatch.SessionID) != "ask" {
		t.Fatal("cross-session changed target permissions", err)
	}
	if _, err = h.executor.ExecuteWithRequestID(ctx, h.otherSession, "plain", "write", "", ""); !errors.Is(err, collab.ErrManagedWorkspace) {
		t.Fatal("ordinary send bypassed claim", err)
	}
	loaded, err := h.repo.Load(ctx, h.otherSession)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.executor.ExecuteWithSession(ctx, loaded, "write", ""); !errors.Is(err, collab.ErrManagedWorkspace) {
		t.Fatal("TUI path bypassed claim", err)
	}
	h.gate.decisions <- agent.VerdictAllow
	for ctx.Err() == nil {
		input, err := h.store.TurnInput(ctx, h.dispatch.SessionID, h.dispatch.RequestID)
		if err == nil && input.State == session.TurnInputCompleted && !h.executor.HasActivity(h.dispatch.SessionID) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	content, err := os.ReadFile(h.builder.marker)
	if err != nil || string(content) != "approved" {
		t.Fatal(string(content), err)
	}
	if h.builder.builds.Load() != 1 {
		t.Fatal("duplicate execution")
	}
	snap, err := h.tasks.Snapshot(ctx, h.actor, h.run.TaskID)
	if err != nil || snap.TaskAcceptance != "not_reviewed" || snap.Phase != "implementing" || snap.CurrentAttemptID == "" {
		t.Fatal("turn success became task acceptance", snap.CodingTask, err)
	}
	for _, attempt := range snap.Attempts {
		if attempt.AttemptID == snap.CurrentAttemptID && attempt.SourceRef.TurnID != first.TurnID {
			t.Fatal("wrong turn")
		}
	}
	if _, err = h.executor.AcceptManagedDispatch(ctx, ctx, h.actor, h.dispatch.Ref); err != nil || h.builder.builds.Load() != 1 {
		t.Fatal("terminal duplicate replayed", err)
	}
}

func TestManagedRecoveryFreezesUnknownExecution(t *testing.T) {
	h := newManagedHarness(t)
	ctx := context.Background()
	d := h.dispatch
	release, err := h.store.AcquireTaskExecution(ctx, d.SessionID, h.root, d.RequestID, &d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: d.SessionID, RequestID: d.RequestID, TurnID: "interrupted", Text: d.Message, PayloadHash: "hash", ManagedTask: &d.Ref}
	stored, _, _, err := h.store.ReserveTurnInput(ctx, input, session.EventRecord{SessionID: d.SessionID, TurnID: input.TurnID, Kind: "turn_accepted", At: time.Now(), Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := h.repo.Load(ctx, d.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: d.Message, OriginTurnID: input.TurnID})
	sess.MarkPaused(time.Now())
	if err = h.store.StartTurnInput(ctx, stored, sess); err != nil {
		t.Fatal(err)
	}
	release()
	if n, err := h.executor.RecoverTurnInputs(ctx); err != nil || n != 0 {
		t.Fatal("automatic recovery dispatched", n, err)
	}
	run, err := h.managed.Run(ctx, h.actor, h.run.RunID)
	if err != nil || run.State != "recovery_required" {
		t.Fatal(run, err)
	}
	snap, err := h.tasks.Snapshot(ctx, h.actor, h.run.TaskID)
	if err != nil || snap.WaitReason != "recovery_required" {
		t.Fatal(snap, err)
	}
	if _, err = h.executor.Resume(ctx, d.SessionID); !errors.Is(err, collab.ErrRecoveryRequired) {
		t.Fatal("resume bypassed reconciliation", err)
	}
	if n, err := h.executor.RecoverSessionTurnInputs(ctx, d.SessionID); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	again, _ := h.tasks.Snapshot(ctx, h.actor, h.run.TaskID)
	if again.Revision != snap.Revision || h.builder.builds.Load() != 0 {
		t.Fatal("recovery mutated twice or invoked model")
	}
	receipt, err := h.executor.AcceptManagedDispatch(ctx, ctx, h.actor, d.Ref)
	if err != nil || receipt.TurnID != stored.TurnID || h.builder.builds.Load() != 0 {
		t.Fatal("reattach resumed interrupted work", receipt, err)
	}
}

func TestCrossSessionRequestKeepsTargetApprovalMode(t *testing.T) {
	store := session.NewMemoryStore()
	repo := NewSQLiteRepository(store, 32000, 24000, "target", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	builder := &defaultOnlyRunBuilder{}
	active := NewActiveTurnRegistry()
	active.SetApprovalMode(sess.ID, "ask")
	e := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, active, NewSubscriptionManager(), builder)
	defer e.Shutdown()
	_, err = e.AcceptCrossSessionMessage(context.Background(), context.Background(), sess.ID, CrossSessionEnvelope{Text: "work", SenderSessionID: "source", MessageID: "request", CorrelationID: "correlation", Intent: "request"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		input, err := repo.(TurnInputRepository).TurnInput(context.Background(), sess.ID, "request")
		if err == nil && input.State == session.TurnInputCompleted {
			if builder.lastCtx.ApprovalMode != "ask" {
				t.Fatal("target mode elevated", builder.lastCtx.ApprovalMode)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cross-session turn did not complete")
}
