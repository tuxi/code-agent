package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/gitworkspace"
	"code-agent/internal/jobs"
	runtimepkg "code-agent/internal/runtime"
	"code-agent/internal/session"

	"github.com/google/uuid"
)

const sessionRailCapability = "session_rail_v1"

var errRailJobHistoryUnavailable = errors.New("job history unavailable")

// Process identity never stands in for the durable Runtime data-source identity.
var sessionRailRuntimeInstanceID = uuid.NewString()

type railSection struct {
	State      string `json:"state"`
	Revision   string `json:"revision,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	Code       string `json:"code,omitempty"`
	Data       any    `json:"data"`
}

type railSnapshot struct {
	Schema            string                 `json:"schema"`
	ServerID          string                 `json:"server_id"`
	RuntimeInstanceID string                 `json:"runtime_instance_id"`
	ConversationID    string                 `json:"conversation_id"`
	WorkspaceID       string                 `json:"workspace_id"`
	SnapshotID        string                 `json:"snapshot_id"`
	GeneratedAt       string                 `json:"generated_at"`
	Sections          map[string]railSection `json:"sections"`
}

type railEnvironment struct {
	Head            *gitworkspace.Checkout `json:"head,omitempty"`
	Runtime         RuntimeInfo            `json:"runtime"`
	Workspace       *WorkspaceDTO          `json:"workspace"`
	ExecutionPolicy string                 `json:"execution_policy"`
	NeedsRebind     bool                   `json:"needs_rebind"`
	Worktree        *ManagedWorktreeDTO    `json:"worktree,omitempty"`
}

type railContext struct {
	exposureRequest         *agent.RequestContextSnapshot
	LatestRequestSnapshotID string                        `json:"latest_request_snapshot_id,omitempty"`
	MeasurementModel        string                        `json:"measurement_model,omitempty"`
	MeasurementWindow       int                           `json:"measurement_window,omitempty"`
	Request                 *agent.RequestContextSnapshot `json:"request,omitempty"`
	MeasurementSnapshotID   string                        `json:"measurement_snapshot_id,omitempty"`
	HistoryState            string                        `json:"history_state,omitempty"`
	Snapshot                ContextSnapshot               `json:"snapshot"`
	MeasurementState        string                        `json:"measurement_state"`
	// Legacy checkpoints do not retain measurement provenance. Do not invent it
	// from UpdatedAt, which also changes after renaming or other metadata edits.
	PromptTokens   *int    `json:"prompt_tokens"`
	MeasuredAt     *string `json:"measured_at"`
	BreakdownState string  `json:"breakdown_state"`
	WindowSource   string  `json:"window_source"`
}

type railJob struct {
	JobID           string `json:"job_id"`
	TurnID          string `json:"turn_id"`
	CallID          string `json:"call_id"`
	Command         string `json:"command"`
	Status          string `json:"status"`
	LiveState       string `json:"live_state"`
	CanControl      bool   `json:"can_control"`
	CancelRequested bool   `json:"cancel_requested"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	DurationMS      *int64 `json:"duration_ms"`
	ExitCode        *int   `json:"exit_code"`
}

type railJobs struct {
	Scope         string    `json:"scope"`
	TotalCount    int       `json:"total_count"`
	ActiveCount   *int      `json:"active_count"`
	StoppingCount *int      `json:"stopping_count"`
	TerminalCount int       `json:"terminal_count"`
	UnknownCount  int       `json:"unknown_count"`
	Items         []railJob `json:"items"`
}

