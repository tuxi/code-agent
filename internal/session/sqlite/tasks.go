package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"code-agent/internal/collab"
	"github.com/google/uuid"
)

type taskRepository struct {
	store    *Store
	serverID string
}

var _ collab.Repository = (*taskRepository)(nil)

func (s *Store) TaskRepository(serverID string) collab.Repository {
	var readOnly int
	if serverID == "" || s.db.QueryRow(`PRAGMA query_only`).Scan(&readOnly) != nil || readOnly != 0 {
		return nil
	}
	return &taskRepository{store: s, serverID: serverID}
}
func (r *taskRepository) Identity(ctx context.Context) (collab.Identity, error) {
	i := collab.Identity{ServerID: r.serverID}
	err := r.store.db.QueryRowContext(ctx, `SELECT value FROM coding_task_meta WHERE key='store_id'`).Scan(&i.StoreID)
	return i, err
}
func (r *taskRepository) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Acquire the SQLite writer before reading, including across Store instances.
	// A deferred read->write upgrade under WAL can otherwise fail BUSY_SNAPSHOT.
	if _, err = tx.ExecContext(ctx, `UPDATE coding_task_meta SET value=value WHERE key='store_id'`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}
func taskJSON(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }
func taskHash(b []byte) string       { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func taskID(prefix string) string    { return prefix + uuid.NewString() }
func taskValid(s string, n int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= n && !strings.ContainsRune(s, 0)
}
func taskDoc[T any](ctx context.Context, tx *sql.Tx, q string, args ...any) (T, error) {
	var value T
	var raw string
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return value, collab.ErrNotFound
		}
		return value, err
	}
	err := json.Unmarshal([]byte(raw), &value)
	return value, err
}
func loadCodingTask(ctx context.Context, tx *sql.Tx, actor collab.Principal, id string) (collab.CodingTask, error) {
	return taskDoc[collab.CodingTask](ctx, tx, `SELECT document FROM coding_tasks WHERE id=? AND owner=?`, id, actor.ID)
}
func taskPut(ctx context.Context, tx *sql.Tx, query string, value any, args ...any) error {
	raw, err := taskJSON(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, query, append([]any{raw}, args...)...)
	return err
}
func taskConstraint(err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		return collab.ErrConflict
	}
	return err
}
func (r *taskRepository) mutate(ctx context.Context, actor collab.Principal, scope, key string, input any, fn func(*sql.Tx) (collab.Receipt, error)) (collab.Receipt, error) {
	var receipt collab.Receipt
	if !taskValid(actor.ID, 160) {
		return receipt, collab.ErrForbidden
	}
	if !taskValid(key, 160) {
		return receipt, collab.ErrInvalid
	}
	b, err := json.Marshal(input)
	if err != nil {
		return receipt, collab.ErrInvalid
	}
	hash := taskHash(b)
	tx, err := r.begin(ctx)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var previousHash, raw string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,receipt FROM coding_task_requests WHERE owner=? AND scope=? AND request_id=?`, actor.ID, scope, key).Scan(&previousHash, &raw)
	if err == nil {
		if previousHash != hash {
			return receipt, collab.ErrConflict
		}
		if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
			return receipt, err
		}
		return receipt, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	receipt, err = fn(tx)
	if err != nil {
		return receipt, taskConstraint(err)
	}
	if receipt.Status == "" {
		receipt.Status = "recorded"
	}
	if receipt.TaskID != "" {
		task, err := loadCodingTask(ctx, tx, actor, receipt.TaskID)
		if err != nil {
			return receipt, err
		}
		receipt.Revision, receipt.TaskSeq = task.Revision, task.SnapshotTaskSeq
	}
	raw, err = taskJSON(receipt)
	if err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO coding_task_requests(owner,scope,request_id,payload_hash,receipt) VALUES(?,?,?,?,?)`, actor.ID, scope, key, hash, raw); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}
