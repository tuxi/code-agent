package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"

	"code-agent/internal/collab"
	"code-agent/internal/executionevidence"
)

var _ collab.ReviewRepository = (*taskRepository)(nil)

func putReviewBlob(ctx context.Context, tx *sql.Tx, run collab.ManagedRun, kind string, value any) (collab.ArtifactBinding, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > collab.MaxReviewBytes {
		return collab.ArtifactBinding{}, collab.ErrInvalid
	}
	b := collab.ArtifactBinding{ArtifactID: taskID("blob_"), Version: "1", Hash: collab.PayloadHash(raw)}
	if kind == "review_bundle" {
		b.Version = strconv.Itoa(run.CurrentIteration)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO coding_task_artifact_blobs(id,task_id,run_id,kind,version,hash,document) VALUES(?,?,?,?,?,?,?)`, b.ArtifactID, run.TaskID, run.RunID, kind, b.Version, b.Hash, raw)
	return b, err
}

func reviewBlob(ctx context.Context, tx *sql.Tx, run collab.ManagedRun, b collab.ArtifactBinding, dest any) error {
	var raw []byte
	var hash, version string
	err := tx.QueryRowContext(ctx, `SELECT version,hash,document FROM coding_task_artifact_blobs WHERE id=? AND task_id=? AND run_id=?`, b.ArtifactID, run.TaskID, run.RunID).Scan(&version, &hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return collab.ErrNotFound
	}
	if err != nil {
		return err
	}
	if b.Version != version || hash != b.Hash || collab.PayloadHash(raw) != hash {
		return collab.ErrConflict
	}
	return json.Unmarshal(raw, dest)
}

func (r *taskRepository) reviewRun(ctx context.Context, tx *sql.Tx, a collab.Principal, id string) (collab.ManagedRun, error) {
	if !a.LocalSessions {
		return collab.ManagedRun{}, collab.ErrForbidden
	}
	run, err := loadOwnedRun(ctx, tx, a, id)
	if err != nil {
		return run, err
	}
	if run.ServerID != r.serverID {
		return run, collab.ErrConflict
	}
	if run.ReviewPolicy == nil || run.Baseline == nil {
		return run, collab.ErrUnavailable
	}
	return run, nil
}

func quiescent(ctx context.Context, tx *sql.Tx, run collab.ManagedRun) error {
	var busy bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coding_task_execution_leases WHERE run_id=?)`, run.RunID).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return collab.ErrRecoveryRequired
	}
	return nil
}

func (r *taskRepository) DispatchAt(ctx context.Context, a collab.Principal, id string, n int) (collab.ManagedDispatch, error) {
	if !a.LocalSessions {
		return collab.ManagedDispatch{}, collab.ErrForbidden
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return collab.ManagedDispatch{}, err
	}
	defer tx.Rollback()
	run, err := loadOwnedRun(ctx, tx, a, id)
	if err != nil {
		return collab.ManagedDispatch{}, err
	}
	if run.ServerID != r.serverID {
		return collab.ManagedDispatch{}, collab.ErrConflict
	}
	d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE run_id=? AND iteration=?`, id, n)
	if err != nil {
		return d, err
	}
	return d, tx.Commit()
}

func turnObservation(ctx context.Context, tx *sql.Tx, ref collab.ManagedTurnRef) (collab.TurnObservation, error) {
	d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE id=? AND run_id=?`, ref.DispatchID, ref.RunID)
	o := collab.TurnObservation{Source: ref, SessionID: d.SessionID, TurnID: d.TurnID, Status: "not_admitted"}
	if err != nil {
		return o, err
	}
	if d.Ref != ref {
		return o, collab.ErrConflict
	}
	if d.TurnID == "" {
		return o, nil
	}
	var turn, state, kind string
	err = tx.QueryRowContext(ctx, `SELECT turn_id,state FROM turn_inputs WHERE session_id=? AND request_id=?`, d.SessionID, d.RequestID).Scan(&turn, &state)
	if err != nil {
		return o, err
	}
	if turn != d.TurnID {
		return o, collab.ErrConflict
	}
	o.Status = state
	err = tx.QueryRowContext(ctx, `SELECT kind FROM session_events WHERE session_id=? AND turn_id=? AND kind IN ('turn_finished','turn_cancelled','turn_failed') ORDER BY id DESC LIMIT 1`, d.SessionID, d.TurnID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	terminal := map[string]string{"turn_finished": "completed", "turn_cancelled": "cancelled", "turn_failed": "failed"}[kind]
	if state != terminal {
		// The loop persists its terminal before TurnExecutor commits the inbox
		// state and releases its writer. This is a normal live drain window.
		if state == "accepted" || state == "queued" || state == "running" {
			o.Status = "finalizing"
			return o, nil
		}
		return o, collab.ErrRecoveryRequired
	}
	o.Terminal = true
	return o, nil
}

