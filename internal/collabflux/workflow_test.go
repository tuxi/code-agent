package collabflux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/collab"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	flux "github.com/tuxi/flux-workflow/runtime"
	"github.com/tuxi/flux-workflow/tool"
)

type offlineApproval struct{ calls atomic.Int32 }

func (a *offlineApproval) Approve(string, json.RawMessage) agent.Verdict {
	a.calls.Add(1)
	return agent.VerdictAllow
}

type offlineBuilder struct {
	calls      atomic.Int32
	root, mode string
	badConfig  atomic.Bool
}

func (*offlineBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "runtime-default", Model: "resolved-default"}, nil
}
func (b *offlineBuilder) Build(c conversation.RuntimeContext) conversation.TurnRunner {
	if c.Model != "runtime-default" || c.ResolvedModel != "resolved-default" || c.ApprovalMode != "ask" {
		b.badConfig.Store(true)
	}
	return &offlineRunner{builder: b, c: c}
}

type offlineRunner struct {
	builder *offlineBuilder
	c       conversation.RuntimeContext
}

func (r *offlineRunner) RunTurn(ctx context.Context, s *session.Session, _ string) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *offlineRunner) ResumeTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *offlineRunner) RunPreparedTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	n := r.builder.calls.Add(1)
	emit := func(e agent.Event) { e.SessionID = s.ID; e.TurnID = r.c.TurnID; r.c.Publisher.Emit(e) }
	emit(agent.Event{Kind: agent.EventTurnStarted})
	if r.builder.mode == "cancelled" {
		return agent.TurnResult{}, context.Canceled
	}
	args := json.RawMessage(`{"path":"main.txt"}`)
	if r.c.Approver == nil || r.c.Approver.Approve("edit_file", args) != agent.VerdictAllow {
		return agent.TurnResult{}, errors.New("denied")
	}
	emit(agent.Event{Kind: agent.EventToolStarted, CallID: "edit", ToolName: "edit_file", ToolArgs: string(args)})
	if err := os.WriteFile(filepath.Join(r.builder.root, "main.txt"), []byte(fmt.Sprintf("candidate %d\n", n)), 0600); err != nil {
		return agent.TurnResult{}, err
	}
	emit(agent.Event{Kind: agent.EventToolFinished, CallID: "edit", ToolName: "edit_file", Output: json.RawMessage(`{"ok":true}`)})
	if r.builder.mode != "missing" {
		args = json.RawMessage(`{"command":"git diff --check"}`)
		if r.c.Approver.Approve("run_command", args) != agent.VerdictAllow {
			return agent.TurnResult{}, errors.New("denied")
		}
		emit(agent.Event{Kind: agent.EventToolStarted, CallID: "test", ToolName: "run_command", ToolArgs: string(args)})
		cmd := exec.CommandContext(ctx, "git", "-C", r.builder.root, "diff", "--check")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return agent.TurnResult{}, err
		}
		emit(agent.Event{Kind: agent.EventToolStdout, CallID: "test", Chunk: string(out)})
		emit(agent.Event{Kind: agent.EventToolFinished, CallID: "test", ToolName: "run_command", Output: json.RawMessage(`{"command":"git diff --check","decision":"confirm","status":"completed","exit_code":0}`)})
	}
	emit(agent.Event{Kind: agent.EventTurnFinished})
	return agent.TurnResult{}, nil
}

type workflowHarness struct {
	runtime  *flux.Runtime
	adapter  *Adapter
	stages   *Stages
	executor *conversation.TurnExecutor
	tasks    collab.Repository
	managed  collab.ManagedRepository
	reviews  collab.ReviewRepository
	actor    collab.Principal
	run      collab.ManagedRun
	id       int64
	builder  *offlineBuilder
	approval *offlineApproval
	root     string
}

