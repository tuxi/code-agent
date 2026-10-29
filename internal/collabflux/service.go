package collabflux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"code-agent/internal/collab"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	flux "github.com/tuxi/flux-workflow/runtime"
)

type ServiceOptions struct {
	Tasks                    collab.Repository
	Actor                    collab.Principal
	Dispatch                 func(context.Context, collab.ManagedTurnRef) error
	ProtectedPaths           []string
	ProtectedPathsForSession func(context.Context, string) ([]string, error)
	// Checkpoint observes durable boundaries. It grants no execution authority;
	// subprocess tests use it to stop at a committed crash window.
	Checkpoint func(string, string)
}

// Service owns a private Flux execution domain. No generic workflow
// Retry/Resume endpoint, memory runtime pool or model tool receives this object.
type Service struct {
	mu         sync.Mutex
	deliveryMu sync.Mutex
	opts       ServiceOptions
	managed    collab.ManagedRepository
	reviews    collab.ReviewRepository
	recovery   collab.RecoveryRepository
	runtime    *flux.Runtime
	adapter    *Adapter
	stages     *Stages
	ready      atomic.Bool
	unLock     func()
	cancel     context.CancelFunc
	done       chan struct{}
	closed     bool
}

var _ collab.ManagedService = (*Service)(nil)

func OpenService(ctx context.Context, path string, opts ServiceOptions) (*Service, error) {
	m, ok := opts.Tasks.(collab.ManagedRepository)
	if !ok {
		return nil, collab.ErrUnavailable
	}
	v, ok := opts.Tasks.(collab.ReviewRepository)
	if !ok {
		return nil, collab.ErrUnavailable
	}
	rec, ok := opts.Tasks.(collab.RecoveryRepository)
	if !ok || !opts.Actor.LocalSessions || opts.Actor.ID == "" || opts.Dispatch == nil || !filepath.IsAbs(path) {
		return nil, collab.ErrUnavailable
	}
	unlock, err := rec.LockManagedHost(ctx)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		unlock()
		return nil, err
	}
	rt, err := flux.NewLocal(path)
	if err != nil {
		unlock()
		return nil, err
	}
	s := &Service{opts: opts, managed: m, reviews: v, recovery: rec, runtime: rt, unLock: unlock, done: make(chan struct{})}
	s.adapter, err = New(rt)
	if err == nil {
		s.stages = &Stages{Runtime: rt, Tasks: opts.Tasks, Managed: m, Reviews: v, Actor: opts.Actor, Dispatch: opts.Dispatch, Checkpoint: opts.Checkpoint}
		err = s.stages.Register()
	}
	if err == nil {
		identity, e := opts.Tasks.Identity(ctx)
		err = e
		if err == nil {
			err = rt.DB().Exec(`CREATE TABLE IF NOT EXISTS codeagent_collaboration_identity(id INTEGER PRIMARY KEY CHECK(id=1),server_id TEXT NOT NULL,store_id TEXT NOT NULL,owner TEXT NOT NULL)`).Error
		}
		if err == nil {
			err = rt.DB().Exec(`INSERT OR IGNORE INTO codeagent_collaboration_identity VALUES(1,?,?,?)`, identity.ServerID, identity.StoreID, opts.Actor.ID).Error
		}
		if err == nil {
			var n int64
			err = rt.DB().Table("codeagent_collaboration_identity").Where("id=1 AND server_id=? AND store_id=? AND owner=?", identity.ServerID, identity.StoreID, opts.Actor.ID).Count(&n).Error
			if err == nil && n != 1 {
				err = collab.ErrConflict
			}
		}
	}
	if err == nil {
		err = s.reconcile(ctx)
	}
	if err != nil {
		rt.Shutdown()
		unlock()
		return nil, err
	}
	live, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	if err = rt.Start(live, flux.WithTaskWorkers(1), flux.WithAsyncWorkers(0), flux.WithAwaitPollWorkers(0), flux.WithRecoveryScanner(false)); err != nil {
		cancel()
		rt.Shutdown()
		unlock()
		return nil, err
	}
	s.ready.Store(true)
	go s.monitor(live)
	return s, nil
}

func (s *Service) Ready() bool { return s != nil && s.ready.Load() }
func (s *Service) checkpoint(stage, id string) {
	if s.opts.Checkpoint != nil {
		s.opts.Checkpoint(stage, id)
	}
}
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.ready.Store(false)
	s.cancel()
	<-s.done
	err := s.runtime.Shutdown()
	s.unLock()
	return err
}

func (s *Service) authorize(a collab.Principal) error {
	if !s.Ready() {
		return collab.ErrUnavailable
	}
	if a != s.opts.Actor {
		return collab.ErrForbidden
	}
	return nil
}

