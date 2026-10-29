package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"code-agent/internal/collab"
)

var _ collab.RecoveryRepository = (*taskRepository)(nil)

func (r *taskRepository) ActiveRuns(ctx context.Context, a collab.Principal) ([]collab.ManagedRun, error) {
	if !a.LocalSessions {
		return nil, collab.ErrForbidden
	}
	rows, err := r.store.db.QueryContext(ctx, `SELECT r.document FROM coding_task_runs r JOIN coding_tasks t ON t.id=r.task_id WHERE r.active=1 AND t.owner=? ORDER BY r.rowid LIMIT 513`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []collab.ManagedRun{}
	for rows.Next() {
		var raw []byte
		var run collab.ManagedRun
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
		if len(runs) > 512 {
			return nil, collab.ErrUnavailable
		}
	}
	return runs, rows.Err()
}

// Until background/client/subagent process ownership is durably reconciled,
// managed v1 refuses such workspace history, even if it looks finished. It
// cannot infer OS process death from a best-effort bracket event. Manual
// collaboration keeps its existing behavior.
func rejectUnprovenWorkspaceProcesses(ctx context.Context, tx *sql.Tx, root string) error {
	paths, err := tx.QueryContext(ctx, `SELECT id,workspace_path FROM sessions`)
	if err != nil {
		return err
	}
	ids := []string{}
	for paths.Next() {
		var id, path string
		if err = paths.Scan(&id, &path); err != nil {
			paths.Close()
			return err
		}
		if overlap(root, canonicalExecutionPath(path)) {
			ids = append(ids, id)
		}
	}
	err = paths.Err()
	paths.Close()
	if err != nil {
		return err
	}
	// Orphaned bracket partitions also block managed execution: deleting a
	// Session does not prove its detached process stopped.
	var orphan bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_events e LEFT JOIN sessions s ON s.id=e.session_id WHERE s.id IS NULL AND e.kind IN ('job_started','task_started'))`).Scan(&orphan); err != nil {
		return err
	}
	if orphan {
		return collab.ErrRecoveryRequired
	}
	for _, id := range ids {
		if err = rejectUnprovenSessionProcesses(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnprovenSessionProcesses(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := tx.QueryContext(ctx, `SELECT kind,payload FROM session_events WHERE session_id=? AND kind IN ('job_started','task_started','tool_started','tool_finished') ORDER BY id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	bytes := 0
	calls := 0
	for rows.Next() {
		var kind string
		var raw []byte
		if err = rows.Scan(&kind, &raw); err != nil {
			return err
		}
		bytes += len(raw)
		calls++
		if bytes > 16<<20 || calls > 30000 {
			return collab.ErrRecoveryRequired
		}
		if kind == "job_started" || kind == "task_started" {
			return collab.ErrRecoveryRequired
		}
		var e struct {
			ToolName, ToolArgs, Observation, Executor string
			Output                                    json.RawMessage
		}
		if json.Unmarshal(raw, &e) != nil {
			return collab.ErrRecoveryRequired
		}
		if e.Executor == "client" || e.ToolName == "task" || e.ToolName == "plan_workflow" {
			return collab.ErrRecoveryRequired
		}
		var input struct {
			Background bool `json:"background"`
		}
		_ = json.Unmarshal([]byte(e.ToolArgs), &input)
		var output struct {
			JobID string `json:"job_id"`
		}
		_ = json.Unmarshal(e.Output, &output)
		if output.JobID != "" || (e.ToolName == "run_command" && input.Background) || strings.Contains(e.Observation, `"job_id":`) {
			return collab.ErrRecoveryRequired
		}
	}
	return rows.Err()
}

func fenceRun(ctx context.Context, tx *sql.Tx, a collab.Principal, run *collab.ManagedRun, reason string) error {
	if run.State == "completed" {
		return collab.ErrConflict
	}
	if run.State == "recovery_required" && run.RecoveryReason != "" {
		return nil
	}
	epoch, err := strconv.ParseUint(run.Epoch, 10, 64)
	if err != nil || epoch == ^uint64(0) {
		return collab.ErrConflict
	}
	previous := run.Epoch
	run.Epoch = strconv.FormatUint(epoch+1, 10)
	run.State = "recovery_required"
	run.RecoveryReason = reason
	t, err := loadCodingTask(ctx, tx, a, run.TaskID)
	if err != nil {
		return err
	}
	t.ManagedRun = run
	t.State = "waiting"
	t.WaitReason = "recovery_required"
	if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, run.RunID); err != nil {
		return err
	}
	return emitTask(ctx, tx, &t, "run.recovery_fenced", "", "", collab.SourceRef{Kind: "recovery"}, map[string]string{"reason": reason, "previous_epoch": previous, "control_epoch": run.Epoch, "approval": "invalidated"})
}