func emitTask(ctx context.Context, tx *sql.Tx, t *collab.CodingTask, kind, part, attempt string, source collab.SourceRef, payload any) error {
	revision, err := strconv.ParseInt(t.Revision, 10, 64)
	if err != nil {
		return err
	}
	revision++
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	e := collab.TaskEvent{SchemaVersion: 1, EventID: taskID("cte_"), TaskID: t.TaskID, TaskRevision: strconv.FormatInt(revision, 10), Kind: kind, RecordedAt: time.Now().UTC(), ParticipationID: part, AttemptID: attempt, Source: source, Payload: b}
	res, err := tx.ExecContext(ctx, `INSERT INTO coding_task_events(event_id,task_id,document) VALUES(?,?,'{}')`, e.EventID, t.TaskID)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	e.TaskSeq = strconv.FormatInt(seq, 10)
	if err = taskPut(ctx, tx, `UPDATE coding_task_events SET document=? WHERE seq=?`, e, seq); err != nil {
		return err
	}
	t.Revision, t.SnapshotTaskSeq = e.TaskRevision, e.TaskSeq
	return taskPut(ctx, tx, `UPDATE coding_tasks SET document=? WHERE id=?`, t, t.TaskID)
}

// Portable session workspace identity; a legacy absolute path is only used
// when that session has no portable ref. Callers cannot invent a workspace ID.
func taskWorkspace(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var root, rel, ext, path string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_root,''),COALESCE(workspace_rel,''),COALESCE(workspace_ext_id,''),COALESCE(workspace_path,'') FROM sessions WHERE id=?`, id).Scan(&root, &rel, &ext, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", collab.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	identity := root + "\x00" + rel + "\x00" + ext
	if root == "" {
		if !filepath.IsAbs(path) {
			return "", collab.ErrInvalid
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		identity = filepath.Clean(path)
	}
	return "workspace_" + taskHash([]byte(identity)), nil
}
func (r *taskRepository) RegisterExternal(ctx context.Context, a collab.Principal, in collab.ExternalInput) (collab.SessionRef, error) {
	var ref collab.SessionRef
	if !taskValid(in.RuntimeKind, 80) || (in.NativeSessionID != nil && !taskValid(*in.NativeSessionID, 256)) {
		return ref, collab.ErrInvalid
	}
	receipt, err := r.mutate(ctx, a, "external-session", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		ref = collab.SessionRef{Kind: "external", AgentID: taskID("agent_"), RuntimeKind: in.RuntimeKind, SessionHandle: taskID("external_"), NativeSessionID: in.NativeSessionID}
		raw, err := taskJSON(ref)
		if err != nil {
			return collab.Receipt{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO coding_task_external_sessions(handle,owner,document) VALUES(?,?,?)`, ref.SessionHandle, a.ID, raw)
		return collab.Receipt{ID: ref.SessionHandle}, err
	})
	if err != nil {
		return ref, err
	}
	var raw string
	err = r.store.db.QueryRowContext(ctx, `SELECT document FROM coding_task_external_sessions WHERE handle=? AND owner=?`, receipt.ID, a.ID).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &ref)
	}
	return ref, err
}
func (r *taskRepository) Create(ctx context.Context, a collab.Principal, in collab.CreateInput) (collab.Receipt, error) {
	if !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	if !taskValid(in.Goal, 16384) || len(in.Acceptance) > 32 {
		return collab.Receipt{}, collab.ErrInvalid
	}
	if (in.WorkspaceSessionID == "") == (in.WorkspacePath == "") {
		return collab.Receipt{}, collab.ErrInvalid
	}
	for _, s := range in.Acceptance {
		if !taskValid(s, 4096) {
			return collab.Receipt{}, collab.ErrInvalid
		}
	}
	return r.mutate(ctx, a, "create", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		var workspace string
		var err error
		if in.WorkspaceSessionID != "" {
			workspace, err = taskWorkspace(ctx, tx, in.WorkspaceSessionID)
		} else {
			if !filepath.IsAbs(in.WorkspacePath) || !taskValid(in.WorkspacePath, 4096) {
				return collab.Receipt{}, collab.ErrInvalid
			}
			path, resolveErr := filepath.EvalSymlinks(in.WorkspacePath)
			if resolveErr != nil {
				return collab.Receipt{}, collab.ErrInvalid
			}
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				return collab.Receipt{}, collab.ErrInvalid
			}
			workspace = "workspace_" + taskHash([]byte(filepath.Clean(path)))
		}
		if err != nil {
			return collab.Receipt{}, err
		}
		acceptance := append([]string{}, in.Acceptance...)
		t := collab.CodingTask{Schema: "task-snapshot/v1", TaskID: taskID("ctask_"), WorkspaceRef: workspace, Goal: in.Goal, Acceptance: acceptance, Mode: "collaborative", State: "active", Phase: "planning", Revision: "0", SnapshotTaskSeq: "0", TaskAcceptance: "not_reviewed"}
		raw, err := taskJSON(t)
		if err != nil {
			return collab.Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO coding_tasks(id,owner,workspace_ref,document) VALUES(?,?,?,?)`, t.TaskID, a.ID, workspace, raw); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "task.created", "", "", collab.SourceRef{Kind: "task_request", RequestID: in.RequestID}, map[string]string{"goal": t.Goal})
		return collab.Receipt{ID: t.TaskID, TaskID: t.TaskID}, err
	})
}
func taskRole(role string) bool { return role == "planner" || role == "executor" || role == "reviewer" }
func taskStage(role string) string {
	return map[string]string{"planner": "planning", "executor": "implementing", "reviewer": "reviewing"}[role]
}
func (r *taskRepository) Participate(ctx context.Context, a collab.Principal, id string, in collab.ParticipationInput) (collab.Receipt, error) {
	if !taskRole(in.Role) {
		return collab.Receipt{}, collab.ErrInvalid
	}
	identity, err := r.Identity(ctx)
	if err != nil {
		return collab.Receipt{}, err
	}
	return r.mutate(ctx, a, id+"/participation", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		t, err := loadCodingTask(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		p := collab.TaskParticipation{ParticipationID: taskID("part_"), TaskID: id, Role: in.Role, SessionRef: in.SessionRef, Capabilities: []string{}}
		key := ""
		switch in.SessionRef.Kind {
		case "local":
			if !a.LocalSessions {
				return collab.Receipt{}, collab.ErrForbidden
			}
			if in.SessionRef.ServerID != identity.ServerID || in.SessionRef.StoreID != identity.StoreID || in.SessionRef.SessionHandle != "" || in.SessionRef.RuntimeKind != "" || in.SessionRef.AgentID != "" || in.SessionRef.NativeSessionID != nil {
				return collab.Receipt{}, collab.ErrForbidden
			}
			workspace, err := taskWorkspace(ctx, tx, in.SessionRef.SessionID)
			if err != nil {
				return collab.Receipt{}, err
			}
			if workspace != t.WorkspaceRef {
				return collab.Receipt{}, collab.ErrForbidden
			}
			p.AgentID = "codeagent:" + r.serverID
			p.ObservationLevel = "managed"
			p.Capabilities = []string{"read_session", "read_artifacts", "link_attempt"}
			key = "local:" + in.SessionRef.SessionID
		case "external":
			registered, err := taskDoc[collab.SessionRef](ctx, tx, `SELECT document FROM coding_task_external_sessions WHERE handle=? AND owner=?`, in.SessionRef.SessionHandle, a.ID)
			if err != nil {
				return collab.Receipt{}, err
			}
			given, _ := taskJSON(in.SessionRef)
			known, _ := taskJSON(registered)
			if given != known {
				return collab.Receipt{}, collab.ErrForbidden
			}
			p.AgentID = registered.AgentID
			p.ObservationLevel = "reported"
			key = "external:" + registered.SessionHandle
		default:
			return collab.Receipt{}, collab.ErrInvalid
		}
		if in.Role == "planner" {
			p.Capabilities = append(p.Capabilities, "submit_plan")
		}
		if in.Role == "reviewer" {
			p.Capabilities = append(p.Capabilities, "submit_review")
		}
		raw, err := taskJSON(p)
		if err != nil {
			return collab.Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO coding_task_participations(id,task_id,session_key,role,document) VALUES(?,?,?,?,?)`, p.ParticipationID, id, key, in.Role, raw); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "participation.attached", p.ParticipationID, "", collab.SourceRef{Kind: "task_request", RequestID: in.RequestID}, p)
		return collab.Receipt{ID: p.ParticipationID, TaskID: id}, err
	})
}
func (r *taskRepository) LinkAttempt(ctx context.Context, a collab.Principal, id string, in collab.AttemptInput) (collab.Receipt, error) {
	if in.Iteration < 1 || in.Iteration > 10000 || !taskValid(in.TurnID, 160) || !taskValid(in.TurnRequestID, 160) {
		return collab.Receipt{}, collab.ErrInvalid
	}
	return r.mutate(ctx, a, id+"/attempt", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		t, err := loadCodingTask(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		if t.ManagedRun != nil {
			return collab.Receipt{}, collab.ErrManagedWorkspace
		}
		p, err := taskDoc[collab.TaskParticipation](ctx, tx, `SELECT document FROM coding_task_participations WHERE id=? AND task_id=?`, in.ParticipationID, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		if !a.LocalSessions || p.ObservationLevel != "managed" || p.SessionRef.Kind != "local" {
			return collab.Receipt{}, collab.ErrForbidden
		}
		workspace, err := taskWorkspace(ctx, tx, p.SessionRef.SessionID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if workspace != t.WorkspaceRef {
			return collab.Receipt{}, collab.ErrForbidden
		}
		var turn string
		var accepted int64
		err = tx.QueryRowContext(ctx, `SELECT turn_id,accepted_seq FROM turn_inputs WHERE session_id=? AND request_id=?`, p.SessionRef.SessionID, in.TurnRequestID).Scan(&turn, &accepted)
		if errors.Is(err, sql.ErrNoRows) {
			return collab.Receipt{}, collab.ErrNotFound
		}
		if err != nil {
			return collab.Receipt{}, err
		}
		if turn != in.TurnID || accepted <= 0 {
			return collab.Receipt{}, collab.ErrConflict
		}
		var admitted bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_events WHERE id=? AND session_id=? AND turn_id=? AND kind='turn_accepted')`, accepted, p.SessionRef.SessionID, turn).Scan(&admitted); err != nil {
			return collab.Receipt{}, err
		}
		if !admitted {
			return collab.Receipt{}, collab.ErrConflict
		}
		attempt := collab.TaskAttempt{AttemptID: taskID("attempt_"), TaskID: id, ParticipationID: p.ParticipationID, Stage: taskStage(p.Role), Iteration: in.Iteration, Status: "queued", SourceRef: collab.SourceRef{Kind: "session_event", ServerID: p.SessionRef.ServerID, StoreID: p.SessionRef.StoreID, SessionID: p.SessionRef.SessionID, TurnID: turn, RequestID: in.TurnRequestID}}
		raw, err := taskJSON(attempt)
		if err != nil {
			return collab.Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO coding_task_attempts(id,task_id,participation_id,session_id,turn_id,document) VALUES(?,?,?,?,?,?)`, attempt.AttemptID, id, p.ParticipationID, p.SessionRef.SessionID, turn, raw); err != nil {
			return collab.Receipt{}, err
		}
		t.CurrentAttemptID = attempt.AttemptID
		t.State = "active"
		t.Phase = attempt.Stage
		t.WaitReason = ""
		if err = emitTask(ctx, tx, &t, "attempt.linked", p.ParticipationID, attempt.AttemptID, attempt.SourceRef, map[string]string{"status": "queued"}); err != nil {
			return collab.Receipt{}, err
		}
		if err = projectTask(ctx, tx, a, &t); err != nil {
			return collab.Receipt{}, err
		}
		return collab.Receipt{ID: attempt.AttemptID, TaskID: id}, nil
	})
}

