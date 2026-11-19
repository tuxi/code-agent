package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

var errQueueUnauthorized = errors.New("user queue authorization unavailable")
var errQueueUnsupported = errors.New("persistent user queue unavailable")

// Scope must authenticate this session's provider principal. A loopback Server
// Access Token alone is not a provider identity or a grant to another session.
type queueContextService struct {
	repo        conversation.ConversationRepository
	models      conversation.ModelResolver
	permissions PermissionService
	scope       func(context.Context, string) (string, error)
}

func (s queueContextService) available() bool {
	return s.repo != nil && s.models != nil && s.permissions != nil && s.scope != nil
}
func (s queueContextService) authorizedScope(ctx context.Context, sid string) (string, error) {
	if !s.available() {
		return "", errQueueUnsupported
	}
	scope, err := s.scope(ctx, sid)
	if err != nil || strings.TrimSpace(scope) == "" {
		return "", errQueueUnauthorized
	}
	return scope, nil
}
func (s queueContextService) resolve(ctx context.Context, sid, wire, effort string) (session.QueueExecutionContext, error) {
	empty := session.QueueExecutionContext{}
	scope, err := s.authorizedScope(ctx, sid)
	if err != nil {
		return empty, err
	}
	sess, err := s.repo.Load(ctx, sid)
	if err != nil {
		return empty, err
	}
	if !sess.ArchivedAt.IsZero() {
		return empty, session.ErrUserQueueUnavailable
	}
	if strings.TrimSpace(wire) == "" {
		return empty, session.ErrInvalidQueuedMessage
	}
	if validator, ok := s.models.(interface {
		ValidateExecutorModel(context.Context, string) error
	}); ok {
		if err := validator.ValidateExecutorModel(ctx, wire); err != nil {
			return empty, err
		}
	}
	mc, err := s.models.ResolveModel(wire)
	if err != nil {
		return empty, err
	}
	if mc == nil || mc.Model == "" {
		return empty, session.ErrInvalidQueuedMessage
	}
	if effort == "" {
		effort = mc.ReasoningEffort
	}
	if effort != "" {
		if effort == model.ReasoningEffortOff {
			if mc.Catalog.CanDisableReasoning != nil && !*mc.Catalog.CanDisableReasoning {
				return empty, session.ErrInvalidQueuedMessage
			}
		} else {
			if mc.Catalog.SupportsReasoning != nil && !*mc.Catalog.SupportsReasoning {
				return empty, session.ErrInvalidQueuedMessage
			}
			if len(mc.Catalog.SupportedReasoningEfforts) > 0 {
				found := false
				for _, v := range mc.Catalog.SupportedReasoningEfforts {
					if v == effort {
						found = true
					}
				}
				if !found {
					return empty, session.ErrInvalidQueuedMessage
				}
			}
		}
	}
	mode, err := s.permissions.EffectiveMode(sess.WorkspacePath)
	if err != nil {
		return empty, err
	}
	if !validApprovalMode(mode) {
		return empty, session.ErrUserQueueUnavailable
	}
	return session.QueueExecutionContext{WireModel: wire, ResolvedModel: mc.Model, ReasoningEffort: effort, WorkspaceRoot: sess.Workspace.Root, WorkspaceRel: sess.Workspace.Rel, WorkspaceExtID: sess.Workspace.ExtID, WorkspacePath: sess.WorkspacePath, AuthorizationScope: scope, ApprovalMode: mode}, nil
}
func (s queueContextService) revalidate(ctx context.Context, sid string, f session.QueueExecutionContext) (session.QueueExecutionContext, error) {
	return s.resolve(ctx, sid, f.WireModel, f.ReasoningEffort)
}

type queueHTTPService struct {
	store    session.UserQueueStore
	contexts queueContextService
	// This is intentionally never set by production assembly yet: automatic
	// consumption and interrupted running-turn recovery must be ready together.
	consumerReady bool
	wake          func(string)
}

func (s *queueHTTPService) ready() bool {
	return s != nil && s.store != nil && s.contexts.available() && s.consumerReady
}
func (s *queueHTTPService) query(ctx context.Context, sid, rid string) (session.QueuedMessageReceipt, error) {
	scope, err := s.contexts.authorizedScope(ctx, sid)
	if err != nil {
		return session.QueuedMessageReceipt{}, err
	}
	receipt, err := s.store.QueuedUserMessage(ctx, sid, rid)
	if err != nil {
		return receipt, err
	}
	if receipt.Input.Context.AuthorizationScope != scope {
		return session.QueuedMessageReceipt{}, errQueueUnauthorized
	}
	return receipt, nil
}

