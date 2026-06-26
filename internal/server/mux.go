package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"

	"github.com/coder/websocket"
)

// CreateConversationRequest is the POST /v1/conversations body. workspace_path is
// the client's desired project root directory; an empty workspace_path means "use
// the server's default workspace."
type CreateConversationRequest struct {
	WorkspacePath string `json:"workspace_path,omitempty"`
}

// ConversationRef is the minimal conversation descriptor returned by create and
// list.
type ConversationRef struct {
	ID            string `json:"id"`
	WorkspacePath string `json:"workspace_path"`
}

// ConversationDetail is GET /v1/conversations/{id}. Counts and timestamps are
// derived from the recorded event stream; workspace_path comes from the session
// metadata (identity, not an event).
type ConversationDetail struct {
	ID            string `json:"id"`
	WorkspacePath string `json:"workspace_path"`
	TurnCount     int    `json:"turn_count"`
	MessageCount  int    `json:"message_count"`
	CreatedAt     string `json:"created_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
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
	// Accept carries WebSocket origin policy. Nil = same-origin only.
	Accept *websocket.AcceptOptions
}

// NewMux builds the HTTP surface of `codeagent serve`:
//
//	GET  /healthz                            liveness
//	GET  /v1/conversations                   list (from Repository / SQLite)
//	POST /v1/conversations                   create → {id} (writes SQLite, no Runtime)
//	GET  /v1/conversations/{id}              detail (derived from events + repo)
//	GET  /v1/conversations/{id}/messages     conversational backbone (derived)
//	GET  /v1/conversations/{id}/events       recorded events, re-encoded to wire v1
//	DELETE /v1/conversations/{id}            delete session + events
//	GET  /v1/conversations/{id}/stream       upgrade via TurnExecutor/TransportSession
//	GET  /v2/conversations/{id}/stream       same (alias)
//
// All read endpoints query the Repository directly — conversation list survives
// server restart. The WS path uses TurnExecutor: a TransportSession is created
// per connection, wrapping the executor — no permanent in-memory Conversation.
func NewMux(repo conversation.ConversationRepository, eventStore conversation.ConversationEventStore, executor *conversation.TurnExecutor, opts MuxOptions) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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
			})
		}
		writeJSON(w, http.StatusOK, refs)
	})

	mux.HandleFunc("POST /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		var req CreateConversationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		sess, err := repo.Create(r.Context(), req.WorkspacePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, ConversationRef{
			ID:            sess.ID,
			WorkspacePath: sess.WorkspacePath,
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
		writeJSON(w, http.StatusOK, detail)
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
		writeJSON(w, http.StatusOK, msgs)
	})

	mux.HandleFunc("GET /v1/conversations/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		recs, ok := loadEvents(r.Context(), eventStore, repo, id, w)
		if !ok {
			return
		}
		frames := make([]json.RawMessage, 0, len(recs))
		for _, rec := range recs {
			ev, ok := decodeStoredEvent(rec)
			if !ok {
				continue
			}
			if frame, err := Encode(ev, "", ""); err == nil {
				frames = append(frames, frame)
			}
		}
		writeJSON(w, http.StatusOK, frames)
	})

	mux.HandleFunc("DELETE /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := repo.Delete(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- WebSocket: TransportSession backed by TurnExecutor ----

	wsResolve := func(r *http.Request) (Session, error) {
		id := r.PathValue("id")
		// Verify the session exists in the Repository.
		if _, err := repo.Load(r.Context(), id); err != nil {
			return nil, fmt.Errorf("conversation %q not found", id)
		}
		return conversation.NewTransportSession(id, executor), nil
	}
	ws := &WSHandler{
		Resolve:    wsResolve,
		ServerName: opts.ServerName,
		Accept:     opts.Accept,
	}
	mux.Handle("GET /v1/conversations/{id}/stream", ws)
	mux.Handle("GET /v2/conversations/{id}/stream", ws)

	return mux
}

// loadEvents fetches a session's recorded events from the EventStore and resolves
// existence: a 404 is written when the id is unknown to both the event log and
// the Repository.
func loadEvents(ctx context.Context, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, id string, w http.ResponseWriter) ([]session.EventRecord, bool) {
	recs, err := eventStore.Replay(ctx, id)
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

// decodeStoredEvent unmarshals one persisted raw agent.Event payload.
func decodeStoredEvent(rec session.EventRecord) (agent.Event, bool) {
	var ev agent.Event
	if err := json.Unmarshal(rec.Payload, &ev); err != nil {
		return ev, false
	}
	return ev, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