// Projection runs in the task transaction on bind/read. It consumes only exact
// turn lifecycle rows, never full tool output, and never dispatches work.
func projectTask(ctx context.Context, tx *sql.Tx, a collab.Principal, t *collab.CodingTask) error {
	type pending struct {
		attempt    collab.TaskAttempt
		checkpoint int64
	}
	rows, err := tx.QueryContext(ctx, `SELECT document,checkpoint FROM coding_task_attempts WHERE task_id=? AND session_id IS NOT NULL ORDER BY rowid`, t.TaskID)
	if err != nil {
		return err
	}
	var work []pending
	for rows.Next() {
		var raw string
		var p pending
		if err = rows.Scan(&raw, &p.checkpoint); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal([]byte(raw), &p.attempt); err != nil {
			rows.Close()
			return err
		}
		work = append(work, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range work {
		source := p.attempt.SourceRef
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)`, source.SessionID).Scan(&exists); err != nil {
			return err
		}
		if !exists && (p.attempt.Status == "queued" || p.attempt.Status == "running") {
			p.attempt.Status = "source_unavailable"
			if t.ManagedRun == nil && t.CurrentAttemptID == p.attempt.AttemptID {
				t.State = "waiting"
				t.WaitReason = "source_unavailable"
			}
			missingSource := source
			missingSource.Kind = "projection"
			if err = emitTask(ctx, tx, t, "attempt.source_unavailable", p.attempt.ParticipationID, p.attempt.AttemptID, missingSource, map[string]string{"reason": "session_removed"}); err != nil {
				return err
			}
			if err = taskPut(ctx, tx, `UPDATE coding_task_attempts SET document=? WHERE id=?`, p.attempt, p.attempt.AttemptID); err != nil {
				return err
			}
		}
		if !exists {
			continue
		}
		events, err := tx.QueryContext(ctx, `SELECT id,kind FROM session_events WHERE session_id=? AND turn_id=? AND id>? AND kind IN ('turn_started','turn_finished','turn_failed','turn_cancelled') ORDER BY id`, source.SessionID, source.TurnID, p.checkpoint)
		if err != nil {
			return err
		}
		type milestone struct {
			seq  int64
			kind string
		}
		var milestones []milestone
		for events.Next() {
			var m milestone
			if err = events.Scan(&m.seq, &m.kind); err != nil {
				events.Close()
				return err
			}
			milestones = append(milestones, m)
		}
		err = events.Err()
		events.Close()
		if err != nil {
			return err
		}
		for _, m := range milestones {
			status := map[string]string{"turn_started": "running", "turn_finished": "completed", "turn_failed": "failed", "turn_cancelled": "cancelled"}[m.kind]
			// Ignore duplicate or late starts after a durable terminal boundary.
			terminal := p.attempt.Status == "completed" || p.attempt.Status == "failed" || p.attempt.Status == "cancelled"
			if !terminal && status != p.attempt.Status {
				p.attempt.Status = status
				if t.ManagedRun == nil && t.CurrentAttemptID == p.attempt.AttemptID {
					t.State = "active"
					t.Phase = p.attempt.Stage
					t.WaitReason = ""
					if status != "running" {
						t.State = "waiting"
						t.WaitReason = "execution_" + status
						if status == "completed" {
							switch p.attempt.Stage {
							case "planning":
								t.WaitReason = "execution_not_started"
							case "implementing":
								t.Phase = "reviewing"
								t.WaitReason = "external_review"
							case "reviewing":
								t.WaitReason = "review_gate_unavailable"
							}
						}
					}
				}
				source.Seq = strconv.FormatInt(m.seq, 10)
				kind := "attempt." + status
				if status == "running" {
					kind = "attempt.started"
				}
				if err = emitTask(ctx, tx, t, kind, p.attempt.ParticipationID, p.attempt.AttemptID, source, map[string]string{"status": status, "task_acceptance": "not_reviewed"}); err != nil {
					return err
				}
			}
			p.checkpoint = m.seq
		}
		if len(milestones) > 0 {
			raw, err := taskJSON(p.attempt)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE coding_task_attempts SET document=?,checkpoint=? WHERE id=?`, raw, p.checkpoint, p.attempt.AttemptID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *taskRepository) LinkArtifact(ctx context.Context, a collab.Principal, id string, in collab.ArtifactInput) (collab.Receipt, error) {
	if in.Kind != "patch" && in.Kind != "test_report" {
		return collab.Receipt{}, collab.ErrInvalid
	}
	seq, err := strconv.ParseInt(in.SourceSeq, 10, 64)
	if err != nil || seq <= 0 {
		return collab.Receipt{}, collab.ErrInvalid
	}
	if in.ContentHash != nil {
		b, err := hex.DecodeString(*in.ContentHash)
		if err != nil || len(b) != 32 {
			return collab.Receipt{}, collab.ErrInvalid
		}
	}
	return r.mutate(ctx, a, id+"/artifact", in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		t, err := loadCodingTask(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		attempt, err := taskDoc[collab.TaskAttempt](ctx, tx, `SELECT document FROM coding_task_attempts WHERE task_id=? AND id=?`, id, in.AttemptID)
		if err != nil {
			return collab.Receipt{}, err
		}
		if attempt.SourceRef.SessionID == "" {
			return collab.Receipt{}, collab.ErrForbidden
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_events WHERE id=? AND session_id=? AND turn_id=? AND kind='tool_finished')`, seq, attempt.SourceRef.SessionID, attempt.SourceRef.TurnID).Scan(&exists); err != nil {
			return collab.Receipt{}, err
		}
		if !exists {
			return collab.Receipt{}, collab.ErrConflict
		}
		source := attempt.SourceRef
		source.Seq = in.SourceSeq
		artifact := collab.TaskArtifactRef{ArtifactID: taskID("artifact_"), AttemptID: attempt.AttemptID, Kind: in.Kind, ContentHash: in.ContentHash, CaptureMode: "reported_reference", SourceRef: source, Truncated: in.Truncated}
		if err = saveTaskArtifact(ctx, tx, id, &artifact); err != nil {
			return collab.Receipt{}, err
		}
		err = emitTask(ctx, tx, &t, "artifact.linked", attempt.ParticipationID, attempt.AttemptID, collab.SourceRef{Kind: "task_request", RequestID: in.RequestID}, artifact)
		return collab.Receipt{ID: artifact.ArtifactID, TaskID: id}, err
	})
}
func saveTaskArtifact(ctx context.Context, tx *sql.Tx, id string, artifact *collab.TaskArtifactRef) error {
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM coding_task_artifacts WHERE task_id=? AND kind=?`, id, artifact.Kind).Scan(&version); err != nil {
		return err
	}
	artifact.Version = strconv.FormatInt(version, 10)
	raw, err := taskJSON(artifact)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO coding_task_artifacts(id,task_id,attempt_id,kind,version,document) VALUES(?,?,?,?,?,?)`, artifact.ArtifactID, id, artifact.AttemptID, artifact.Kind, version, raw)
	return err
}
func (r *taskRepository) Report(ctx context.Context, a collab.Principal, id, kind string, in collab.ReportInput) (collab.Receipt, error) {
	if (kind != "plan" && kind != "review") || !taskValid(in.SubmissionID, 160) || !taskValid(in.Content, 65536) || in.Iteration < 1 || in.Iteration > 10000 {
		return collab.Receipt{}, collab.ErrInvalid
	}
	if kind == "review" {
		if in.Verdict != "pass" && in.Verdict != "request_changes" && in.Verdict != "inconclusive" {
			return collab.Receipt{}, collab.ErrInvalid
		}
	} else if in.Verdict != "" || in.ReviewedArtifactID != "" {
		return collab.Receipt{}, collab.ErrInvalid
	}
	return r.mutate(ctx, a, id+"/"+kind, in.RequestID, in, func(tx *sql.Tx) (collab.Receipt, error) {
		t, err := loadCodingTask(ctx, tx, a, id)
		if err != nil {
			return collab.Receipt{}, err
		}
		p, err := taskDoc[collab.TaskParticipation](ctx, tx, `SELECT document FROM coding_task_participations WHERE task_id=? AND id=?`, id, in.ParticipationID)
		if err != nil {
			return collab.Receipt{}, err
		}
		role := "planner"
		if kind == "review" {
			role = "reviewer"
		}
		if p.Role != role {
			return collab.Receipt{}, collab.ErrForbidden
		}
		var reviewed *collab.TaskArtifactRef
		if kind == "review" {
			target, err := taskDoc[collab.TaskArtifactRef](ctx, tx, `SELECT document FROM coding_task_artifacts WHERE id=? AND task_id=?`, in.ReviewedArtifactID, id)
			if err != nil {
				return collab.Receipt{}, err
			}
			if target.Kind != "patch" {
				return collab.Receipt{}, collab.ErrInvalid
			}
			reviewed = &target
		}
		attempt := collab.TaskAttempt{AttemptID: taskID("attempt_"), TaskID: id, ParticipationID: p.ParticipationID, Stage: taskStage(role), Iteration: in.Iteration, Status: "reported", SourceRef: collab.SourceRef{Kind: "report", ProducerSubmissionID: in.SubmissionID, RequestID: in.RequestID}}
		raw, err := taskJSON(attempt)
		if err != nil {
			return collab.Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO coding_task_attempts(id,task_id,participation_id,submission_id,document) VALUES(?,?,?,?,?)`, attempt.AttemptID, id, p.ParticipationID, in.SubmissionID, raw); err != nil {
			return collab.Receipt{}, err
		}
		content, err := json.Marshal(struct {
			Text     string                  `json:"text"`
			Verdict  string                  `json:"verdict,omitempty"`
			Reviewed *collab.TaskArtifactRef `json:"reviewed_artifact,omitempty"`
		}{in.Content, in.Verdict, reviewed})
		if err != nil {
			return collab.Receipt{}, err
		}
		hash := taskHash(content)
		artifact := collab.TaskArtifactRef{ArtifactID: taskID("artifact_"), AttemptID: attempt.AttemptID, Kind: kind, ContentHash: &hash, CaptureMode: "reported", SourceRef: attempt.SourceRef, Content: content}
		if err = saveTaskArtifact(ctx, tx, id, &artifact); err != nil {
			return collab.Receipt{}, err
		}
		if t.ManagedRun == nil && t.CurrentAttemptID == "" {
			t.State = "waiting"
			t.WaitReason = "execution_not_started"
		}
		if kind == "review" {
			// A reported review of an earlier patch cannot hide a currently
			// queued/running execution attempt. Recording is not scheduling.
			if err = projectTask(ctx, tx, a, &t); err != nil {
				return collab.Receipt{}, err
			}
			active := false
			if t.CurrentAttemptID != "" {
				current, err := taskDoc[collab.TaskAttempt](ctx, tx, `SELECT document FROM coding_task_attempts WHERE id=? AND task_id=?`, t.CurrentAttemptID, id)
				if err != nil {
					return collab.Receipt{}, err
				}
				active = current.Status == "queued" || current.Status == "running"
			}
			if t.ManagedRun == nil && !active {
				t.State = "waiting"
				t.Phase = "reviewing"
				t.WaitReason = "review_gate_unavailable"
			}
		}
		milestone := artifact
		milestone.Content = nil // detail remains in the artifact, not every timeline event
		err = emitTask(ctx, tx, &t, kind+".reported", p.ParticipationID, attempt.AttemptID, attempt.SourceRef, milestone)
		return collab.Receipt{ID: artifact.ArtifactID, TaskID: id}, err
	})
}