func (r *taskRepository) ObserveTurn(ctx context.Context, a collab.Principal, ref collab.ManagedTurnRef) (collab.TurnObservation, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return collab.TurnObservation{}, err
	}
	defer tx.Rollback()
	run, err := r.reviewRun(ctx, tx, a, ref.RunID)
	if err != nil {
		return collab.TurnObservation{}, err
	}
	if ref.ServerID != run.ServerID || ref.StoreID != run.StoreID || ref.TaskID != run.TaskID || ref.Epoch != run.Epoch {
		return collab.TurnObservation{}, collab.ErrConflict
	}
	o, err := turnObservation(ctx, tx, ref)
	if err != nil {
		return o, err
	}
	// A terminal inbox may precede TurnExecutor's final save/deferred lease
	// release. Keep polling this ordinary drain window instead of freezing it.
	if o.Terminal {
		if err = quiescent(ctx, tx, run); errors.Is(err, collab.ErrRecoveryRequired) {
			o.Terminal = false
			o.Status = "finalizing"
		} else if err != nil {
			return o, err
		}
	}
	return o, tx.Commit()
}

// Only exact-turn execution/lifecycle records enter the bundle. Model prompts,
// thinking and other Session history are not copied into review artifacts.
type reviewEvent struct {
	Seq                                                           string `json:"seq"`
	Kind                                                          string
	CallID, ToolName, ToolArgs, Observation, Err, Chunk, Executor string
	Output                                                        json.RawMessage
}