// Mounted under the existing authenticated NewMux only after production
// assembly is ready. This batch exercises it through an isolated test mux.
func registerUserQueueRoutes(mux *http.ServeMux, s *queueHTTPService, enabled bool) bool {
	ready := enabled && s.ready()
	respond := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	failure := func(w http.ResponseWriter, err error) {
		status, code := 500, "queue_storage_error"
		switch {
		case errors.Is(err, errQueueUnsupported):
			status, code = 501, "queue_unsupported"
		case errors.Is(err, errQueueUnauthorized):
			status, code = 403, "queue_unauthorized"
		case errors.Is(err, session.ErrInvalidQueuedMessage):
			status, code = 400, "invalid_queue_message"
		case errors.Is(err, session.ErrQueuedMessageConflict):
			status, code = 409, "request_conflict"
		case errors.Is(err, session.ErrUserQueueRevision):
			status, code = 409, "queue_revision_changed"
		case errors.Is(err, session.ErrUserQueueUnavailable):
			status, code = 409, "queue_unavailable"
		case errors.Is(err, sql.ErrNoRows):
			status, code = 404, "queue_message_not_found"
		}
		respond(w, status, map[string]string{"code": code})
	}
	mux.HandleFunc("POST /v1/conversations/{id}/queue", func(w http.ResponseWriter, r *http.Request) {
		if !ready {
			failure(w, errQueueUnsupported)
			return
		}
		var body struct {
			RequestID string `json:"request_id"`
			Text      string `json:"text"`
			Model     string `json:"model"`
			Effort    string `json:"reasoning_effort"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil {
			failure(w, session.ErrInvalidQueuedMessage)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			failure(w, session.ErrInvalidQueuedMessage)
			return
		}
		sid := r.PathValue("id")
		existing, err := s.query(r.Context(), sid, body.RequestID)
		if err == nil {
			if existing.Input.Text != body.Text || existing.Input.Context.WireModel != body.Model || existing.Input.SelectionEffort != body.Effort {
				failure(w, session.ErrQueuedMessageConflict)
				return
			}
			if s.wake != nil {
				s.wake(sid)
			}
			respond(w, 202, existing)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			failure(w, err)
			return
		}
		selection, err := s.contexts.resolve(r.Context(), sid, body.Model, body.Effort)
		if err != nil {
			failure(w, err)
			return
		}
		receipt, _, err := s.store.EnqueueUserMessage(r.Context(), session.QueuedMessage{SessionID: sid, RequestID: body.RequestID, Text: body.Text, SelectionEffort: body.Effort, Context: selection})
		if errors.Is(err, session.ErrQueuedMessageConflict) {
			// Another identical HTTP submission may have frozen settings just
			// before this one, while the server configuration was changing.
			previous, queryErr := s.query(r.Context(), sid, body.RequestID)
			if queryErr == nil && previous.Input.Text == body.Text && previous.Input.Context.WireModel == body.Model && previous.Input.SelectionEffort == body.Effort {
				respond(w, 202, previous)
				return
			}
		}
		if err != nil {
			failure(w, err)
			return
		}
		if s.wake != nil {
			s.wake(sid)
		}
		respond(w, 202, receipt)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/queue/{request_id}", func(w http.ResponseWriter, r *http.Request) {
		if !ready {
			failure(w, errQueueUnsupported)
			return
		}
		receipt, err := s.query(r.Context(), r.PathValue("id"), r.PathValue("request_id"))
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, receipt)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/queue", func(w http.ResponseWriter, r *http.Request) {
		if !ready {
			failure(w, errQueueUnsupported)
			return
		}
		sid := r.PathValue("id")
		scope, err := s.contexts.authorizedScope(r.Context(), sid)
		if err != nil {
			failure(w, err)
			return
		}
		values := r.URL.Query()
		limit := 0
		if raw := values.Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil {
				failure(w, session.ErrInvalidQueuedMessage)
				return
			}
		}
		var cursor *session.UserQueueCursor
		if values.Has("revision") || values.Has("after_sequence") {
			revision, e1 := strconv.ParseInt(values.Get("revision"), 10, 64)
			after, e2 := strconv.ParseInt(values.Get("after_sequence"), 10, 64)
			if e1 != nil || e2 != nil {
				failure(w, session.ErrInvalidQueuedMessage)
				return
			}
			cursor = &session.UserQueueCursor{SessionID: sid, Revision: revision, AfterSequence: after}
		}
		page, err := s.store.ListUserQueue(r.Context(), sid, cursor, limit)
		if err != nil {
			failure(w, err)
			return
		}
		for _, item := range page.Items {
			if item.Input.Context.AuthorizationScope != scope {
				failure(w, errQueueUnauthorized)
				return
			}
		}
		_, page.Controls = s.store.(session.UserQueueControlStore)
		// Reopening the queue also recovers an enabled queue after a daemon
		// restart. Wake rechecks durable state and never resumes a paused queue.
		if page.State == "enabled" && len(page.Items) > 0 && s.wake != nil {
			s.wake(sid)
		}
		respond(w, 200, page)
	})
	mux.HandleFunc("POST /v1/conversations/{id}/queue/control", func(w http.ResponseWriter, r *http.Request) {
		if !ready {
			failure(w, errQueueUnsupported)
			return
		}
		store, ok := s.store.(session.UserQueueControlStore)
		if !ok {
			failure(w, errQueueUnsupported)
			return
		}
		var change session.UserQueueControl
		var extra any
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&change) != nil || decoder.Decode(&extra) != io.EOF {
			failure(w, session.ErrInvalidQueuedMessage)
			return
		}
		sid := r.PathValue("id")
		scope, err := s.contexts.authorizedScope(r.Context(), sid)
		if err == nil {
			err = store.ControlUserQueue(r.Context(), sid, scope, change)
		}
		if err != nil {
			failure(w, err)
			return
		}
		if s.wake != nil {
			s.wake(sid)
		}
		respond(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	})
	return ready
}

const userQueueCapability = "user_queue_text_v1"

type LocalUserQueueStore interface {
	session.UserQueueStore
	conversation.QueueExecutionStore
}

// LocalUserQueue is scoped to one authenticated, loopback runtime owner.
// The daemon owns its lifetime and schedules work outside HTTP requests.
type LocalUserQueue struct {
	service  queueHTTPService
	ctx      context.Context
	executor *conversation.TurnExecutor
	store    LocalUserQueueStore
	mu       sync.Mutex
	pending  map[string]bool
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	closed   bool
}

func NewLocalUserQueue(ctx context.Context, executor *conversation.TurnExecutor, store LocalUserQueueStore, repo conversation.ConversationRepository, models conversation.ModelResolver, permissions PermissionService, scope string) *LocalUserQueue {
	if executor == nil || store == nil || scope == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	q := &LocalUserQueue{ctx: ctx, cancel: cancel, executor: executor, store: store, pending: map[string]bool{}}
	q.service = queueHTTPService{store: store, consumerReady: true, contexts: queueContextService{repo: repo, models: models, permissions: permissions, scope: func(ctx context.Context, sid string) (string, error) {
		if !localQueueOwner(ctx) || !executor.UsesPrivateQueueCredentials(sid) {
			return "", errQueueUnauthorized
		}
		return scope, nil
	}}}
	if !q.service.ready() {
		cancel()
		return nil
	}
	q.service.wake = q.Wake
	executor.ConfigureUserQueueWakeup(q.Wake)
	return q
}

func (q *LocalUserQueue) Wake(sid string) {
	if q == nil || q.ctx.Err() != nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	_, running := q.pending[sid]
	q.pending[sid] = true
	if running {
		return
	}
	q.workers.Add(1)
	go func() { defer q.workers.Done(); q.drain(sid) }()
}

func (q *LocalUserQueue) drain(sid string) {
	ctx := context.WithValue(q.ctx, runtimeOwnerAuthContextKey{}, true)
	for {
		q.mu.Lock()
		q.pending[sid] = false
		q.mu.Unlock()
		count := 0
		if !q.executor.HasActivity(sid) {
			count, _ = q.executor.DrainUserQueue(ctx, sid, q.store, q.service.contexts.revalidate, 16)
		}
		q.mu.Lock()
		if ctx.Err() != nil || (count < 16 && !q.pending[sid]) {
			delete(q.pending, sid)
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
	}
}

func (q *LocalUserQueue) SessionReady(ctx context.Context, sid string) {
	if q == nil {
		return
	}
	if _, err := q.service.contexts.authorizedScope(ctx, sid); err == nil {
		q.Wake(sid)
	}
}

func (q *LocalUserQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cancel()
	q.mu.Unlock()
	q.workers.Wait()
}

func localQueueOwner(ctx context.Context) bool {
	owner, _ := ctx.Value(runtimeOwnerAuthContextKey{}).(bool)
	shared, _ := ctx.Value(sharedDeviceAuthContextKey{}).(bool)
	return owner && !shared
}