func taskDocuments[T any](ctx context.Context, tx *sql.Tx, table, id string) ([]T, error) {
	// table is selected only by internal constant call sites.
	rows, err := tx.QueryContext(ctx, `SELECT document FROM `+table+` WHERE task_id=? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []T{}
	for rows.Next() {
		var raw string
		var v T
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
func (r *taskRepository) Snapshot(ctx context.Context, a collab.Principal, id string) (collab.Snapshot, error) {
	var snap collab.Snapshot
	tx, err := r.begin(ctx)
	if err != nil {
		return snap, err
	}
	defer tx.Rollback()
	t, err := loadCodingTask(ctx, tx, a, id)
	if err != nil {
		return snap, err
	}
	if err = projectTask(ctx, tx, a, &t); err != nil {
		return snap, err
	}
	snap.CodingTask = t
	if snap.Participations, err = taskDocuments[collab.TaskParticipation](ctx, tx, "coding_task_participations", id); err != nil {
		return snap, err
	}
	if snap.Attempts, err = taskDocuments[collab.TaskAttempt](ctx, tx, "coding_task_attempts", id); err != nil {
		return snap, err
	}
	if snap.ArtifactRefs, err = taskDocuments[collab.TaskArtifactRef](ctx, tx, "coding_task_artifacts", id); err != nil {
		return snap, err
	}
	snap.AvailableActions = []string{"read_task", "read_events"}
	hasPatch := false
	for _, artifact := range snap.ArtifactRefs {
		if artifact.Kind == "patch" {
			hasPatch = true
		}
	}
	seenActions := map[string]bool{}
	for _, p := range snap.Participations {
		if p.Role == "planner" && !seenActions["submit_plan"] {
			snap.AvailableActions = append(snap.AvailableActions, "submit_plan")
			seenActions["submit_plan"] = true
		}
		if p.Role == "reviewer" && hasPatch && !seenActions["submit_review"] {
			snap.AvailableActions = append(snap.AvailableActions, "submit_review")
			seenActions["submit_review"] = true
		}
	}
	// Pending controls, aggregate usage and cost are unknown in B1, not zero.
	return snap, tx.Commit()
}
func (r *taskRepository) Events(ctx context.Context, a collab.Principal, id string, after int64, limit int) (collab.EventPage, error) {
	page := collab.EventPage{Schema: "task-event-page/v1", TaskID: id, After: strconv.FormatInt(after, 10), NextCursor: strconv.FormatInt(after, 10), Events: []collab.TaskEvent{}}
	if after < 0 || limit < 1 || limit > 200 {
		return page, collab.ErrInvalid
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	t, err := loadCodingTask(ctx, tx, a, id)
	if err != nil {
		return page, err
	}
	if err = projectTask(ctx, tx, a, &t); err != nil {
		return page, err
	}
	watermark, err := strconv.ParseInt(t.SnapshotTaskSeq, 10, 64)
	if err != nil {
		return page, err
	}
	if after > watermark {
		return page, collab.ErrInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT document FROM coding_task_events WHERE task_id=? AND seq>? ORDER BY seq LIMIT ?`, id, after, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var raw string
		var event collab.TaskEvent
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return page, err
		}
		if err = json.Unmarshal([]byte(raw), &event); err != nil {
			rows.Close()
			return page, err
		}
		page.Events = append(page.Events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(page.Events) > limit {
		page.HasMore = true
		page.Events = page.Events[:limit]
	}
	if len(page.Events) > 0 {
		page.NextCursor = page.Events[len(page.Events)-1].TaskSeq
	}
	return page, tx.Commit()
}
func (r *taskRepository) List(ctx context.Context, a collab.Principal, workspace string, limit int) ([]collab.CodingTask, error) {
	if !taskValid(a.ID, 160) {
		return nil, collab.ErrForbidden
	}
	if limit < 1 || limit > 200 {
		return nil, collab.ErrInvalid
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT document FROM coding_tasks WHERE owner=? AND (?='' OR workspace_ref=?) ORDER BY rowid DESC LIMIT ?`, a.ID, workspace, workspace, limit)
	if err != nil {
		return nil, err
	}
	tasks := []collab.CodingTask{}
	for rows.Next() {
		var raw string
		var t collab.CodingTask
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &t); err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if err = projectTask(ctx, tx, a, &tasks[i]); err != nil {
			return nil, err
		}
	}
	return tasks, tx.Commit()
}
