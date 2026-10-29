package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"code-agent/internal/collab"
	"code-agent/internal/workspace"
)

var _ collab.ManagedRepository = (*taskRepository)(nil)

func validWorkflow(w collab.WorkflowBinding) bool {
	id, err := strconv.ParseInt(w.VersionID, 10, 64)
	hash, hashErr := hex.DecodeString(w.Hash)
	return taskValid(w.Name, 160) && err == nil && id > 0 && strconv.FormatInt(id, 10) == w.VersionID && hashErr == nil && len(hash) == 32
}

func (r *taskRepository) PrepareRun(ctx context.Context, a collab.Principal, taskID string, in collab.PrepareRunInput) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	if !validWorkflow(in.Workflow) || in.MaxIterations < 1 || in.MaxIterations > 5 || (in.ExecutorModel != "" && !taskValid(in.ExecutorModel, 256)) {
		return collab.Receipt{}, collab.ErrInvalid
	}
	if in.ReviewPolicy != nil {
		if err := collab.ValidateReviewPolicy(*in.ReviewPolicy); err != nil {
			return collab.Receipt{}, err
		}
	}
	identity, err := r.Identity(ctx)
	if err != nil {
		return collab.Receipt{}, err
	}
	return r.mutate(ctx, a, taskID+"/run", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		t, err := loadCodingTask(ctx, tx, a, taskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if t.ManagedRun != nil || t.State == "completed" || t.State == "cancelled" || t.State == "failed" {
			return collab.Receipt{}, collab.ErrConflict
		}
		executor, err := taskDoc[collab.TaskParticipation](ctx, tx, `SELECT document FROM coding_task_participations WHERE id=? AND task_id=?`, in.ExecutorParticipationID, taskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if executor.Role != "executor" || executor.SessionRef.Kind != "local" || executor.ObservationLevel != "managed" || executor.SessionRef.ServerID != identity.ServerID || executor.SessionRef.StoreID != identity.StoreID {
			return collab.Receipt{}, collab.ErrForbidden
		}
		reviewer, err := taskDoc[collab.TaskParticipation](ctx, tx, `SELECT document FROM coding_task_participations WHERE id=? AND task_id=?`, in.ReviewerParticipationID, taskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if reviewer.Role != "reviewer" || reviewer.ObservationLevel != "reported" {
			return collab.Receipt{}, collab.ErrForbidden
		}
		plan, err := taskDoc[collab.TaskArtifactRef](ctx, tx, `SELECT document FROM coding_task_artifacts WHERE id=? AND task_id=?`, in.PlanArtifactID, taskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if plan.Kind != "plan" || plan.CaptureMode != "reported" || plan.ContentHash == nil || plan.Truncated || *plan.ContentHash != collab.PayloadHash(plan.Content) {
			return collab.Receipt{}, collab.ErrInvalid
		}
		workspace, err := taskWorkspace(ctx, tx, executor.SessionRef.SessionID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if workspace != t.WorkspaceRef {
			return collab.Receipt{}, collab.ErrForbidden
		}
		var path string
		if err = tx.QueryRowContext(ctx, `SELECT workspace_path FROM sessions WHERE id=?`, executor.SessionRef.SessionID).Scan(&path); err != nil {
			return collab.Receipt{}, err
		}
		anchor, err := collab.AnchorWorkspace(path)
		if err != nil {
			return collab.Receipt{}, err
		}
		if err = rejectWorkspaceOverlap(ctx, tx, anchor.Root); err != nil {
			return collab.Receipt{}, err
		}
		if in.ReviewPolicy != nil {
			if err = rejectUnprovenWorkspaceProcesses(ctx, tx, anchor.Root); err != nil {
				return collab.Receipt{}, err
			}
		}
		// A durable accepted/running inbox from an interrupted ordinary turn is
		// not evidence that the checkout is idle, even when no live lease exists.
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT s.workspace_path FROM turn_inputs i JOIN sessions s ON s.id=i.session_id WHERE i.state IN ('accepted','queued','running')`)
		if err != nil {
			return collab.Receipt{}, err
		}
		var paths []string
		for rows.Next() {
			var p string
			if err = rows.Scan(&p); err != nil {
				rows.Close()
				return collab.Receipt{}, err
			}
			paths = append(paths, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return collab.Receipt{}, err
		}
		for _, p := range paths {
			if overlap(anchor.Root, canonicalExecutionPath(p)) {
				return collab.Receipt{}, collab.ErrManagedWorkspace
			}
		}
		run := collab.ManagedRun{RunID: taskIDValue("run_"), TaskID: taskID, ServerID: identity.ServerID, StoreID: identity.StoreID, Epoch: "1", State: "prepared", Workspace: anchor,
			PlanArtifactID: plan.ArtifactID, PlanVersion: plan.Version, PlanHash: *plan.ContentHash, ExecutorParticipationID: executor.ParticipationID, ReviewerParticipationID: reviewer.ParticipationID, MaxIterations: in.MaxIterations, Workflow: in.Workflow, CommandID: taskIDValue("command_")}
		run.ExecutorModel = in.ExecutorModel
		if in.ReviewPolicy != nil {
			image, err := collab.CaptureWorkspace(ctx, anchor, *in.ReviewPolicy)
			if err != nil {
				return collab.Receipt{}, err
			}
			binding, err := putReviewBlob(ctx, tx, run, "baseline", image)
			if err != nil {
				return collab.Receipt{}, err
			}
			run.ReviewPolicy, run.Baseline = in.ReviewPolicy, &binding
		}
		payload, err := json.Marshal(run)
		if err != nil {
			return collab.Receipt{}, err
		}
		command := collab.ManagedCommand{CommandID: run.CommandID, StoreID: identity.StoreID, Kind: "submit_workflow", RunID: run.RunID, Payload: payload, PayloadHash: collab.PayloadHash(payload)}
		if err = taskPut(ctx, tx, `INSERT INTO coding_task_runs(document,id,task_id) VALUES(?,?,?)`, run, run.RunID, taskID); err != nil {
			return collab.Receipt{}, err
		}
		if err = taskPut(ctx, tx, `INSERT INTO coding_task_commands(document,id,task_id,run_id) VALUES(?,?,?,?)`, command, command.CommandID, taskID, run.RunID); err != nil {
			return collab.Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO coding_task_workspace_claims(root,task_id,run_id) VALUES(?,?,?)`, anchor.Root, taskID, run.RunID); err != nil {
			return collab.Receipt{}, err
		}
		t.ManagedRun, t.State, t.Phase, t.WaitReason = &run, "waiting", "implementing", "workflow_submission"
		if run.ReviewPolicy != nil {
			t.Mode = "managed_collaboration"
		}
		err = emitTask(ctx, tx, &t, "run.prepared", executor.ParticipationID, "", collab.SourceRef{Kind: "task_request", RequestID: in.RequestID}, run)
		return collab.Receipt{ID: run.RunID, TaskID: taskID}, err
	})
}

// Keep the generator distinct from taskID parameters in domain methods.
func taskIDValue(prefix string) string { return taskID(prefix) }

func (r *taskRepository) Run(ctx context.Context, a collab.Principal, id string) (collab.ManagedRun, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return collab.ManagedRun{}, err
	}
	defer tx.Rollback()
	run, err := loadOwnedRun(ctx, tx, a, id)
	if err != nil {
		return run, err
	}
	return run, tx.Commit()
}

func loadOwnedRun(ctx context.Context, tx *sql.Tx, a collab.Principal, id string) (collab.ManagedRun, error) {
	return taskDoc[collab.ManagedRun](ctx, tx, `SELECT r.document FROM coding_task_runs r JOIN coding_tasks t ON t.id=r.task_id WHERE r.id=? AND t.owner=?`, id, a.ID)
}

func (r *taskRepository) PendingCommands(ctx context.Context, a collab.Principal, limit int) ([]collab.ManagedCommand, error) {
	if !a.LocalSessions {
		return nil, collab.ErrForbidden
	}
	if limit < 1 || limit > 200 {
		return nil, collab.ErrInvalid
	}
	rows, err := r.store.db.QueryContext(ctx, `SELECT c.document FROM coding_task_commands c JOIN coding_tasks t ON c.task_id=t.id WHERE t.owner=? AND c.receipt IS NULL ORDER BY c.rowid LIMIT ?`, a.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []collab.ManagedCommand{}
	for rows.Next() {
		var raw string
		var c collab.ManagedCommand
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *taskRepository) AcknowledgeSubmission(ctx context.Context, a collab.Principal, receipt collab.SubmissionReceipt) error {
	if !a.LocalSessions {
		return collab.ErrForbidden
	}
	id, err := strconv.ParseInt(receipt.FluxTaskID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != receipt.FluxTaskID {
		return collab.ErrInvalid
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := taskDoc[collab.ManagedCommand](ctx, tx, `SELECT c.document FROM coding_task_commands c JOIN coding_tasks t ON t.id=c.task_id WHERE c.id=? AND t.owner=?`, receipt.CommandID, a.ID)
	if err != nil {
		return err
	}
	run, err := loadOwnedRun(ctx, tx, a, c.RunID)
	if err != nil {
		return err
	}
	if run.ServerID != r.serverID || receipt.StoreID != run.StoreID || receipt.PayloadHash != c.PayloadHash || receipt.Workflow != run.Workflow {
		return collab.ErrConflict
	}
	var previous sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT receipt FROM coding_task_commands WHERE id=?`, c.CommandID).Scan(&previous); err != nil {
		return err
	}
	raw, err := taskJSON(receipt)
	if err != nil {
		return err
	}
	if previous.Valid {
		if previous.String != raw {
			return collab.ErrConflict
		}
		return tx.Commit()
	}
	if run.State != "prepared" {
		return collab.ErrRecoveryRequired
	}
	run.State, run.FluxTaskID = "submitted", receipt.FluxTaskID
	if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, run.RunID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE coding_task_commands SET receipt=? WHERE id=?`, raw, c.CommandID); err != nil {
		return err
	}
	t, err := loadCodingTask(ctx, tx, a, run.TaskID)
	if err != nil {
		return err
	}
	t.ManagedRun, t.WorkflowRef, t.WaitReason = &run, &run.FluxTaskID, "managed_dispatch"
	if err = emitTask(ctx, tx, &t, "workflow.submitted", "", "", collab.SourceRef{Kind: "command", RequestID: c.CommandID}, receipt); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *taskRepository) ReserveDispatch(ctx context.Context, a collab.Principal, id string, in collab.ReserveDispatchInput) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	if in.Iteration < 1 || in.Iteration > 5 {
		return collab.Receipt{}, collab.ErrInvalid
	}
	if !taskValid(in.Message, 65536) || (in.Model != "" && !taskValid(in.Model, 256)) {
		return collab.Receipt{}, collab.ErrInvalid
	}
	return r.mutate(ctx, a, id+"/dispatch", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		run, err := loadOwnedRun(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		if run.ServerID != r.serverID || run.Epoch != in.Epoch {
			return collab.Receipt{}, collab.ErrConflict
		}
		selectedModel := in.Model
		if run.ExecutorModel != "" {
			if in.Model != "" && in.Model != run.ExecutorModel {
				return collab.Receipt{}, collab.ErrConflict
			}
			selectedModel = run.ExecutorModel
		}
		if err = authorizeIteration(ctx, tx, run, in); err != nil {
			return collab.Receipt{}, err
		}
		p, err := taskDoc[collab.TaskParticipation](ctx, tx, `SELECT document FROM coding_task_participations WHERE id=? AND task_id=?`, run.ExecutorParticipationID, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		d := collab.ManagedDispatch{Ref: collab.ManagedTurnRef{ServerID: run.ServerID, StoreID: run.StoreID, TaskID: run.TaskID, RunID: id, Epoch: run.Epoch, DispatchID: taskID("dispatch_")}, ParticipationID: p.ParticipationID, SessionID: p.SessionRef.SessionID, RequestID: taskID("managed_"), Iteration: in.Iteration, Message: in.Message, Model: in.Model}
		d.Model = selectedModel
		if err = run.Workspace.Check(run.Workspace.Root); err != nil {
			return collab.Receipt{}, err
		}
		if run.ReviewPolicy != nil {
			image, err := collab.CaptureWorkspace(ctx, run.Workspace, *run.ReviewPolicy)
			if err != nil {
				return collab.Receipt{}, err
			}
			d.ExpectedWorkspaceHash = image.Hash()
		}
		if err = taskPut(ctx, tx, `INSERT INTO coding_task_dispatches(document,id,task_id,run_id,session_id,request_id,iteration) VALUES(?,?,?,?,?,?,?)`, d, d.Ref.DispatchID, run.TaskID, id, d.SessionID, d.RequestID, d.Iteration); err != nil {
			return collab.Receipt{}, err
		}
		t, err := loadCodingTask(ctx, tx, a, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		run.CurrentIteration, run.State, run.CurrentBundle = in.Iteration, "submitted", nil
		if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, run.RunID); err != nil {
			return collab.Receipt{}, err
		}
		t.ManagedRun, t.State, t.WaitReason = &run, "waiting", "managed_dispatch"
		if in.Iteration > 1 {
			t.Phase = "reworking"
		}
		err = emitTask(ctx, tx, &t, "dispatch.reserved", p.ParticipationID, "", collab.SourceRef{Kind: "task_request", RequestID: in.RequestID}, d)
		return collab.Receipt{ID: d.Ref.DispatchID, TaskID: run.TaskID}, err
	})
}

func (r *taskRepository) Dispatch(ctx context.Context, a collab.Principal, ref collab.ManagedTurnRef) (collab.ManagedDispatch, error) {
	if !a.LocalSessions {
		return collab.ManagedDispatch{}, collab.ErrForbidden
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return collab.ManagedDispatch{}, err
	}
	defer tx.Rollback()
	run, err := loadOwnedRun(ctx, tx, a, ref.RunID)
	if err != nil {
		return collab.ManagedDispatch{}, err
	}
	if ref.ServerID != r.serverID || run.ServerID != ref.ServerID || run.StoreID != ref.StoreID || run.Epoch != ref.Epoch || run.TaskID != ref.TaskID {
		return collab.ManagedDispatch{}, collab.ErrConflict
	}
	d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE id=? AND run_id=?`, ref.DispatchID, ref.RunID)
	if err != nil {
		return d, err
	}
	if d.Ref != ref {
		return d, collab.ErrConflict
	}
	return d, tx.Commit()
}

func overlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	inside := func(root, path string) bool {
		rel, err := filepath.Rel(root, path)
		return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	if inside(a, b) || inside(b, a) {
		return true
	}
	// Filesystem identity also catches differently-cased aliases on macOS,
	// without treating distinct paths as equal on case-sensitive volumes.
	sameAncestor := func(root, path string) bool {
		r, err := os.Stat(root)
		if err != nil {
			return false
		}
		for {
			if p, err := os.Stat(path); err == nil && os.SameFile(r, p) {
				return true
			}
			parent := filepath.Dir(path)
			if parent == path {
				return false
			}
			path = parent
		}
	}
	return sameAncestor(a, b) || sameAncestor(b, a)
}

func canonicalExecutionPath(path string) string {
	if path == "" {
		return ""
	}
	if p, err := workspace.CanonicalPath(path); err == nil {
		return filepath.Clean(p)
	}
	p, _ := filepath.Abs(path)
	return filepath.Clean(p)
}

func rejectWorkspaceOverlap(ctx context.Context, tx *sql.Tx, root string) error {
	rows, err := tx.QueryContext(ctx, `SELECT root FROM coding_task_workspace_claims UNION ALL SELECT root FROM coding_task_execution_leases`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			return err
		}
		if overlap(root, p) {
			return collab.ErrManagedWorkspace
		}
	}
	return rows.Err()
}

func managedRefForInput(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, sessionID, requestID string) (*collab.ManagedTurnRef, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT document FROM coding_task_dispatches WHERE session_id=? AND request_id=?`, sessionID, requestID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "no such table: coding_task_dispatches") {
			var version string
			versionErr := q.QueryRowContext(ctx, `SELECT value FROM coding_task_meta WHERE key='schema_version'`).Scan(&version)
			if version == "1" || (versionErr != nil && strings.Contains(versionErr.Error(), "no such table: coding_task_meta")) {
				return nil, nil
			}
		}
		return nil, err
	}
	var d collab.ManagedDispatch
	if err = json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	return &d.Ref, nil
}