func executionRecords(ctx context.Context, tx *sql.Tx, d collab.ManagedDispatch) ([]json.RawMessage, []executionevidence.Event, string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,payload FROM session_events WHERE session_id=? AND turn_id=? AND kind IN ('tool_started','tool_finished','tool_stdout','tool_stderr','turn_finished','turn_failed','turn_cancelled') ORDER BY id`, d.SessionID, d.TurnID)
	if err != nil {
		return nil, nil, "", false, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	events := []executionevidence.Event{}
	size := 0
	cursor := ""
	incomplete := false
	seenCalls := map[string]bool{}
	terminalSeen := false
	for rows.Next() {
		var seq int64
		var kind string
		var raw []byte
		if err = rows.Scan(&seq, &kind, &raw); err != nil {
			return nil, nil, "", false, err
		}
		size += len(raw)
		if size > 2<<20 || len(out) >= 10000 {
			return nil, nil, "", false, collab.ErrInvalid
		}
		var e reviewEvent
		if err = json.Unmarshal(raw, &e); err != nil {
			return nil, nil, "", false, err
		}
		e.Seq = strconv.FormatInt(seq, 10)
		e.Kind = kind
		cursor = e.Seq
		if terminalSeen {
			incomplete = true
		}
		if kind == "turn_finished" || kind == "turn_failed" || kind == "turn_cancelled" {
			terminalSeen = true
		}
		// Any truncation marker, background job or client execution leaves
		// process/output completeness unproven in this first gate version.
		outputText := strings.ToLower(e.Chunk + "\n" + e.Observation + "\n" + string(e.Output))
		if strings.Contains(outputText, "[tool output truncated:") || strings.Contains(outputText, "...<truncated:") || strings.Contains(outputText, "[output truncated]") || e.Executor == "client" {
			incomplete = true
		}
		var result struct {
			JobID     string `json:"job_id"`
			Truncated bool   `json:"truncated"`
		}
		_ = json.Unmarshal(e.Output, &result)
		var input struct {
			Background bool `json:"background"`
		}
		_ = json.Unmarshal([]byte(e.ToolArgs), &input)
		if result.JobID != "" || result.Truncated || (e.Kind == "tool_started" && e.ToolName == "run_command" && input.Background) || (e.Kind == "tool_finished" && strings.Contains(e.Observation, `"job_id":`)) {
			incomplete = true
		}
		if e.Kind == "tool_started" && (e.CallID == "" || !json.Valid([]byte(e.ToolArgs))) {
			incomplete = true
		}
		if e.Kind == "tool_started" {
			if seenCalls[e.CallID] {
				incomplete = true
			}
			seenCalls[e.CallID] = true
		}
		encoded, _ := json.Marshal(e)
		out = append(out, encoded)
		events = append(events, executionevidence.Event{Kind: kind, CallID: e.CallID, ToolName: e.ToolName, Input: json.RawMessage(e.ToolArgs), Output: e.Output, Observation: e.Observation, Err: e.Err})
	}
	return out, events, cursor, incomplete, rows.Err()
}

func (r *taskRepository) CaptureBundle(ctx context.Context, a collab.Principal, ref collab.ManagedTurnRef) (collab.TaskArtifactRef, error) {
	if !a.LocalSessions {
		return collab.TaskArtifactRef{}, collab.ErrForbidden
	}
	receipt, err := r.mutate(ctx, a, ref.RunID+"/capture", ref.DispatchID, ref, func(tx *sql.Tx) (collab.Receipt, error) {
		run, err := r.reviewRun(ctx, tx, a, ref.RunID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if ref.ServerID != run.ServerID || ref.StoreID != run.StoreID || ref.TaskID != run.TaskID || ref.Epoch != run.Epoch {
			return collab.Receipt{}, collab.ErrConflict
		}
		if run.State != "submitted" {
			return collab.Receipt{}, collab.ErrRecoveryRequired
		}
		if err = quiescent(ctx, tx, run); err != nil {
			return collab.Receipt{}, err
		}
		d, err := taskDoc[collab.ManagedDispatch](ctx, tx, `SELECT document FROM coding_task_dispatches WHERE id=? AND run_id=?`, ref.DispatchID, ref.RunID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if d.Ref != ref || d.Iteration != run.CurrentIteration || d.AttemptID == "" {
			return collab.Receipt{}, collab.ErrConflict
		}
		o, err := turnObservation(ctx, tx, ref)
		if err != nil {
			return collab.Receipt{}, err
		}
		if !o.Terminal || o.Status != "completed" {
			return collab.Receipt{}, collab.ErrRecoveryRequired
		}
		var before collab.WorkspaceImage
		if err = reviewBlob(ctx, tx, run, *run.Baseline, &before); err != nil {
			return collab.Receipt{}, err
		}
		after, err := collab.CaptureWorkspace(ctx, run.Workspace, *run.ReviewPolicy)
		if err != nil {
			return collab.Receipt{}, err
		}
		if before.Head != after.Head {
			return collab.Receipt{}, collab.ErrConflict
		}
		events, evidence, cursor, incomplete, err := executionRecords(ctx, tx, d)
		if err != nil {
			return collab.Receipt{}, err
		}
		report := executionevidence.Check(evidence, executionevidence.Expectations{ValidationCommands: run.ReviewPolicy.ValidationCommands}, o.Status, false)
		if incomplete {
			report.Status = "needs_validation"
			report.Reasons = append(report.Reasons, "incomplete_output_or_unproven_process_quiescence")
		}
		patch, err := collab.ImagePatch(before.Files, after.Files)
		if err != nil {
			return collab.Receipt{}, err
		}
		indexPatch, err := collab.ImagePatch(before.Index, after.Index)
		if err != nil {
			return collab.Receipt{}, err
		}
		reportJSON, _ := json.Marshal(report)
		bundle := collab.ReviewBundle{Schema: "coding_review_bundle_v1", Source: ref, AttemptID: d.AttemptID, TurnID: d.TurnID, Iteration: d.Iteration, EventCursor: cursor, Plan: collab.ArtifactBinding{ArtifactID: run.PlanArtifactID, Version: run.PlanVersion, Hash: run.PlanHash}, Baseline: before, Candidate: after, Patch: patch, IndexPatch: indexPatch, ValidationStatus: report.Status, Evidence: reportJSON, Events: events}
		binding, err := putReviewBlob(ctx, tx, run, "review_bundle", bundle)
		if err != nil {
			return collab.Receipt{}, err
		}
		artifact := collab.TaskArtifactRef{ArtifactID: binding.ArtifactID, AttemptID: d.AttemptID, Kind: "review_bundle", Version: binding.Version, ContentHash: &binding.Hash, CaptureMode: "immutable", SourceRef: collab.SourceRef{Kind: "session_event", ServerID: run.ServerID, StoreID: run.StoreID, SessionID: d.SessionID, TurnID: d.TurnID, RequestID: d.RequestID, Seq: cursor}}
		if err = taskPut(ctx, tx, `INSERT INTO coding_task_artifacts(document,id,task_id,attempt_id,kind,version) VALUES(?,?,?,?,?,?)`, artifact, artifact.ArtifactID, run.TaskID, d.AttemptID, artifact.Kind, d.Iteration); err != nil {
			return collab.Receipt{}, err
		}
		run.CurrentBundle = &binding
		run.State = "reviewing"
		attempt, err := taskDoc[collab.TaskAttempt](ctx, tx, `SELECT document FROM coding_task_attempts WHERE id=? AND task_id=?`, d.AttemptID, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		attempt.ExecutionEvidence = reportJSON
		if err = taskPut(ctx, tx, `UPDATE coding_task_attempts SET document=? WHERE id=?`, attempt, d.AttemptID); err != nil {
			return collab.Receipt{}, err
		}
		t, err := loadCodingTask(ctx, tx, a, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		t.ManagedRun = &run
		t.State = "waiting"
		t.Phase = "reviewing"
		t.WaitReason = "review_required"
		if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, run.RunID); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "artifact.captured", d.ParticipationID, d.AttemptID, artifact.SourceRef, map[string]any{"artifact": artifact, "validation_status": report.Status})
		return collab.Receipt{ID: artifact.ArtifactID, TaskID: run.TaskID}, err
	})
	if err != nil {
		return collab.TaskArtifactRef{}, err
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return collab.TaskArtifactRef{}, err
	}
	defer tx.Rollback()
	artifact, err := taskDoc[collab.TaskArtifactRef](ctx, tx, `SELECT document FROM coding_task_artifacts WHERE id=? AND task_id=?`, receipt.ID, receipt.TaskID)
	if err != nil {
		return artifact, err
	}
	return artifact, tx.Commit()
}

func (r *taskRepository) ReadBundle(ctx context.Context, a collab.Principal, id string) (collab.ReviewBundle, error) {
	var out collab.ReviewBundle
	if !a.LocalSessions {
		return out, collab.ErrForbidden
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var runID, hash, version string
	err = tx.QueryRowContext(ctx, `SELECT b.run_id,b.hash,b.version FROM coding_task_artifact_blobs b JOIN coding_tasks t ON t.id=b.task_id WHERE b.id=? AND b.kind='review_bundle' AND t.owner=?`, id, a.ID).Scan(&runID, &hash, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return out, collab.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	run, err := r.reviewRun(ctx, tx, a, runID)
	if err != nil {
		return out, err
	}
	if err = reviewBlob(ctx, tx, run, collab.ArtifactBinding{ArtifactID: id, Version: version, Hash: hash}, &out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func validReview(in collab.ReviewInput) bool {
	if !taskValid(in.SubmissionID, 160) || !taskValid(in.RunID, 160) || !taskValid(in.Epoch, 40) || !taskValid(in.AttemptID, 160) || len(in.Findings) > 32 {
		return false
	}
	if in.Verdict != "pass" && in.Verdict != "request_changes" && in.Verdict != "inconclusive" {
		return false
	}
	if in.Verdict == "request_changes" && len(in.Findings) == 0 {
		return false
	}
	for _, s := range in.Findings {
		if !taskValid(s, 4096) {
			return false
		}
	}
	return true
}

func (r *taskRepository) SubmitReview(ctx context.Context, a collab.Principal, in collab.ReviewInput) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	if !validReview(in) {
		return collab.Receipt{}, collab.ErrInvalid
	}
	return r.mutate(ctx, a, in.RunID+"/review", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		run, err := r.reviewRun(ctx, tx, a, in.RunID)
		if err != nil {
			return collab.Receipt{}, err
		}
		// Producer deduplication is independent of the transport request id.
		old, err := taskDoc[collab.GateDecision](ctx, tx, `SELECT document FROM coding_task_reviews WHERE run_id=? AND producer=?`, run.RunID, in.SubmissionID)
		if err == nil {
			a, b := old.Review, in
			a.RequestID = ""
			b.RequestID = ""
			if !reflect.DeepEqual(a, b) {
				return collab.Receipt{}, collab.ErrConflict
			}
			return collab.Receipt{ID: old.DecisionID, TaskID: run.TaskID}, nil
		}
		if !errors.Is(err, collab.ErrNotFound) {
			return collab.Receipt{}, err
		}
		if in.ReviewerParticipationID != run.ReviewerParticipationID {
			return collab.Receipt{}, collab.ErrForbidden
		}
		if run.State != "reviewing" || in.Epoch != run.Epoch || run.CurrentBundle == nil || in.Artifact != *run.CurrentBundle || in.Plan != (collab.ArtifactBinding{ArtifactID: run.PlanArtifactID, Version: run.PlanVersion, Hash: run.PlanHash}) {
			return collab.Receipt{}, collab.ErrConflict
		}
		var bundle collab.ReviewBundle
		if err = reviewBlob(ctx, tx, run, in.Artifact, &bundle); err != nil {
			return collab.Receipt{}, err
		}
		if bundle.AttemptID != in.AttemptID || bundle.Iteration != run.CurrentIteration || bundle.Source.Epoch != run.Epoch {
			return collab.Receipt{}, collab.ErrConflict
		}
		decision := collab.GateDecision{DecisionID: taskID("review_"), Review: in, Iteration: bundle.Iteration, Action: "pending"}
		if err = taskPut(ctx, tx, `INSERT INTO coding_task_reviews(document,id,task_id,run_id,iteration,producer) VALUES(?,?,?,?,?,?)`, decision, decision.DecisionID, run.TaskID, run.RunID, decision.Iteration, in.SubmissionID); err != nil {
			return collab.Receipt{}, err
		}
		t, err := loadCodingTask(ctx, tx, a, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "review.recorded", in.ReviewerParticipationID, in.AttemptID, collab.SourceRef{Kind: "reported", ProducerSubmissionID: in.SubmissionID}, decision)
		return collab.Receipt{ID: decision.DecisionID, TaskID: run.TaskID}, err
	})
}

func loadDecision(ctx context.Context, tx *sql.Tx, runID string, n int) (collab.GateDecision, error) {
	d, err := taskDoc[collab.GateDecision](ctx, tx, `SELECT document FROM coding_task_reviews WHERE run_id=? AND iteration=?`, runID, n)
	if err != nil {
		return d, err
	}
	gate, err := taskDoc[collab.GateDecision](ctx, tx, `SELECT document FROM coding_task_gate_decisions WHERE id=? AND run_id=?`, d.DecisionID, runID)
	if errors.Is(err, collab.ErrNotFound) {
		return d, nil
	}
	return gate, err
}

func (r *taskRepository) Decision(ctx context.Context, a collab.Principal, id string, n int) (collab.GateDecision, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return collab.GateDecision{}, err
	}
	defer tx.Rollback()
	if _, err = r.reviewRun(ctx, tx, a, id); err != nil {
		return collab.GateDecision{}, err
	}
	d, err := loadDecision(ctx, tx, id, n)
	if err != nil {
		return d, err
	}
	return d, tx.Commit()
}

func currentCandidate(ctx context.Context, tx *sql.Tx, run collab.ManagedRun) (collab.ReviewBundle, error) {
	var b collab.ReviewBundle
	if run.CurrentBundle == nil {
		return b, collab.ErrConflict
	}
	if err := reviewBlob(ctx, tx, run, *run.CurrentBundle, &b); err != nil {
		return b, err
	}
	if b.Source.Epoch != run.Epoch || b.Iteration != run.CurrentIteration || b.Plan != (collab.ArtifactBinding{ArtifactID: run.PlanArtifactID, Version: run.PlanVersion, Hash: run.PlanHash}) {
		return b, collab.ErrConflict
	}
	if err := quiescent(ctx, tx, run); err != nil {
		return b, err
	}
	image, err := collab.CaptureWorkspace(ctx, run.Workspace, *run.ReviewPolicy)
	if err != nil {
		return b, err
	}
	if image.Hash() != b.Candidate.Hash() {
		return b, collab.ErrConflict
	}
	return b, nil
}

func (r *taskRepository) ApplyGate(ctx context.Context, a collab.Principal, id, decisionID string) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	return r.mutate(ctx, a, id+"/gate", decisionID, decisionID, func(tx *sql.Tx) (collab.Receipt, error) {
		run, err := r.reviewRun(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		d, err := loadDecision(ctx, tx, id, run.CurrentIteration)
		if err != nil {
			return collab.Receipt{}, err
		}
		if run.State != "reviewing" || d.Action != "pending" || d.DecisionID != decisionID || run.CurrentBundle == nil || d.Review.Artifact != *run.CurrentBundle || d.Review.Epoch != run.Epoch {
			return collab.Receipt{}, collab.ErrConflict
		}
		d.Action, d.Reason = "attention", "inconclusive_review"
		bundle, candidateErr := currentCandidate(ctx, tx, run)
		if candidateErr != nil {
			d.Reason = "candidate_invalidated"
		} else if bundle.ValidationStatus != "evidence_complete" {
			d.Reason = "needs_validation"
		} else if d.Review.Verdict == "pass" {
			d.Action, d.Reason = "pass", ""
		} else if d.Review.Verdict == "request_changes" {
			if d.Iteration < run.MaxIterations {
				d.Action, d.Reason = "repair", ""
			} else {
				d.Reason = "iterations_exhausted"
			}
		}
		if err = taskPut(ctx, tx, `INSERT INTO coding_task_gate_decisions(document,id,run_id) VALUES(?,?,?)`, d, d.DecisionID, id); err != nil {
			return collab.Receipt{}, err
		}
		run.State = map[string]string{"pass": "finalizing", "repair": "rework_authorized", "attention": "waiting"}[d.Action]
		t, err := loadCodingTask(ctx, tx, a, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		t.ManagedRun = &run
		t.State = "waiting"
		t.WaitReason = d.Reason
		if d.Action == "repair" {
			t.Phase = "reworking"
			t.WaitReason = "repair_dispatch"
		}
		if d.Action == "pass" {
			t.WaitReason = "finalization"
		}
		if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, id); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "review.gate_consumed", d.Review.ReviewerParticipationID, d.Review.AttemptID, collab.SourceRef{Kind: "gate", RequestID: decisionID}, d)
		return collab.Receipt{ID: d.DecisionID, TaskID: run.TaskID, Status: "gate_accepted"}, err
	})
}

func authorizeIteration(ctx context.Context, tx *sql.Tx, run collab.ManagedRun, in collab.ReserveDispatchInput) error {
	if in.Iteration > run.MaxIterations {
		return collab.ErrConflict
	}
	if in.Iteration == 1 {
		if run.State != "submitted" || run.CurrentIteration != 0 || in.GateDecisionID != "" {
			return collab.ErrConflict
		}
		return nil
	}
	if run.ReviewPolicy == nil {
		return collab.ErrUnavailable
	}
	if run.State != "rework_authorized" || in.Iteration != run.CurrentIteration+1 {
		return collab.ErrConflict
	}
	d, err := loadDecision(ctx, tx, run.RunID, run.CurrentIteration)
	if err != nil {
		return err
	}
	if d.Action != "repair" || d.DecisionID != in.GateDecisionID {
		return collab.ErrConflict
	}
	_, err = currentCandidate(ctx, tx, run)
	return err
}

func (r *taskRepository) FinalizeRun(ctx context.Context, a collab.Principal, id, decisionID string) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	return r.mutate(ctx, a, id+"/finalize", decisionID, decisionID, func(tx *sql.Tx) (collab.Receipt, error) {
		run, err := r.reviewRun(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		d, err := loadDecision(ctx, tx, id, run.CurrentIteration)
		if err != nil {
			return collab.Receipt{}, err
		}
		if run.State != "finalizing" || d.Action != "pass" || d.DecisionID != decisionID || d.Review.Epoch != run.Epoch || run.CurrentBundle == nil || d.Review.Artifact != *run.CurrentBundle {
			return collab.Receipt{}, collab.ErrConflict
		}
		bundle, candidateErr := currentCandidate(ctx, tx, run)
		t, err := loadCodingTask(ctx, tx, a, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		status, kind := "needs_attention", "review.invalidated"
		if candidateErr != nil || bundle.ValidationStatus != "evidence_complete" {
			run.State = "waiting"
			t.State = "waiting"
			t.WaitReason = "candidate_invalidated"
		} else {
			run.State = "completed"
			t.State = "completed"
			t.Phase = "completed"
			t.TaskAcceptance = "accepted"
			t.WaitReason = ""
			status, kind = "task_accepted", "task.accepted"
			if _, err = tx.ExecContext(ctx, `DELETE FROM coding_task_workspace_claims WHERE run_id=?`, id); err != nil {
				return collab.Receipt{}, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE coding_task_runs SET active=0 WHERE id=?`, id); err != nil {
				return collab.Receipt{}, err
			}
		}
		t.ManagedRun = &run
		if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, id); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, kind, d.Review.ReviewerParticipationID, d.Review.AttemptID, collab.SourceRef{Kind: "gate", RequestID: decisionID}, map[string]any{"decision_id": decisionID, "artifact": d.Review.Artifact, "status": status})
		return collab.Receipt{ID: id, TaskID: run.TaskID, Status: status}, err
	})
}

func (r *taskRepository) WaitForHuman(ctx context.Context, a collab.Principal, id, reason string) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	if !taskValid(reason, 160) {
		return collab.Receipt{}, collab.ErrInvalid
	}
	return r.mutate(ctx, a, id+"/attention", reason, reason, func(tx *sql.Tx) (collab.Receipt, error) {
		run, err := r.reviewRun(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		if run.State == "completed" {
			return collab.Receipt{}, collab.ErrConflict
		}
		run.State = "waiting"
		t, err := loadCodingTask(ctx, tx, a, run.TaskID)
		if err != nil {
			return collab.Receipt{}, err
		}
		t.ManagedRun = &run
		t.State = "waiting"
		t.WaitReason = reason
		if err = taskPut(ctx, tx, `UPDATE coding_task_runs SET document=? WHERE id=?`, run, id); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "run.attention_required", "", "", collab.SourceRef{Kind: "gate"}, map[string]string{"reason": reason})
		return collab.Receipt{ID: id, TaskID: run.TaskID, Status: "needs_attention"}, err
	})
}
