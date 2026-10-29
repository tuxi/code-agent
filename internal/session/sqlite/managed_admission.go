package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

var _ collab.ExecutionGuard = (*Store)(nil)

func (s *Store) AcquireTaskExecution(ctx context.Context, sessionID, path, requestID string, ref *collab.ManagedTurnRef) (func(), error) {
	r := &taskRepository{store: s}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	root := canonicalExecutionPath(path)
	if err = checkManagedAdmission(ctx, tx, sessionID, requestID, ref, root, false); err != nil {
		return nil, err
	}
	// Ordinary turns remain concurrent as before. Their short-lived leases only
	// prevent a Run from claiming a checkout between guard and durable acceptance.
	id, runID := taskID("lease_"), ""
	if ref != nil {
		runID = ref.RunID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO coding_task_execution_leases(id,root,session_id,request_id,run_id) VALUES(?,?,?,?,?)`, id, root, sessionID, requestID, runID); err != nil {
		return nil, taskConstraint(err)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { _, _ = s.db.Exec(`DELETE FROM coding_task_execution_leases WHERE id=?`, id) })
	}, nil
}

func checkManagedAdmission(ctx context.Context, tx *sql.Tx, sessionID, requestID string, ref *collab.ManagedTurnRef, root string, requireLease bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT root,run_id FROM coding_task_workspace_claims`)
	if err != nil {
		return err
	}
	var owningRun string
	for rows.Next() {
		var p, id string
		if err = rows.Scan(&p, &id); err != nil {
			rows.Close()
			return err
		}
		if overlap(root, p) {
			if ref == nil || id != ref.RunID {
				rows.Close()
				return collab.ErrManagedWorkspace
			}
			owningRun = id
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if ref == nil {
		var bound bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coding_task_workspace_claims c JOIN coding_task_runs r ON r.id=c.run_id JOIN coding_task_participations p ON p.id=json_extract(r.document,'$.executor_participation_id') WHERE json_extract(p.document,'$.session_ref.session_id')=?)`, sessionID).Scan(&bound); err != nil {
			return err
		}
		if bound {
			return collab.ErrManagedWorkspace
		}
		// A request id from a managed reservation is never a plain submission,
		// even if a stale caller attempts to omit its source tag.
		stored, err := managedRefForInput(ctx, tx, sessionID, requestID)
		if err != nil {
			return err
		}
		if stored != nil {
			return collab.ErrManagedWorkspace
		}
		return nil
	}
	if owningRun != ref.RunID {
		return collab.ErrManagedWorkspace
	}
	run, err := taskDoc[collab.ManagedRun](ctx, tx, `SELECT document FROM coding_task_runs WHERE id=?`, ref.RunID)
	if err != nil {
		return err
	}
	if run.ServerID != ref.ServerID || run.StoreID != ref.StoreID || run.TaskID != ref.TaskID || run.Epoch != ref.Epoch {
		return collab.ErrConflict
	}
	if run.State != "submitted" {
		return collab.ErrRecoveryRequired
	}
	if err = run.Workspace.Check(root); err != nil {
		return err
	}
	d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE id=? AND run_id=?`, ref.DispatchID, ref.RunID)
	if err != nil {
		return err
	}
	if d.Ref != *ref || d.SessionID != sessionID || d.RequestID != requestID {
		return collab.ErrConflict
	}
	if d.Iteration != run.CurrentIteration {
		return collab.ErrConflict
	}
	workspace, err := taskWorkspace(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	var taskWorkspaceRef string
	if err = tx.QueryRowContext(ctx, `SELECT workspace_ref FROM coding_tasks WHERE id=?`, ref.TaskID).Scan(&taskWorkspaceRef); err != nil {
		return err
	}
	if workspace != taskWorkspaceRef {
		return collab.ErrManagedWorkspace
	}
	if requireLease {
		var held bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coding_task_execution_leases WHERE run_id=? AND session_id=? AND request_id=?)`, ref.RunID, sessionID, requestID).Scan(&held); err != nil {
			return err
		}
		if !held {
			return collab.ErrManagedWorkspace
		}
	}
	return nil
}

func checkTurnInputAdmission(ctx context.Context, tx *sql.Tx, input session.TurnInput) error {
	var path string
	err := tx.QueryRowContext(ctx, `SELECT workspace_path FROM sessions WHERE id=?`, input.SessionID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) && input.ManagedTask == nil {
		return nil
	}
	if err != nil {
		return err
	}
	return checkManagedAdmission(ctx, tx, input.SessionID, input.RequestID, input.ManagedTask, canonicalExecutionPath(path), input.ManagedTask != nil)
}

func bindManagedAttempt(ctx context.Context, tx *sql.Tx, input session.TurnInput) error {
	if input.ManagedTask == nil {
		return nil
	}
	ref := *input.ManagedTask
	d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE id=?`, ref.DispatchID)
	if err != nil {
		return err
	}
	if input.Provenance != nil || input.Text != d.Message || (d.Model != "" && input.WireModel != d.Model) || len(input.Assets) > 0 || len(input.LocalAssets) > 0 {
		return collab.ErrConflict
	}
	if d.TurnID != "" {
		return collab.ErrConflict
	}
	attempt := collab.TaskAttempt{AttemptID: taskID("attempt_"), TaskID: ref.TaskID, ParticipationID: d.ParticipationID, Stage: "implementing", Iteration: d.Iteration, Status: "queued",
		SourceRef: collab.SourceRef{Kind: "session_event", ServerID: ref.ServerID, StoreID: ref.StoreID, SessionID: input.SessionID, TurnID: input.TurnID, RequestID: input.RequestID}}
	if d.Iteration > 1 {
		attempt.Stage = "reworking"
	}
	if err = taskPut(ctx, tx, `INSERT INTO coding_task_attempts(document,id,task_id,participation_id,session_id,turn_id) VALUES(?,?,?,?,?,?)`, attempt, attempt.AttemptID, ref.TaskID, d.ParticipationID, input.SessionID, input.TurnID); err != nil {
		return taskConstraint(err)
	}
	d.TurnID, d.AttemptID = input.TurnID, attempt.AttemptID
	if err = taskPut(ctx, tx, `UPDATE coding_task_dispatches SET document=?,turn_id=? WHERE id=?`, d, input.TurnID, ref.DispatchID); err != nil {
		return err
	}
	t, err := taskDoc[collab.CodingTask](ctx, tx, `SELECT document FROM coding_tasks WHERE id=?`, ref.TaskID)
	if err != nil {
		return err
	}
	t.CurrentAttemptID, t.State, t.Phase, t.WaitReason = attempt.AttemptID, "active", attempt.Stage, ""
	return emitTask(ctx, tx, &t, "attempt.linked", d.ParticipationID, attempt.AttemptID, attempt.SourceRef, map[string]string{"run_id": ref.RunID, "control_epoch": ref.Epoch, "status": "queued"})
}

func (s *Store) RequireTaskRecovery(ctx context.Context, sessionID, requestID string) error {
	r := &taskRepository{store: s}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ref, err := managedRefForInput(ctx, tx, sessionID, requestID)
	if err != nil {
		return err
	}
	if ref == nil {
		return tx.Commit()
	}
	run, err := taskDoc[collab.ManagedRun](ctx, tx, `SELECT document FROM coding_task_runs WHERE id=?`, ref.RunID)
	if err != nil {
		return err
	}
	if run.State == "recovery_required" {
		return tx.Commit()
	}
	if run.Epoch != ref.Epoch || run.State == "completed" {
		return collab.ErrConflict
	}
	d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE id=? AND run_id=?`, ref.DispatchID, ref.RunID)
	if err != nil {
		return err
	}
	if run.CurrentIteration > 0 && d.Iteration != run.CurrentIteration {
		return collab.ErrConflict
	}
	run.State = "recovery_required"
	if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, run.RunID); err != nil {
		return err
	}
	t, err := taskDoc[collab.CodingTask](ctx, tx, `SELECT document FROM coding_tasks WHERE id=?`, run.TaskID)
	if err != nil {
		return err
	}
	t.ManagedRun, t.State, t.WaitReason = &run, "waiting", "recovery_required"
	if err = emitTask(ctx, tx, &t, "run.recovery_required", "", "", collab.SourceRef{Kind: "recovery", SessionID: sessionID, RequestID: requestID}, map[string]string{"run_id": run.RunID}); err != nil {
		return err
	}
	return tx.Commit()
}
