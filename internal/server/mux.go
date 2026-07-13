package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/mcp"
	"code-agent/internal/repos"
	"code-agent/internal/session"

	"github.com/coder/websocket"
)

// statusForCloneCode maps a structured clone error code to an HTTP status.
func statusForCloneCode(code string) int {
	switch code {
	case "invalid_url", "invalid_name":
		return http.StatusBadRequest
	case "repo_not_found", "ref_not_found":
		return http.StatusNotFound
	case "network_error":
		return http.StatusBadGateway
	default: // io_error and anything unexpected
		return http.StatusInternalServerError
	}
}

// CreateConversationRequest is the POST /v1/conversations body. workspace_path is
// the client's desired project root directory; an empty workspace_path means "use
// the server's default workspace." workspace_ext_id is the host's stable identifier
// for a workspace outside the launch workspaceDir (an iOS security-scoped-bookmark
// id); empty for workspace-local paths and on desktop. See spec §6.1.
type CreateConversationRequest struct {
	ClientRequestID string `json:"client_request_id,omitempty"`
	WorkspacePath   string `json:"workspace_path,omitempty"`
	WorkspaceExtID  string `json:"workspace_ext_id,omitempty"`
	// ExecutionPolicy controls Runtime workspace leasing. isolated_worktree
	// requires workspace_path to already identify the session's own worktree.
	ExecutionPolicy string                        `json:"execution_policy,omitempty"`
	WorkspaceID     string                        `json:"workspace_id,omitempty"`
	BaseWorkspaceID string                        `json:"base_workspace_id,omitempty"`
	Worktree        *ManagedWorktreeCreateRequest `json:"worktree,omitempty"`
}

type ManagedWorktreeCreateRequest struct {
	Managed       bool   `json:"managed"`
	SuggestedName string `json:"suggested_name,omitempty"`
	BaseRef       string `json:"base_ref,omitempty"`
}