func workflowFixture(t *testing.T, rounds int, mode string) *workflowHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	git("-c", "init.defaultBranch=main", "init", "--template=")
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "--", "main.txt")
	git("-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "-m", "baseline")
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	conversations := conversation.NewSQLiteRepository(store, 32000, 24000, "runtime-default", "", nil)
	target, err := conversations.Create(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	tasks := store.TaskRepository("server")
	m := tasks.(collab.ManagedRepository)
	v := tasks.(collab.ReviewRepository)
	a := collab.Principal{ID: "owner", LocalSessions: true}
	identity, err := tasks.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := tasks.Create(ctx, a, collab.CreateInput{RequestID: "task", WorkspaceSessionID: target.ID, Goal: "offline integration"})
	if err != nil {
		t.Fatal(err)
	}
	local, err := tasks.Participate(ctx, a, task.ID, collab.ParticipationInput{RequestID: "executor", Role: "executor", SessionRef: collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: target.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := tasks.RegisterExternal(ctx, a, collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, role := range []string{"planner", "reviewer"} {
		part, err := tasks.Participate(ctx, a, task.ID, collab.ParticipationInput{RequestID: role, Role: role, SessionRef: ext})
		if err != nil {
			t.Fatal(err)
		}
		parts[role] = part.ID
	}
	plan, err := tasks.Report(ctx, a, task.ID, "plan", collab.ReportInput{RequestID: "plan", ParticipationID: parts["planner"], SubmissionID: "plan", Iteration: 1, Content: "change main.txt and validate"})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := flux.NewLocal(filepath.Join(t.TempDir(), "flux.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Shutdown() })
	def, err := CollaborationDefinition(rounds)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.RegisterWorkflow(ctx, def); err != nil {
		t.Fatal(err)
	}
	version, err := query.NewWorkflowVersionRepository(runtime.DB()).GetLatestByWorkflowName(ctx, def.Name)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := m.PrepareRun(ctx, a, task.ID, collab.PrepareRunInput{RequestID: "prepare", PlanArtifactID: plan.ID, ExecutorParticipationID: local.ID, ReviewerParticipationID: parts["reviewer"], MaxIterations: rounds, Workflow: collab.WorkflowBinding{Name: def.Name, VersionID: strconv.FormatInt(version.ID, 10), Hash: version.Hash}, ReviewPolicy: &collab.ReviewPolicy{Files: []string{"main.txt"}, ValidationCommands: []string{"git diff --check"}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.Run(ctx, a, prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	builder := &offlineBuilder{root: root, mode: mode}
	approval := &offlineApproval{}
	executor := conversation.NewTurnExecutor(conversations, &conversation.StoreEventAdapter{Store: store}, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	t.Cleanup(executor.Shutdown)
	executor.SetApprovalMode(target.ID, "ask")
	executor.SetApprover(target.ID, approval)
	stages := &Stages{Runtime: runtime, Tasks: tasks, Managed: m, Reviews: v, Actor: a, Dispatch: func(ctx context.Context, ref collab.ManagedTurnRef) error {
		_, err := executor.AcceptManagedDispatch(ctx, context.Background(), a, ref)
		return err
	}}
	if err = stages.Register(); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(runtime)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := m.PendingCommands(ctx, a, 10)
	if err != nil || len(commands) != 1 {
		t.Fatal(commands, err)
	}
	receipt, err := adapter.CommitSubmission(ctx, commands[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AcknowledgeSubmission(ctx, a, receipt); err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(receipt.FluxTaskID, 10, 64)
	if err = adapter.EnqueueKnownPending(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err = runtime.Start(ctx, flux.WithTaskWorkers(1), flux.WithAsyncWorkers(0), flux.WithAwaitPollWorkers(0), flux.WithRecoveryScanner(false)); err != nil {
		t.Fatal(err)
	}
	return &workflowHarness{runtime: runtime, adapter: adapter, stages: stages, executor: executor, tasks: tasks, managed: m, reviews: v, actor: a, run: run, id: id, builder: builder, approval: approval, root: root}
}

func (h *workflowHarness) binding(t *testing.T, name string) *domain.AwaitBinding {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		binding, err := h.runtime.AwaitBinding(context.Background(), h.id, name)
		if err == nil && binding != nil && binding.Status == domain.AwaitBindingWaiting {
			return binding
		}
		task, err := h.runtime.Status(context.Background(), h.id)
		if err == nil && task.Status == domain.TaskFailed {
			t.Fatalf("Flux failed: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	task, _ := h.runtime.Status(context.Background(), h.id)
	snapshot, _ := h.tasks.Snapshot(context.Background(), h.actor, h.run.TaskID)
	d, _ := h.reviews.DispatchAt(context.Background(), h.actor, h.run.RunID, snapshot.ManagedRun.CurrentIteration)
	o, observeErr := h.reviews.ObserveTurn(context.Background(), h.actor, d.Ref)
	t.Fatalf("no binding %s: flux=%s task=%s reason=%s run=%s iteration=%d turn=%s observe=%v calls=%d", name, task.Status, snapshot.State, snapshot.WaitReason, snapshot.ManagedRun.State, snapshot.ManagedRun.CurrentIteration, o.Status, observeErr, h.builder.calls.Load())
	return nil
}

func (h *workflowHarness) resolve(t *testing.T, b *domain.AwaitBinding) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		payload, err := h.stages.ResolveAwait(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		if payload != nil {
			receipt, err := h.runtime.CompleteAwait(context.Background(), h.id, b.NodeName, payload, "")
			if err != nil || receipt.Err != nil {
				t.Fatal(receipt, err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("resolver remained pending", b.NodeName)
}

func (h *workflowHarness) submit(t *testing.T, n int, verdict string) collab.Receipt {
	t.Helper()
	ctx := context.Background()
	run, err := h.managed.Run(ctx, h.actor, h.run.RunID)
	if err != nil || run.CurrentBundle == nil {
		t.Fatal(run, err)
	}
	b, err := h.reviews.ReadBundle(ctx, h.actor, run.CurrentBundle.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Iteration != n || b.TurnID == "" || b.AttemptID == "" {
		t.Fatal(b)
	}
	receipt, err := h.reviews.SubmitReview(ctx, h.actor, collab.ReviewInput{RequestID: fmt.Sprintf("review-%d", n), SubmissionID: fmt.Sprintf("submission-%d", n), RunID: run.RunID, Epoch: run.Epoch, ReviewerParticipationID: run.ReviewerParticipationID, AttemptID: b.AttemptID, Plan: b.Plan, Artifact: *run.CurrentBundle, Verdict: verdict, Findings: []string{"improve edge case"}})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCollaborationDAGUsesRealExecutorAndSameSessionRepair(t *testing.T) {
	h := workflowFixture(t, 2, "")
	ctx := context.Background()
	var sessionID string
	for n, verdict := range []string{"request_changes", "pass"} {
		h.resolve(t, h.binding(t, fmt.Sprintf("wait_turn_%d", n+1)))
		binding := h.binding(t, fmt.Sprintf("wait_review_%d", n+1))
		pending, err := h.stages.ResolveAwait(ctx, binding)
		if err != nil || pending != nil {
			t.Fatal("review did not wait", pending, err)
		}
		d, err := h.reviews.DispatchAt(ctx, h.actor, h.run.RunID, n+1)
		if err != nil {
			t.Fatal(err)
		}
		if sessionID != "" && d.SessionID != sessionID {
			t.Fatal("repair forked session")
		}
		sessionID = d.SessionID
		h.submit(t, n+1, verdict)
		h.resolve(t, binding)
	}
	waitWorkflowSuccess(t, h)
	snap, err := h.tasks.Snapshot(ctx, h.actor, h.run.TaskID)
	if err != nil || snap.State != "completed" || snap.TaskAcceptance != "accepted" || h.builder.calls.Load() != 2 || h.builder.badConfig.Load() || h.approval.calls.Load() != 4 {
		t.Fatal(snap.CodingTask, h.builder.calls.Load(), h.approval.calls.Load(), err)
	}
	if _, err = h.stages.Execute(ctx, map[string]any{"run_id": h.run.RunID}, nil); !errors.Is(err, collab.ErrForbidden) {
		t.Fatal("no execution metadata accepted", err)
	}
	badCtx := tool.ContextWithExecutionMeta(ctx, tool.ExecutionMeta{TaskID: h.id + 1, NodeName: "dispatch_1"})
	if _, err = h.stages.Execute(badCtx, map[string]any{"run_id": h.run.RunID, "epoch": "1", "iteration": 1, "stage": "dispatch"}, nil); err == nil {
		t.Fatal("wrong Flux identity accepted")
	}
}

func waitWorkflowSuccess(t *testing.T, h *workflowHarness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := h.runtime.Status(context.Background(), h.id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == domain.TaskSuccess {
			return
		}
		if task.Status == domain.TaskFailed {
			t.Fatalf("workflow failed: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("workflow did not finish")
}

func TestCollaborationDAGAttentionBranchesAndForgedAwait(t *testing.T) {
	for _, tc := range []struct{ name, mode, verdict, reason string }{{"inconclusive", "", "inconclusive", "inconclusive_review"}, {"exhausted", "", "request_changes", "iterations_exhausted"}, {"missing", "missing", "pass", "needs_validation"}, {"cancelled", "cancelled", "", "workflow_requires_attention"}, {"forged_review", "", "forged", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			h := workflowFixture(t, 1, tc.mode)
			ctx := context.Background()
			h.resolve(t, h.binding(t, "wait_turn_1"))
			if tc.mode != "cancelled" {
				b := h.binding(t, "wait_review_1")
				bad := *b
				provider := "unknown"
				bad.Provider = &provider
				if _, err := h.stages.ResolveAwait(ctx, &bad); !errors.Is(err, collab.ErrUnavailable) {
					t.Fatal("unknown provider accepted", err)
				}
				if tc.verdict == "forged" {
					completion, err := h.runtime.CompleteAwait(ctx, h.id, b.NodeName, map[string]any{"verdict": "pass"}, "")
					if err != nil || completion.Err != nil {
						t.Fatal(completion, err)
					}
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						task, _ := h.runtime.Status(ctx, h.id)
						if task.Status == domain.TaskFailed {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					snap, err := h.tasks.Snapshot(ctx, h.actor, h.run.TaskID)
					if err != nil || snap.TaskAcceptance == "accepted" || snap.State == "completed" || h.builder.calls.Load() != 1 {
						t.Fatal("forged await authorized acceptance", snap.CodingTask, err)
					}
					return
				}
				h.submit(t, 1, tc.verdict)
				h.resolve(t, b)
			}
			waitWorkflowSuccess(t, h)
			snap, err := h.tasks.Snapshot(ctx, h.actor, h.run.TaskID)
			if err != nil || snap.State != "waiting" || snap.WaitReason != tc.reason || snap.TaskAcceptance == "accepted" || h.builder.calls.Load() != 1 {
				t.Fatal(snap.CodingTask, err)
			}
		})
	}
}

func TestCollaborationDefinitionFiniteAndVersioned(t *testing.T) {
	for n := 1; n <= 5; n++ {
		d, err := CollaborationDefinition(n)
		if err != nil || d.Name != CollaborationWorkflow {
			t.Fatal(d, err)
		}
		names := map[string]bool{}
		for _, node := range d.Nodes {
			if names[node.Name] {
				t.Fatal("duplicate node")
			}
			names[node.Name] = true
		}
		for _, edge := range d.Edges {
			if !names[edge.From] || !names[edge.To] {
				t.Fatal("bad edge")
			}
			if strings.HasPrefix(edge.To, "dispatch_") {
				to, _ := strconv.Atoi(strings.TrimPrefix(edge.To, "dispatch_"))
				if to > 1 && !strings.HasPrefix(edge.From, fmt.Sprintf("gate_%d", to-1)) {
					t.Fatal("non-gate repair edge", edge)
				}
			}
		}
	}
	for _, n := range []int{0, 6} {
		if _, err := CollaborationDefinition(n); !errors.Is(err, collab.ErrInvalid) {
			t.Fatal(err)
		}
	}
}