func railRevision(value any) string {
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

func railReady(data any, now string) railSection {
	return railSection{State: "ready", Revision: railRevision(data), ObservedAt: now, Data: data}
}

func railUnavailable(code string) railSection {
	return railSection{State: "unavailable", Code: code, Data: nil}
}

// This incremental contract is deliberately not advertised as session_rail_v1
// until request provenance and output manifests are wired as well.
func registerSessionRailRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, events conversation.ConversationEventStore, opts MuxOptions, executor *conversation.TurnExecutor) {
	if repo == nil {
		return
	}
	changes := newRailChangesStore()
	registerRailChangesRoutes(mux, repo, changes)
	registerRailOutputRoutes(mux, repo, events)
	registerRailContextRoutes(mux, repo, events)
	registerRailConnectorRoutes(mux, repo, opts, executor, events)
	mux.HandleFunc("GET /v1/conversations/{id}/session-rail", func(w http.ResponseWriter, r *http.Request) {
		sess, err := repo.Load(r.Context(), r.PathValue("id"))
		if err != nil {
			ErrorSimple(w, r, http.StatusNotFound, "conversation not found")
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		workspaceID, _ := sess.Metadata[session.MetaWorkspaceID].(string)
		if workspaceID == "" {
			workspaceID = railRevision(sess.WorkspacePath)
		}
		out := railSnapshot{Schema: "session-rail/v1", ServerID: opts.RuntimeInfo.ServerID,
			RuntimeInstanceID: sessionRailRuntimeInstanceID, ConversationID: sess.ID, WorkspaceID: workspaceID,
			GeneratedAt: now, Sections: map[string]railSection{
				"environment":      railUnavailable("environment_unavailable"),
				"context":          railUnavailable("context_unavailable"),
				"connectors":       railUnavailable("connector_inventory_unavailable"),
				"outputs":          railUnavailable("output_manifest_pending"),
				"changes":          railUnavailable("workspace_changes_unavailable"),
				"background_tasks": railUnavailable("job_history_unavailable"),
			}}
		rebind, rebindErr := repo.NeedsRebind(r.Context(), sess.ID)
		if rebindErr == nil {
			env := railEnvironment{Runtime: opts.RuntimeInfo, Workspace: workspaceDTO(sess.WorkspacePath), ExecutionPolicy: sess.ExecutionPolicy(), NeedsRebind: rebind}
			out.Sections["environment"] = railReady(env, now)
			if opts.ManagedWorktrees != nil {
				if record, e := opts.ManagedWorktrees.WorktreeBySessionID(r.Context(), sess.ID); e == nil {
					env.Worktree = managedWorktreeDTO(record)
					out.Sections["environment"] = railReady(env, now)
				}
			}
		}
		if rebindErr == nil && !rebind {
			if reader, ok := opts.Prompts.(interface {
				ConnectorSnapshot(string) runtimepkg.ConnectorSnapshot
			}); ok {
				data := collectRailConnectors(reader, executor, sess.ID, sess.WorkspacePath)
				for i := range data.Items {
					data.Items[i].Tools = nil
				}
				section := railReady(data, now)
				section.State = data.State
				out.Sections["connectors"] = section
			}
			if data, e := changes.capture(r.Context(), sess.ID, sess.WorkspacePath); e == nil {
				preview := data
				if len(preview.Files) > 3 {
					preview.Files = preview.Files[:3]
				}
				section := railReady(preview, data.ObservedAt)
				section.Revision = data.SnapshotID
				if data.FileCount == 0 {
					section.State = "empty"
				}
				out.Sections["changes"] = section
				if env, ok := out.Sections["environment"].Data.(railEnvironment); ok {
					env.Head = &data.Head
					out.Sections["environment"] = railReady(env, data.ObservedAt)
				}
			} else {
				out.Sections["changes"] = railSection{State: "error", Code: e.Error(), Data: nil}
			}
		} else if rebindErr != nil {
			out.Sections["changes"] = railUnavailable("workspace_unavailable")
			out.Sections["connectors"] = railUnavailable("workspace_unavailable")
		} else {
			out.Sections["changes"] = railUnavailable("workspace_needs_rebind")
			out.Sections["connectors"] = railUnavailable("workspace_needs_rebind")
		}
		ctx := railContext{Snapshot: contextSnapshotFromSession(sess), MeasurementState: "unavailable", BreakdownState: "unavailable", WindowSource: "configured"}
		if sess.ContextWindow <= 0 {
			ctx.WindowSource = "fallback"
		}
		if sess.PromptTokens > 0 {
			n := sess.PromptTokens
			ctx.PromptTokens = &n
			ctx.MeasurementState = "legacy_without_provenance"
		}
		ctx = extendRailContext(r.Context(), ctx, sess, events)
		if data, ok := out.Sections["connectors"].Data.(runtimepkg.ConnectorSnapshot); ok {
			applyRailRequestExposure(&data, ctx.exposureRequest)
			section := out.Sections["connectors"]
			section.Data = data
			section.Revision = railRevision(data)
			out.Sections["connectors"] = section
		}

		out.Sections["context"] = railReady(ctx, now)
		if data, e := collectRailOutputs(r.Context(), events, sess.ID, sess.WorkspacePath); e == nil {
			preview := data
			if len(preview.Items) > 5 {
				preview.Items = preview.Items[:5]
			}
			section := railReady(preview, now)
			section.Revision = data.Revision
			if data.TotalCount == 0 && data.Coverage == "recorded_evidence" {
				section.State = "empty"
			}
			out.Sections["outputs"] = section
		}
		if data, e := collectRailJobs(r.Context(), events, opts.BackgroundJobs, sess.ID); e == nil {
			// Summary is bounded; the directory below returns the complete history.
			preview := data
			if len(preview.Items) > 3 {
				preview.Items = preview.Items[:3]
			}
			out.Sections["background_tasks"] = railReady(preview, now)
			if data.TotalCount == 0 && opts.BackgroundJobs != nil {
				s := out.Sections["background_tasks"]
				s.State = "empty"
				out.Sections["background_tasks"] = s
			}
		}
		revisions := map[string]string{}
		for name, s := range out.Sections {
			revisions[name] = s.State + ":" + s.Revision + ":" + s.Code
		}
		out.SnapshotID = railRevision(struct {
			ServerID, InstanceID, ConversationID, WorkspaceID string
			Revisions                                         map[string]string
		}{out.ServerID, out.RuntimeInstanceID, out.ConversationID, out.WorkspaceID, revisions})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/jobs/history", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := loadConversationHeader(r.Context(), repo, id); err != nil {
			ErrorSimple(w, r, http.StatusNotFound, "conversation not found")
			return
		}
		limit := 50
		if raw, exists := r.URL.Query()["limit"]; exists {
			n, e := strconv.Atoi(r.URL.Query().Get("limit"))
			if len(raw) != 1 || e != nil || n < 1 || n > 100 {
				ErrorSimple(w, r, 400, "limit must be between 1 and 100")
				return
			}
			limit = n
		}
		data, err := collectRailJobs(r.Context(), events, opts.BackgroundJobs, id)
		if err != nil {
			ErrorSimple(w, r, 503, "job history unavailable")
			return
		}
		// Cursor binds ordered identities, not durations or transient live states.
		ids := make([]string, len(data.Items))
		for i, j := range data.Items {
			ids[i] = j.JobID
		}
		revision := railRevision(ids)
		offset := 0
		if raw, exists := r.URL.Query()["cursor"]; exists {
			var cursor struct {
				Conversation string
				Revision     string
				Offset       int
			}
			b, e := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("cursor"))
			if len(raw) != 1 || len(r.URL.Query().Get("cursor")) > 4096 || e != nil || json.Unmarshal(b, &cursor) != nil || cursor.Conversation != id || cursor.Offset < 0 {
				ErrorSimple(w, r, 400, "invalid cursor")
				return
			}
			if cursor.Revision != revision {
				ErrorSimple(w, r, 409, "job history changed; refresh the directory")
				return
			}
			offset = cursor.Offset
		}
		if offset > len(data.Items) {
			ErrorSimple(w, r, 400, "invalid cursor")
			return
		}
		end := min(offset+limit, len(data.Items))
		next := ""
		if end < len(data.Items) {
			b, _ := json.Marshal(struct {
				Conversation string
				Revision     string
				Offset       int
			}{id, revision, end})
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		data.Items = data.Items[offset:end]
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, 200, struct {
			railJobs
			Revision   string `json:"revision"`
			NextCursor string `json:"next_cursor,omitempty"`
		}{data, revision, next})
	})
}