type ManagedWorktreeDTO struct {
	Managed bool   `json:"managed"`
	Name    string `json:"name,omitempty"`
	Branch  string `json:"branch,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	State   string `json:"state"`
}

type APIWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type managedWorktreeErrorResponse struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
}

// RebindRequest is the POST /v1/conversations/{id}/rebind body: the host re-supplies
// the fresh absolute path of an external workspace for this launch. See spec §6.2bis.
type RebindRequest struct {
	WorkspacePath string `json:"workspace_path"`
}

// WorkspaceRefDTO is the structured workspace identity returned in ConversationDetail,
// so the host can map ext_id back to a security-scoped bookmark.
type WorkspaceRefDTO struct {
	Root  string `json:"root,omitempty"`
	Rel   string `json:"rel,omitempty"`
	ExtID string `json:"ext_id,omitempty"`
}

// WorkspaceDTO is the UI-facing workspace anchor. It scopes relative paths and
// asset ids without replacing the portable WorkspaceRefDTO used for iOS rebinds.
type WorkspaceDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	RootPath    string `json:"root_path"`
	RuntimeCWD  string `json:"runtime_cwd,omitempty"`
	DisplayPath string `json:"display_path,omitempty"`
	Kind        string `json:"kind"`
}

// CloneRepoRequest is the POST /v1/repos/clone body. See docs/ios_github_clone_spec.md.
type CloneRepoRequest struct {
	URL   string `json:"url"`             // https GitHub URL or "owner/repo" shorthand
	Ref   string `json:"ref,omitempty"`   // optional branch/tag
	Name  string `json:"name,omitempty"`  // optional target dir name
	Depth int    `json:"depth,omitempty"` // optional shallow depth (0 => default)
}

// CloneRepoResponse is returned on a successful clone. workspace_path is this
// launch's absolute path (host displays it, does not persist it); workspace_ref is
// the portable identity to create the conversation with.
type CloneRepoResponse struct {
	WorkspacePath string           `json:"workspace_path"`
	WorkspaceRef  *WorkspaceRefDTO `json:"workspace_ref"`
}

// cloneErrorResponse is the structured error body keyed on a stable code.
type cloneErrorResponse struct {
	Error   string `json:"error"`   // invalid_url | repo_not_found | ref_not_found | network_error | io_error | invalid_name
	Message string `json:"message"` // human-readable detail
}

// ConversationRef is the minimal conversation descriptor returned by create and
// list. TurnStatus/PausedAt surface the turn lifecycle (v1.2 §3.2) so a host can
// list interrupted sessions and render a "continue" entry — a paused status with
// paused_at (unix seconds) marks a turn the host may resume.
type ConversationRef struct {
	ID              string              `json:"id"`
	WorkspacePath   string              `json:"workspace_path"`
	Workspace       *WorkspaceDTO       `json:"workspace,omitempty"`
	Name            string              `json:"name,omitempty"`
	TurnStatus      string              `json:"turn_status,omitempty"`
	PausedAt        int64               `json:"paused_at,omitempty"`
	ExecutionPolicy string              `json:"execution_policy,omitempty"`
	WorkspaceID     string              `json:"workspace_id,omitempty"`
	BaseWorkspaceID string              `json:"base_workspace_id,omitempty"`
	Worktree        *ManagedWorktreeDTO `json:"worktree,omitempty"`
	Warnings        []APIWarning        `json:"warnings,omitempty"`
}

// ConversationDetail is GET /v1/conversations/{id}. Counts and timestamps are
// derived from the recorded event stream; workspace_path comes from the session
// metadata (identity, not an event). workspace_ref + needs_rebind let an iOS host
// re-anchor an external workspace before opening the stream (spec §6.2bis/§6.3).
type ConversationDetail struct {
	ID              string              `json:"id"`
	WorkspacePath   string              `json:"workspace_path"`
	Workspace       *WorkspaceDTO       `json:"workspace,omitempty"`
	WorkspaceRef    *WorkspaceRefDTO    `json:"workspace_ref,omitempty"`
	NeedsRebind     bool                `json:"needs_rebind,omitempty"`
	Name            string              `json:"name,omitempty"`
	TurnCount       int                 `json:"turn_count"`
	MessageCount    int                 `json:"message_count"`
	CreatedAt       string              `json:"created_at,omitempty"`
	UpdatedAt       string              `json:"updated_at,omitempty"`
	ExecutionPolicy string              `json:"execution_policy,omitempty"`
	WorkspaceID     string              `json:"workspace_id,omitempty"`
	BaseWorkspaceID string              `json:"base_workspace_id,omitempty"`
	Worktree        *ManagedWorktreeDTO `json:"worktree,omitempty"`
	Warnings        []APIWarning        `json:"warnings,omitempty"`
}

// MessageView is one entry of GET /v1/conversations/{id}/messages. v1 reconstructs
// the conversational backbone from turn_started (user) / turn_finished (assistant)
// events.
type MessageView struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

// MuxOptions configures the HTTP surface.
type MuxOptions struct {
	// ServerName is reported in the WebSocket hello handshake.
	ServerName string
	// Capabilities is the server capability list declared in the hello handshake.
	// Nil means no capabilities advertised. The caller (cmd/codeagent) owns the
	// list; the server layer is a dumb pipe — it never derives capabilities from
	// runtime state.
	Capabilities []string
	// Accept carries WebSocket origin policy. Nil = same-origin only.
	Accept *websocket.AcceptOptions
	// WorkspaceRoot is the default workspace directory (cfg.Workspace.Root). The
	// repo-clone endpoint clones into a subdirectory of it. Empty disables that
	// endpoint (it returns 400).
	WorkspaceRoot string
	// Granter persists a client's "always allow" verdict into the shared permission
	// store (the same one the loop's allowlist reads). Nil disables persistence, so
	// an "always" over the wire is treated as a one-time allow.
	Granter PermissionGranter
	// WorkspaceReloader reloads MCP servers for a given workspace. Nil disables
	// the POST /v1/workspaces/{path}/mcp/reload endpoint (returns 404).
	WorkspaceReloader func(workspacePath string) error
	// Prompts serves GET /v1/prompts and renders invoke_prompt. Nil disables MCP
	// prompts on the wire (the endpoint returns an empty list; invoke is a no-op).
	Prompts PromptService
	// CredentialStore stores a per-session credential extracted from the
	// Authorization header at WS upgrade time. Nil means credentials come from
	// the base provider (embedded/CLI modes).
	CredentialStore func(sessionID string, cred credential.Resolver)
	// RuntimeCapabilities describes execution guarantees, not merely supported
	// endpoints. It intentionally defaults to all false until scheduler and
	// workspace isolation are fully installed.
	RuntimeCapabilities RuntimeCapabilities
}

// RuntimeCapabilities is the explicit concurrency handshake consumed by
// multi-session clients. WebSocket hello capabilities remain a backwards-
// compatible feature list; this endpoint is the source of truth for execution.
type RuntimeCapabilities struct {
	MultiSessionExecution    bool `json:"multi_session_execution_v1"`
	SessionScopedClientTools bool `json:"session_scoped_client_tools_v1"`
	ActivitySnapshot         bool `json:"activity_snapshot_v1"`
	SessionAttentionSnapshot bool `json:"session_attention_snapshot_v1"`
	SessionAttentionDelta    bool `json:"session_attention_delta_v1"`
	WorkspaceExecutionPolicy bool `json:"workspace_execution_policy_v1"`
	ManagedWorktree          bool `json:"managed_worktree_v1"`
	MaxConcurrentTurns       int  `json:"max_concurrent_turns"`
	MaxConnectedSessions     int  `json:"max_connected_sessions"`
}

// ConfiguredRuntimeCapabilities derives advertised guarantees from the same
// effective scheduler limit used by the executor. Managed worktree provisioning
// remains a separate capability and is intentionally false.
func ConfiguredRuntimeCapabilities(maxConcurrentTurns int) RuntimeCapabilities {
	if maxConcurrentTurns < 1 {
		maxConcurrentTurns = 1
	}
	return RuntimeCapabilities{
		MultiSessionExecution:    maxConcurrentTurns > 1,
		SessionScopedClientTools: true,
		ActivitySnapshot:         true,
		SessionAttentionSnapshot: true,
		SessionAttentionDelta:    true,
		WorkspaceExecutionPolicy: true,
		ManagedWorktree:          false,
		MaxConcurrentTurns:       maxConcurrentTurns,
	}
}

type runtimeCapabilitiesResponse struct {
	Capabilities RuntimeCapabilities `json:"capabilities"`
}

// SessionActivity is the merged scheduler, broker, lifecycle, and durable-event
// projection for one session.
type SessionTerminalActivity struct {
	TurnID   string `json:"turn_id"`
	Kind     string `json:"kind"`
	Sequence int64  `json:"sequence"`
	At       string `json:"at"`
}

type SessionActivity struct {
	SessionID              string                   `json:"session_id"`
	TurnID                 string                   `json:"turn_id,omitempty"`
	ActiveTurnID           string                   `json:"active_turn_id,omitempty"`
	State                  string                   `json:"state"`
	QueuePosition          int                      `json:"queue_position"`
	PendingApprovalCount   int                      `json:"pending_approval_count"`
	PendingClientToolCount int                      `json:"pending_client_tool_count"`
	LastSequence           int64                    `json:"last_sequence"`
	LatestTerminal         *SessionTerminalActivity `json:"latest_terminal,omitempty"`
	UpdatedAt              string                   `json:"updated_at,omitempty"`
}

type activityResponse struct {
	GeneratedAt string            `json:"generated_at"`
	Cursor      int64             `json:"cursor"`
	IsDelta     bool              `json:"is_delta"`
	Sessions    []SessionActivity `json:"sessions"`
}

func terminalActivityState(kind string) string {
	switch kind {
	case string(agent.EventTurnFinished):
		return "done"
	case string(agent.EventTurnFailed):
		return "failed"
	case string(agent.EventTurnCancelled):
		return "cancelled"
	default:
		return "idle"
	}
}

func laterTimestamp(current string, candidate time.Time) string {
	if candidate.IsZero() {
		return current
	}
	if current == "" {
		return candidate.UTC().Format(time.RFC3339Nano)
	}
	parsed, err := time.Parse(time.RFC3339Nano, current)
	if err != nil || candidate.After(parsed) {
		return candidate.UTC().Format(time.RFC3339Nano)
	}
	return current
}

// NewMux builds the HTTP surface of `codeagent serve`:
//
//	GET  /healthz                            liveness
//	GET  /v1/conversations                   list (from Repository / SQLite)
//	POST /v1/conversations                   create → {id} (writes SQLite, no Runtime)
//	GET  /v1/conversations/{id}              detail (derived from events + repo)
//	GET  /v1/conversations/{id}/messages     conversational backbone (derived)
//	GET  /v1/conversations/{id}/events       recorded events, re-encoded to wire v1
//	GET  /v1/conversations/{id}/assets/{asset_id}/preview  derived asset preview
//	GET  /v1/conversations/{id}/assets/{asset_id}/content  workspace text content
//	GET  /v1/conversations/{id}/assets/{asset_id}/blob     workspace binary content
//	GET  /v1/conversations/{id}/assets/{asset_id}/thumbnail media thumbnail placeholder
//	DELETE /v1/conversations/{id}            delete session + events
//	PATCH /v1/conversations/{id}              rename — body: {"name":"..."}
//	GET  /v1/conversations/{id}/stream       upgrade via TurnExecutor/TransportSession
//	GET  /v2/conversations/{id}/stream       same (alias)
//
// All read endpoints query the Repository directly — conversation list survives
// server restart. The WS path uses TurnExecutor: a TransportSession is created
// per connection, wrapping the executor — no permanent in-memory Conversation.
func NewMux(repo conversation.ConversationRepository, eventStore conversation.ConversationEventStore, executor *conversation.TurnExecutor, opts MuxOptions) http.Handler {
	mux := http.NewServeMux()
	attentionStore, attentionSupported := eventStore.(conversation.ConversationAttentionStore)
	if capability, ok := eventStore.(conversation.ConversationAttentionCapability); ok {
		attentionSupported = attentionSupported && capability.SupportsAttentionSnapshot()
	}
	runtimeCapabilities := opts.RuntimeCapabilities
	if !attentionSupported {
		runtimeCapabilities.SessionAttentionSnapshot = false
		runtimeCapabilities.SessionAttentionDelta = false
	}
	// Assigned below when the WebSocket routes are assembled. Activity handlers
	// run only after NewMux returns, so the closure always sees the installed
	// session-scoped broker.
	var ws *WSHandler

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /v1/runtime/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, runtimeCapabilitiesResponse{Capabilities: runtimeCapabilities})
	})

	mux.HandleFunc("GET /v1/activity", func(w http.ResponseWriter, r *http.Request) {
		generatedAt := time.Now().UTC()
		sinceSequence := int64(0)
		isDelta := false
		if raw := r.URL.Query().Get("since_sequence"); raw != "" {
			parsed, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || parsed < 0 {
				http.Error(w, "since_sequence must be a non-negative integer", http.StatusBadRequest)
				return
			}
			sinceSequence = parsed
			isDelta = true
		}
		metas, err := repo.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		metaBySession := make(map[string]session.Meta, len(metas))
		activityBySession := make(map[string]SessionActivity)
		for _, meta := range metas {
			metaBySession[meta.ID] = meta
			switch meta.TurnStatus {
			case session.TurnStatusRunning, session.TurnStatusResuming:
				// Persisted running/resuming is a recovery checkpoint, not proof that
				// this process still owns a goroutine. Only the scheduler overlay below
				// may report live execution; otherwise surface a resumable pause.
				activityBySession[meta.ID] = SessionActivity{
					SessionID: meta.ID,
					State:     session.TurnStatusPaused,
					UpdatedAt: meta.UpdatedAt.UTC().Format(time.RFC3339Nano),
				}
			case session.TurnStatusPaused:
				activityBySession[meta.ID] = SessionActivity{
					SessionID: meta.ID,
					State:     meta.TurnStatus,
					UpdatedAt: meta.UpdatedAt.UTC().Format(time.RFC3339Nano),
				}
			}
		}

		// Stable unread/completion facts come only from the persisted event log.
		// NewMux downgrades the attention capability when the backend does not
		// implement this optional projection.
		cursor := int64(0)
		if attentionSupported {
			snapshot, attentionErr := attentionStore.Attention(r.Context(), sinceSequence)
			if attentionErr != nil {
				http.Error(w, attentionErr.Error(), http.StatusInternalServerError)
				return
			}
			cursor = snapshot.LastSequence
			for _, head := range snapshot.Sessions {
				meta, exists := metaBySession[head.SessionID]
				if !exists {
					continue // exclude background-job event partitions
				}
				activity := activityBySession[head.SessionID]
				activity.SessionID = head.SessionID
				if activity.State == "" {
					activity.State = "idle"
				}
				activity.LastSequence = head.LastSequence
				if head.LatestEvent != nil && activity.ActiveTurnID == "" &&
					(meta.TurnStatus == session.TurnStatusPaused || meta.TurnStatus == session.TurnStatusResuming || meta.TurnStatus == session.TurnStatusRunning) {
					activity.ActiveTurnID = head.LatestEvent.TurnID
					activity.TurnID = activity.ActiveTurnID
				}
				if head.LatestTerminal != nil {
					activity.LatestTerminal = &SessionTerminalActivity{
						TurnID:   head.LatestTerminal.TurnID,
						Kind:     head.LatestTerminal.Kind,
						Sequence: head.LatestTerminal.Seq,
						At:       head.LatestTerminal.At.UTC().Format(time.RFC3339Nano),
					}
					activity.UpdatedAt = laterTimestamp(activity.UpdatedAt, head.LatestTerminal.At)
				}
				activityBySession[head.SessionID] = activity
			}
		}
		// Scheduler state wins over persisted lifecycle state: it can report a
		// freshly queued/running turn before the first checkpoint reaches SQLite.
		// Read-only/test muxes intentionally have no executor.
		if executor != nil {
			for _, live := range executor.Activity() {
				activity := activityBySession[live.SessionID]
				activity.SessionID = live.SessionID
				activity.TurnID = live.TurnID
				activity.ActiveTurnID = live.TurnID
				activity.State = live.State
				activity.QueuePosition = live.QueuePosition
				activity.UpdatedAt = generatedAt.Format(time.RFC3339Nano)
				activityBySession[live.SessionID] = activity
			}
		}
		if ws != nil {
			for sessionID, broker := range ws.BrokerAttention() {
				if _, exists := metaBySession[sessionID]; !exists {
					continue
				}
				activity := activityBySession[sessionID]
				activity.SessionID = sessionID
				activity.PendingApprovalCount = broker.PendingApprovalCount
				activity.PendingClientToolCount = broker.PendingClientToolCount
				activity.UpdatedAt = generatedAt.Format(time.RFC3339Nano)
				activityBySession[sessionID] = activity
			}
		}
		activities := make([]SessionActivity, 0, len(activityBySession))
		for _, activity := range activityBySession {
			// Broker waits outrank scheduler state. Client tools are blocking but do
			// not imply that a human approval notification is needed.
			switch {
			case activity.PendingApprovalCount > 0:
				activity.State = "waiting_approval"
			case activity.PendingClientToolCount > 0:
				activity.State = "waiting_client_tool"
			case activity.State == "":
				activity.State = "idle"
			}
			activity.TurnID = activity.ActiveTurnID // migration alias
			if activity.LatestTerminal != nil && activity.ActiveTurnID != "" && activity.LatestTerminal.TurnID == activity.ActiveTurnID {
				activity.State = terminalActivityState(activity.LatestTerminal.Kind)
				activity.QueuePosition = 0
			}
			activities = append(activities, activity)
		}
		sort.Slice(activities, func(i, j int) bool { return activities[i].SessionID < activities[j].SessionID })
		writeJSON(w, r, http.StatusOK, activityResponse{
			GeneratedAt: generatedAt.Format(time.RFC3339Nano),
			Cursor:      cursor,
			IsDelta:     isDelta && attentionSupported,
			Sessions:    activities,
		})
	})

	// ---- v1 CRUD: all read from Repository (SQLite) ----

	mux.HandleFunc("GET /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		metas, err := repo.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		refs := make([]ConversationRef, 0, len(metas))
		for _, m := range metas {
			refs = append(refs, ConversationRef{
				ID:            m.ID,
				WorkspacePath: m.WorkspacePath,
				Workspace:     workspaceDTO(m.WorkspacePath),
				Name:          effectiveName(m),
				TurnStatus:    m.TurnStatus,
				PausedAt:      m.PausedAt,
			})
		}
		writeJSON(w, r, http.StatusOK, refs)
	})

	mux.HandleFunc("POST /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		var req CreateConversationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Worktree != nil && req.Worktree.Managed {
			writeManagedWorktreeError(w, r, http.StatusNotImplemented, managedWorktreeErrorResponse{
				Code:            "managed_worktree_not_supported",
				Message:         "managed worktree provisioning is not enabled",
				ClientRequestID: req.ClientRequestID,
			})
			return
		}

		if req.WorkspacePath == "" {
			http.Error(w, `"workspace_path" is required`, http.StatusBadRequest)
			return
		}
		if req.ExecutionPolicy != "" && req.ExecutionPolicy != session.ExecutionPolicySharedWorkspace && req.ExecutionPolicy != session.ExecutionPolicyIsolatedWorktree && req.ExecutionPolicy != session.ExecutionPolicyReadOnly {
			http.Error(w, `"execution_policy" must be shared_workspace, isolated_worktree, or read_only`, http.StatusBadRequest)
			return
		}
		sess, err := repo.Create(r.Context(), req.WorkspacePath, req.WorkspaceExtID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if req.ExecutionPolicy != "" || req.WorkspaceID != "" || req.BaseWorkspaceID != "" {
			sess.SetExecutionPolicy(req.ExecutionPolicy)
			if req.ExecutionPolicy == "" {
				sess.SetExecutionPolicy(session.ExecutionPolicySharedWorkspace)
			}
			if sess.Metadata == nil {
				sess.Metadata = map[string]any{}
			}
			sess.Metadata[session.MetaWorkspaceID] = req.WorkspaceID
			sess.Metadata[session.MetaBaseWorkspaceID] = req.BaseWorkspaceID
			if err := repo.Save(r.Context(), sess); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(w, r, http.StatusCreated, ConversationRef{
			ID:            sess.ID,
			WorkspacePath: sess.WorkspacePath,
			Workspace:     workspaceDTO(sess.WorkspacePath),
		})
	})

	mux.HandleFunc("GET /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		recs, ok := loadEvents(r.Context(), eventStore, repo, id, w)
		if !ok {
			return
		}
		detail := ConversationDetail{ID: id}
		if s, err := repo.Load(r.Context(), id); err == nil {
			detail.WorkspacePath = s.WorkspacePath
			detail.Workspace = workspaceDTO(s.WorkspacePath)
			detail.Name = s.Name
			if s.Workspace.Root != "" {
				detail.WorkspaceRef = &WorkspaceRefDTO{
					Root:  s.Workspace.Root,
					Rel:   s.Workspace.Rel,
					ExtID: s.Workspace.ExtID,
				}
			}
			if nr, err := repo.NeedsRebind(r.Context(), id); err == nil {
				detail.NeedsRebind = nr
			}
		}
		for _, rec := range recs {
			if detail.CreatedAt == "" {
				detail.CreatedAt = rec.At.UTC().Format(rfc3339Millis)
			}
			detail.UpdatedAt = rec.At.UTC().Format(rfc3339Millis)
			switch rec.Kind {
			case string(agent.EventTurnStarted):
				detail.TurnCount++
				detail.MessageCount++
			case string(agent.EventTurnFinished):
				detail.MessageCount++
			}
		}
		writeJSON(w, r, http.StatusOK, detail)
	})

	mux.HandleFunc("GET /v1/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		recs, ok := loadEvents(r.Context(), eventStore, repo, id, w)
		if !ok {
			return
		}
		msgs := []MessageView{}
		for _, rec := range recs {
			var role string
			switch rec.Kind {
			case string(agent.EventTurnStarted):
				role = "user"
			case string(agent.EventTurnFinished):
				role = "assistant"
			default:
				continue
			}
			if ev, ok := decodeStoredEvent(rec); ok {
				msgs = append(msgs, MessageView{Seq: len(msgs), Role: role, Content: ev.Text})
			}
		}
		writeJSON(w, r, http.StatusOK, msgs)
	})

	mux.HandleFunc("GET /v1/conversations/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// ?since=<seq> replays only events after the client's last seen seq (v1.2
		// §4) — the incremental catch-up a client runs on reconnect. Absent/invalid
		// => full replay.
		recs, ok := loadEventsSince(r.Context(), eventStore, repo, id, parseSince(r), w)
		if !ok {
			return
		}
		frames := make([]json.RawMessage, 0, len(recs))
		for _, rec := range recs {
			ev, ok := decodeStoredEvent(rec)
			if !ok {
				continue
			}
			// The seq is the row identity, not part of the stored payload — stamp it
			// so replayed frames carry the same seq the live stream reported.
			ev.Seq = rec.Seq
			if frame, err := Encode(ev, "", ""); err == nil {
				frames = append(frames, frame)
			}
		}
		writeJSON(w, r, http.StatusOK, frames)
	})

	mux.HandleFunc("GET /v1/conversations/{id}/assets/{asset_id}/preview", func(w http.ResponseWriter, r *http.Request) {
		resp, err := previewConversationAsset(r.Context(), eventStore, repo, r.PathValue("id"), r.PathValue("asset_id"))
		if err != nil {
			writeAssetError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/conversations/{id}/assets/{asset_id}/content", func(w http.ResponseWriter, r *http.Request) {
		resp, err := contentConversationAsset(r.Context(), eventStore, repo, r.PathValue("id"), r.PathValue("asset_id"))
		if err != nil {
			writeAssetError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/conversations/{id}/assets/{asset_id}/blob", func(w http.ResponseWriter, r *http.Request) {
		if err := serveConversationAssetBlob(r.Context(), w, r, eventStore, repo, r.PathValue("id"), r.PathValue("asset_id")); err != nil {
			writeAssetError(w, r, err)
			return
		}
	})

	mux.HandleFunc("GET /v1/conversations/{id}/assets/{asset_id}/thumbnail", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "asset thumbnails are not implemented", http.StatusNotImplemented)
	})

	// ---- WebSocket: TransportSession backed by TurnExecutor ----
	// Declared before the DELETE handler so it can call ws.RemoveApprover.

	wsResolve := func(r *http.Request) (Session, error) {
		id := r.PathValue("id")
		// Verify the session exists in the Repository.
		if _, err := repo.Load(r.Context(), id); err != nil {
			return nil, fmt.Errorf("conversation %q not found", id)
		}
		return conversation.NewTransportSession(id, executor), nil
	}
	ws = &WSHandler{
		Resolve:         wsResolve,
		ServerName:      opts.ServerName,
		Capabilities:    opts.Capabilities,
		Accept:          opts.Accept,
		Granter:         opts.Granter,
		Prompts:         opts.Prompts,
		CredentialStore: opts.CredentialStore,
	}
	mux.Handle("GET /v1/conversations/{id}/stream", ws)
	mux.Handle("GET /v2/conversations/{id}/stream", ws)

	// ---- P8.7 Phase C: background job child streams (read-only) ----
	//
	// A job is not a conversation: it has no turns, no messages, no command
	// plane, and never appears in GET /v1/conversations (that list reads the
	// sessions table; jobs only write session_events). These two endpoints mirror
	// the conversation events + stream shapes so the client reuses its WireFrame
	// decoder — backlog over /events (with ?since=), live over /stream.
	mux.HandleFunc("GET /v1/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		recs, ok := loadEventsSince(r.Context(), eventStore, repo, id, parseSince(r), w)
		if !ok {
			return
		}
		frames := make([]json.RawMessage, 0, len(recs))
		for _, rec := range recs {
			ev, ok := decodeStoredEvent(rec)
			if !ok {
				continue
			}
			ev.Seq = rec.Seq
			if frame, err := Encode(ev, "", ""); err == nil {
				frames = append(frames, frame)
			}
		}
		writeJSON(w, r, http.StatusOK, frames)
	})

	jobStream := &JobStreamHandler{
		ServerName:   opts.ServerName,
		Capabilities: opts.Capabilities,
		Accept:       opts.Accept,
		Resolve: func(r *http.Request) (Subscriber, error) {
			id := r.PathValue("id")
			// A job is "known" once it has emitted at least job_started (persisted
			// inside Registry.Start, before the tool even returns the id) — so any
			// id a client could hold already has events. Empty => unknown => 404.
			recs, err := eventStore.ReplaySince(r.Context(), id, 0)
			if err != nil {
				return nil, err
			}
			if len(recs) == 0 {
				return nil, fmt.Errorf("job %q not found", id)
			}
			return conversation.NewChildStreamSubscriber(id, executor), nil
		},
	}
	mux.Handle("GET /v1/jobs/{id}/stream", jobStream)

	// GET /v1/prompts — list the MCP prompts a client can invoke via invoke_prompt.
	// Server-wide (MCP servers are global), so it needs no conversation id.
	mux.HandleFunc("GET /v1/prompts", func(w http.ResponseWriter, r *http.Request) {
		var specs []mcp.PromptSpec
		if opts.Prompts != nil {
			specs = opts.Prompts.Prompts()
		}
		writeJSON(w, r, http.StatusOK, toPromptsResponse(specs))
	})

	mux.HandleFunc("PATCH /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			http.Error(w, `"name" is required`, http.StatusBadRequest)
			return
		}
		if len(body.Name) > 200 {
			http.Error(w, `"name" must not exceed 200 characters`, http.StatusBadRequest)
			return
		}
		if err := repo.UpdateName(r.Context(), id, body.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Reload to return the full ref.
		sess, err := repo.Load(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, http.StatusOK, ConversationRef{
			ID:            sess.ID,
			WorkspacePath: sess.WorkspacePath,
			Workspace:     workspaceDTO(sess.WorkspacePath),
			Name:          sess.Name,
		})
	})

	// Rebind re-supplies an external workspace's absolute path for this launch. The
	// host calls it in Phase 1 (HTTP, before the WS stream) when detail.needs_rebind
	// is true, so any turn's Load resolves the workspace correctly. No-race by design:
	// the stream is not yet open. See spec §6.2bis.
	mux.HandleFunc("POST /v1/conversations/{id}/rebind", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body RebindRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if body.WorkspacePath == "" {
			http.Error(w, `"workspace_path" is required`, http.StatusBadRequest)
			return
		}
		if err := repo.Rebind(r.Context(), id, body.WorkspacePath); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Clone a public GitHub repo into the workspace. Used by the host's "Import from
	// GitHub" flow before a conversation is created — the cloned directory lands under
	// the workspace root and is returned as a workspace_ref, reusing the same
	// relativize/re-anchor path as any other workspace project. See
	// docs/ios_github_clone_spec.md.
	mux.HandleFunc("POST /v1/repos/clone", func(w http.ResponseWriter, r *http.Request) {
		if opts.WorkspaceRoot == "" {
			writeJSON(w, r, http.StatusBadRequest, cloneErrorResponse{Error: "io_error", Message: "server has no workspace root configured"})
			return
		}
		var req CloneRepoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, r, http.StatusBadRequest, cloneErrorResponse{Error: "invalid_url", Message: "invalid JSON body"})
			return
		}
		res, err := repos.Clone(r.Context(), opts.WorkspaceRoot, repos.CloneOptions{
			URL: req.URL, Ref: req.Ref, Name: req.Name, Depth: req.Depth,
		})
		if err != nil {
			var ce *repos.CloneError
			if errors.As(err, &ce) {
				writeJSON(w, r, statusForCloneCode(ce.Code), cloneErrorResponse{Error: ce.Code, Message: ce.Err.Error()})
				return
			}
			writeJSON(w, r, http.StatusInternalServerError, cloneErrorResponse{Error: "io_error", Message: err.Error()})
			return
		}
		writeJSON(w, r, http.StatusCreated, CloneRepoResponse{
			WorkspacePath: res.AbsPath,
			WorkspaceRef:  &WorkspaceRefDTO{Root: session.RootWorkspace, Rel: res.Rel},
		})
	})

	mux.HandleFunc("DELETE /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Close and remove the session-scoped approver so blocked turns wake
		// and the approver is garbage-collected.
		ws.RemoveApprover(id)
		if err := repo.Delete(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/workspaces/{path}/mcp/reload", func(w http.ResponseWriter, r *http.Request) {
		if opts.WorkspaceReloader == nil {
			http.Error(w, "MCP reload not available", http.StatusNotFound)
			return
		}
		if err := opts.WorkspaceReloader(r.PathValue("path")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func writeManagedWorktreeError(w http.ResponseWriter, r *http.Request, status int, detail managedWorktreeErrorResponse) {
	Result(w, r, status, status*100, detail.Message, detail)
}

// loadEvents fetches a session's recorded events from the EventStore and resolves
// existence: a 404 is written when the id is unknown to both the event log and
// the Repository.
func loadEvents(ctx context.Context, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, id string, w http.ResponseWriter) ([]session.EventRecord, bool) {
	return loadEventsSince(ctx, eventStore, repo, id, 0, w)
}

// loadEventsSince is loadEvents with an incremental floor: sinceSeq > 0 replays
// only events after that seq (v1.2 §4), else the full log. The existence 404 is
// still resolved against the Repository when the (possibly filtered) result is
// empty, so an unknown id 404s and a known id with no newer events returns [].
func loadEventsSince(ctx context.Context, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, id string, sinceSeq int64, w http.ResponseWriter) ([]session.EventRecord, bool) {
	var recs []session.EventRecord
	var err error
	if sinceSeq > 0 {
		recs, err = eventStore.ReplaySince(ctx, id, sinceSeq)
	} else {
		recs, err = eventStore.Replay(ctx, id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	if len(recs) == 0 {
		if _, err := repo.Load(ctx, id); err != nil {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return nil, false
		}
	}
	return recs, true
}

// parseSince reads the ?since=<seq> query parameter as a non-negative int64. A
// missing or malformed value yields 0 (full replay).
func parseSince(r *http.Request) int64 {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// decodeStoredEvent unmarshals one persisted raw agent.Event payload.
func decodeStoredEvent(rec session.EventRecord) (agent.Event, bool) {
	var ev agent.Event
	if err := json.Unmarshal(rec.Payload, &ev); err != nil {
		return ev, false
	}
	return ev, true
}

// effectiveName returns the display name for a session Meta, preferring the
// persisted Name and falling back to Title (first user message truncation).
func effectiveName(m session.Meta) string {
	if m.Name != "" {
		return m.Name
	}
	return m.Title
}

func workspaceDTO(path string) *WorkspaceDTO {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	name := filepath.Base(clean)
	if name == "." || name == string(filepath.Separator) {
		name = clean
	}
	return &WorkspaceDTO{
		ID:          assets.WorkspaceID(clean),
		Name:        name,
		RootPath:    clean,
		RuntimeCWD:  clean,
		DisplayPath: name,
		Kind:        "local",
	}
}

// writeJSON writes a unified-envelope JSON response. All public API endpoints
// use this so every response has the {trace_id, code, msg, data} shape that
// matches the Agent Gateway format.
//
// For success (2xx), the value is the data payload.
// For errors (4xx/5xx), the value is included as data when it is a structured
// error (cloneErrorResponse), otherwise a generic message is used.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	switch {
	case status >= 200 && status < 300:
		writeResponse(w, status, apiResponse{
			TraceID: traceID(r),
			Code:    0,
			Msg:     "success",
			Data:    v,
		})
	default:
		msg := http.StatusText(status)
		if e, ok := v.(cloneErrorResponse); ok {
			msg = e.Message
		}
		Result(w, r, status, status*100, msg, v)
	}
}