func (r *taskRepository) FenceRun(ctx context.Context, a collab.Principal, id, reason string) error {
	if !a.LocalSessions {
		return collab.ErrForbidden
	}
	if !taskValid(reason, 160) {
		return collab.ErrInvalid
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	run, err := loadOwnedRun(ctx, tx, a, id)
	if err != nil {
		return err
	}
	if run.ServerID != r.serverID {
		return collab.ErrConflict
	}
	if err = fenceRun(ctx, tx, a, &run, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileRun is called under the exclusive managed-host lock, before workers
// or public managed capabilities exist. It records ambiguity, never clears
// stale leases or restarts an admitted Turn.
func (r *taskRepository) ReconcileRun(ctx context.Context, a collab.Principal, id string) (collab.RecoveryCheck, error) {
	var out collab.RecoveryCheck
	if !a.LocalSessions {
		return out, collab.ErrForbidden
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	run, err := loadOwnedRun(ctx, tx, a, id)
	if err != nil {
		return out, err
	}
	if run.State == "completed" || run.State == "waiting" {
		out = collab.RecoveryCheck{Run: run, Safe: false, Reason: "already_completed_or_waiting_for_human"}
		return out, tx.Commit()
	}
	var storeID string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM coding_task_meta WHERE key='store_id'`).Scan(&storeID); err != nil {
		return out, err
	}
	reason := ""
	switch {
	case run.ServerID != r.serverID || run.StoreID != storeID:
		reason = "runtime_identity_changed"
	case run.ReviewPolicy == nil || run.Baseline == nil:
		reason = "legacy_run_requires_reconciliation"
	case run.State == "recovery_required":
		reason = "manual_recovery_required"
	case run.State != "prepared" && run.State != "submitted" && run.State != "reviewing" && run.State != "finalizing" && run.State != "rework_authorized":
		reason = "unknown_managed_state"
	default:
		if err = run.Workspace.Check(run.Workspace.Root); err != nil {
			reason = "workspace_identity_changed"
		}
		if reason == "" {
			if err = quiescent(ctx, tx, run); err != nil {
				if !errors.Is(err, collab.ErrRecoveryRequired) {
					return out, err
				}
				reason = "writer_lease_survived_restart"
			}
		}
		if reason == "" {
			if err = rejectUnprovenWorkspaceProcesses(ctx, tx, run.Workspace.Root); err != nil {
				if !errors.Is(err, collab.ErrRecoveryRequired) {
					return out, err
				}
				reason = "unproven_background_or_client_process"
			}
		}
		if reason == "" && run.CurrentIteration > 0 {
			d, e := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE run_id=? AND iteration=?`, id, run.CurrentIteration)
			if e != nil {
				return out, e
			}
			if d.Ref.Epoch != run.Epoch {
				reason = "stale_dispatch_epoch"
			} else if d.TurnID == "" {
				image, e := collab.CaptureWorkspace(ctx, run.Workspace, *run.ReviewPolicy)
				if e != nil || d.ExpectedWorkspaceHash == "" || image.Hash() != d.ExpectedWorkspaceHash {
					reason = "unadmitted_candidate_changed"
				}
			} else {
				o, e := turnObservation(ctx, tx, d.Ref)
				if e != nil || !o.Terminal || o.Status != "completed" {
					reason = "admitted_turn_outcome_unconfirmed"
				}
			}
		}
		if reason == "" && run.CurrentIteration == 0 {
			image, e := collab.CaptureWorkspace(ctx, run.Workspace, *run.ReviewPolicy)
			if e != nil || image.Hash() != run.Baseline.Hash {
				reason = "baseline_changed_before_dispatch"
			}
		}
	}
	if reason != "" {
		if err = fenceRun(ctx, tx, a, &run, reason); err != nil {
			return out, err
		}
	}
	out = collab.RecoveryCheck{Run: run, Safe: reason == "", Reason: reason}
	return out, tx.Commit()
}