func collectRailJobs(ctx context.Context, events conversation.ConversationEventStore, registry *jobs.Registry, id string) (railJobs, error) {
	out := railJobs{Scope: "conversation_recorded_and_live_jobs", Items: []railJob{}}
	byID := map[string]railJob{}
	if events == nil {
		return out, errRailJobHistoryUnavailable
	}
	kinds := []string{string(agent.EventJobStarted), string(agent.EventJobFinished)}
	var records []session.EventRecord
	var err error
	if indexed, ok := events.(session.EventKindStore); ok {
		records, err = indexed.SessionEventsByKind(ctx, id, kinds)
	} else {
		records, err = events.Replay(ctx, id)
	}
	if err != nil {
		return out, err
	}
	for _, rec := range records {
		if rec.Kind != string(agent.EventJobStarted) && rec.Kind != string(agent.EventJobFinished) {
			continue
		}
		ev, ok := decodeStoredEvent(rec)
		// Parent bracket payloads keep the child job ID; the row owns the session.
		if !ok || ev.SessionID == "" || ev.SessionID == id {
			return out, errRailJobHistoryUnavailable
		}
		item := byID[ev.SessionID]
		item.JobID = ev.SessionID
		item.TurnID = ev.TurnID
		item.CallID = ev.CallID
		if rec.Kind == string(agent.EventJobStarted) {
			if item.StartedAt == "" && !rec.At.IsZero() {
				item.StartedAt = rec.At.UTC().Format(time.RFC3339Nano)
			}
			item.Command = ev.Text
			if item.Status == "" {
				item.Status = "unknown"
				item.LiveState = "unknown"
			}
		} else {
			switch ev.Text {
			case string(jobs.Exited), string(jobs.Failed), string(jobs.Canceled):
				item.Status = ev.Text
				item.LiveState = "historical"
				if !rec.At.IsZero() {
					item.FinishedAt = rec.At.UTC().Format(time.RFC3339Nano)
				}
				n := ev.ExitCode
				item.ExitCode = &n
				d := ev.Elapsed.Milliseconds()
				item.DurationMS = &d
			default:
				continue
			}
		}
		byID[item.JobID] = item
	}
	if registry != nil {
		active, stopping := 0, 0
		out.ActiveCount = &active
		out.StoppingCount = &stopping
		for _, snap := range registry.List() {
			if snap.Owner.SessionID != id {
				continue
			}
			job, ok := registry.Get(snap.ID)
			if !ok {
				continue
			}
			view := backgroundJobSnapshot(job)
			item := byID[snap.ID]
			item.JobID = snap.ID
			item.TurnID = view.TurnID
			item.CallID = view.CallID
			item.Command = view.Command
			item.StartedAt = view.StartedAt
			item.FinishedAt = view.FinishedAt
			item.Status = string(view.Status)
			item.LiveState = "current_instance"
			item.CanControl = view.Status == jobs.Running && view.TurnID != "" && view.CallID != ""
			item.CancelRequested = view.CancelRequested
			item.ExitCode = view.ExitCode
			d := view.DurationMS
			item.DurationMS = &d
			if view.Status == jobs.Running {
				active++
				if view.CancelRequested {
					stopping++
				}
			}
			byID[snap.ID] = item
		}
	}
	for _, item := range byID {
		switch item.Status {
		case string(jobs.Exited), string(jobs.Failed), string(jobs.Canceled):
			out.TerminalCount++
		case "unknown":
			out.UnknownCount++
		}
		out.Items = append(out.Items, item)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if (a.Status == string(jobs.Running)) != (b.Status == string(jobs.Running)) {
			return a.Status == string(jobs.Running)
		}
		at, _ := time.Parse(time.RFC3339Nano, a.StartedAt)
		bt, _ := time.Parse(time.RFC3339Nano, b.StartedAt)
		if !at.Equal(bt) {
			return at.After(bt)
		}
		return a.JobID < b.JobID
	})
	out.TotalCount = len(out.Items)
	return out, nil
}