func (s *Service) StartRun(ctx context.Context, a collab.Principal, id string, in collab.StartRunInput) (collab.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(a); err != nil {
		return collab.Receipt{}, err
	}
	def, err := CollaborationDefinition(in.MaxIterations)
	if err != nil {
		return collab.Receipt{}, err
	}
	snapshot, err := s.opts.Tasks.Snapshot(ctx, a, id)
	if err != nil {
		return collab.Receipt{}, err
	}
	var workflow collab.WorkflowBinding
	if snapshot.ManagedRun != nil {
		workflow = snapshot.ManagedRun.Workflow
	} else {
		workflow, err = s.templateBinding(ctx, def)
		if err != nil {
			return collab.Receipt{}, err
		}
	}
	protected := append([]string(nil), s.opts.ProtectedPaths...)
	if s.opts.ProtectedPathsForSession != nil {
		for _, p := range snapshot.Participations {
			if p.ParticipationID == in.ExecutorParticipationID && p.SessionRef.Kind == "local" {
				extra, e := s.opts.ProtectedPathsForSession(ctx, p.SessionRef.SessionID)
				if e != nil {
					return collab.Receipt{}, e
				}
				protected = append(protected, extra...)
			}
		}
	}
	receipt, err := s.managed.PrepareRun(ctx, a, id, collab.PrepareRunInput{RequestID: in.RequestID, PlanArtifactID: in.PlanArtifactID, ExecutorParticipationID: in.ExecutorParticipationID, ReviewerParticipationID: in.ReviewerParticipationID, MaxIterations: in.MaxIterations, Workflow: workflow, ReviewPolicy: &collab.ReviewPolicy{Files: in.Files, ValidationCommands: in.ValidationCommands, ProtectedPaths: protected}})
	if err != nil {
		return receipt, err
	}
	s.checkpoint("run_prepared", receipt.ID)
	// An ACK/transport failure leaves the original command available. Never
	// allocate another run or request identity to hide an uncertain outcome.
	if err = s.deliver(ctx, receipt.ID); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (s *Service) ReportReview(ctx context.Context, a collab.Principal, taskID, runID string, in collab.ReviewInput) (collab.Receipt, error) {
	if err := s.authorize(a); err != nil {
		return collab.Receipt{}, err
	}
	run, err := s.managed.Run(ctx, a, runID)
	if err != nil {
		return collab.Receipt{}, err
	}
	if run.TaskID != taskID || in.RunID != runID {
		return collab.Receipt{}, collab.ErrConflict
	}
	r, err := s.reviews.SubmitReview(ctx, a, in)
	if err == nil {
		s.checkpoint("review_recorded", runID)
	}
	return r, err
}

func (s *Service) Artifact(ctx context.Context, a collab.Principal, taskID, id string) (collab.ReviewBundle, error) {
	if err := s.authorize(a); err != nil {
		return collab.ReviewBundle{}, err
	}
	bundle, err := s.reviews.ReadBundle(ctx, a, id)
	if err != nil {
		return bundle, err
	}
	if bundle.Source.TaskID != taskID {
		return collab.ReviewBundle{}, collab.ErrNotFound
	}
	return bundle, nil
}

func (s *Service) deliver(ctx context.Context, runID string) error {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	commands, err := s.managed.PendingCommands(ctx, s.opts.Actor, 200)
	if err != nil {
		return err
	}
	for _, c := range commands {
		if c.RunID != runID {
			continue
		}
		receipt, err := s.adapter.CommitSubmission(ctx, c)
		if err != nil {
			return err
		}
		s.checkpoint("flux_submitted", runID)
		if err = s.managed.AcknowledgeSubmission(ctx, s.opts.Actor, receipt); err != nil {
			return err
		}
		s.checkpoint("submission_acknowledged", runID)
		return s.adapter.EnqueueKnownPending(ctx, receipt)
	}
	return nil
}

func (s *Service) reconcile(ctx context.Context) error {
	runs, err := s.recovery.ActiveRuns(ctx, s.opts.Actor)
	if err != nil {
		return err
	}
	for _, saved := range runs {
		check, err := s.recovery.ReconcileRun(ctx, s.opts.Actor, saved.RunID)
		if err != nil {
			return err
		}
		if !check.Safe {
			continue
		}
		run := check.Run
		if run.State == "prepared" {
			if err = s.deliver(ctx, run.RunID); err != nil {
				return err
			}
			continue
		}
		if err = s.recoverFlux(ctx, run); err != nil {
			if e := s.recovery.FenceRun(ctx, s.opts.Actor, run.RunID, "workflow_reconciliation_failed"); e != nil {
				return e
			}
		}
	}
	return nil
}

func (s *Service) recoverFlux(ctx context.Context, run collab.ManagedRun) error {
	id, err := strconv.ParseInt(run.FluxTaskID, 10, 64)
	if err != nil || id <= 0 {
		return collab.ErrConflict
	}
	task, err := s.runtime.Status(ctx, id)
	if err != nil {
		return err
	}
	if _, err = s.stages.boundRun(ctx, run.RunID, run.Epoch, id, task.WorkflowVersionID); err != nil {
		return err
	}
	var original collab.ManagedRun
	if json.Unmarshal(task.InputJSON, &original) != nil || original.RunID != run.RunID || original.TaskID != run.TaskID || original.StoreID != run.StoreID || original.CommandID != run.CommandID || original.Workflow != run.Workflow || original.Epoch != run.Epoch {
		return collab.ErrConflict
	}
	var receipt string
	if err = s.runtime.DB().WithContext(ctx).Raw(`SELECT document FROM codeagent_submission_receipts WHERE source_store_id=? AND command_id=? AND flux_task_id=?`, run.StoreID, run.CommandID, id).Row().Scan(&receipt); err != nil {
		return err
	}
	var bound collab.SubmissionReceipt
	if json.Unmarshal([]byte(receipt), &bound) != nil || bound.Workflow != run.Workflow || bound.FluxTaskID != run.FluxTaskID || bound.PayloadHash != collab.PayloadHash(task.InputJSON) {
		return collab.ErrConflict
	}
	if task.Status == domain.TaskFailed || task.Status == domain.TaskCanceled || task.Status == domain.TaskSuccess {
		return collab.ErrRecoveryRequired
	}
	bindings, err := query.NewAwaitBindingRepository(s.runtime.DB()).ListByTaskID(ctx, id)
	if err != nil {
		return err
	}
	waiting := false
	for _, binding := range bindings {
		if binding.Status == domain.AwaitBindingWaiting {
			if binding.Provider == nil || (*binding.Provider != TurnProvider && *binding.Provider != ReviewProvider) || binding.WorkflowVersionID != task.WorkflowVersionID {
				return collab.ErrConflict
			}
			waiting = true
		}
	}
	if waiting {
		// Re-enqueuing an unsatisfied Await restores no runnable node in the
		// pinned engine and produces a DAG deadlock. Preserve suspension; only
		// typed persisted completion may queue the continuation.
		return s.runtime.DB().WithContext(ctx).Table("tasks").Where("id=?", id).Updates(map[string]any{"status": string(domain.TaskSuspended), "worker_id": ""}).Error
	}
	// The exclusive host owns this private DB. All domain stages are idempotent
	// and the Session DB has just proved no admitted unknown execution. Flux
	// restores node outputs/pending edges; this never calls generic Retry or
	// resets a completed node/subtree/await binding.
	if task.Status != domain.TaskPending {
		if err = s.runtime.DB().WithContext(ctx).Table("tasks").Where("id=? AND status IN (?,?)", id, string(domain.TaskRunning), string(domain.TaskSuspended)).Updates(map[string]any{"status": string(domain.TaskPending), "worker_id": ""}).Error; err != nil {
			return err
		}
	}
	return s.runtime.Engine().TaskRepo().Enqueue(ctx, id)
}

func (s *Service) monitor(ctx context.Context) {
	defer close(s.done)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			runs, err := s.recovery.ActiveRuns(ctx, s.opts.Actor)
			if err != nil {
				s.ready.Store(false)
				return
			}
			for _, run := range runs {
				if run.State == "prepared" {
					if err = s.deliver(ctx, run.RunID); err != nil && !errors.Is(err, context.Canceled) {
						_ = s.recovery.FenceRun(ctx, s.opts.Actor, run.RunID, "submission_requires_reconciliation")
					}
					continue
				}
				if run.State == "waiting" || run.State == "recovery_required" || run.FluxTaskID == "" {
					continue
				}
				id, err := strconv.ParseInt(run.FluxTaskID, 10, 64)
				if err != nil {
					continue
				}
				task, err := s.runtime.Status(ctx, id)
				if err != nil {
					continue
				}
				if task.Status == domain.TaskFailed || task.Status == domain.TaskCanceled || task.Status == domain.TaskSuccess {
					fresh, e := s.managed.Run(ctx, s.opts.Actor, run.RunID)
					if e == nil && fresh.State != "completed" && fresh.State != "waiting" && fresh.State != "recovery_required" {
						_ = s.recovery.FenceRun(ctx, s.opts.Actor, run.RunID, "workflow_ended_without_task_acceptance")
					}
					continue
				}
				// Flux's generic poller scans every 15s and defaults to a 3m
				// repeat. Deliver only persisted typed bindings in this private
				// domain; Flux still owns every stage/edge decision. No definition
				// or historical binding is rewritten to change its business flow.
				bindings, pollErr := query.NewAwaitBindingRepository(s.runtime.DB()).ListByTaskID(ctx, id)
				if pollErr != nil {
					continue
				}
				for _, binding := range bindings {
					if binding.Status != domain.AwaitBindingWaiting {
						continue
					}
					payload, resolveErr := s.stages.ResolveAwait(ctx, binding)
					if resolveErr != nil {
						_ = s.recovery.FenceRun(ctx, s.opts.Actor, run.RunID, "await_requires_reconciliation")
						break
					}
					if payload != nil {
						result, completeErr := s.runtime.CompleteAwait(ctx, id, binding.NodeName, payload, "")
						if completeErr != nil || result.Err != nil {
							_ = s.recovery.FenceRun(ctx, s.opts.Actor, run.RunID, "await_delivery_requires_reconciliation")
						}
					}
				}
			}
		}
	}
}
