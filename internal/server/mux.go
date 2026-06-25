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
// frozen into the contract now even though the skeleton ignores it: macOS / iOS /
// web will always send a workspace when creating a conversation, so the shape must
// not change under them later (a `{}` API today would force a Swift DTO rewrite
// when the field appears).
type CreateConversationRequest struct {
	WorkspacePath string `json:"workspace_path,omitempty"`
}

// ConversationRef is the minimal conversation descriptor returned by create and
// list.
type ConversationRef struct {
	ID string `json:"id"`
}

// ConversationDetail is GET /v1/conversations/{id}. v1 derives everything from the
// recorded event stream (no session row needed): counts and timestamps come from
// the events. summary/model arrive with persistence (P1-B).
type ConversationDetail struct {
	ID           string `json:"id"`
	TurnCount    int    `json:"turn_count"`
	MessageCount int    `json:"message_count"`
	CreatedAt    string `json:"created_at,omitempty"` // first event's `at`
	UpdatedAt    string `json:"updated_at,omitempty"` // last event's `at`
}

// MessageView is one entry of GET /v1/conversations/{id}/messages. v1 reconstructs
// the conversational backbone from turn_started (user) / turn_finished (assistant)
// events. Tool/system messages and full fidelity arrive with persistence (P1-B).
type MessageView struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

// EventSource is the read side the conversation-history endpoints need: the
// recorded, replayable event stream of a session. *session.SQLiteStore satisfies
// it. The stored payload is a raw agent.Event (not wire form), so the read
// endpoints re-encode it through the same toWire path the live stream uses — so
// history and the live WS feed are byte-for-byte the same shape.
type EventSource interface {
	SessionEvents(ctx context.Context, sessionID string) ([]session.EventRecord, error)
}

// MuxOptions configures the HTTP surface.
type MuxOptions struct {
	// ServerName is reported in the WebSocket hello handshake.
	ServerName string
	// Accept carries WebSocket origin policy. Nil = same-origin only.
	Accept *websocket.AcceptOptions
}

// NewMux builds the HTTP surface of `codeagent serve` over a conversation Manager
// and the recorded event log:
//
//	GET  /healthz                            liveness
//	GET  /v1/conversations                   list (ids only)
//	POST /v1/conversations                   create -> {id}
//	GET  /v1/conversations/{id}              detail (derived from events)
//	GET  /v1/conversations/{id}/messages     conversational backbone (derived)
//	GET  /v1/conversations/{id}/events       recorded events, re-encoded to wire v1
//	GET  /v1/conversations/{id}/stream       upgrade to the agent-wire WebSocket
//
// The read endpoints let a client restore a Timeline on reopen (history) and then
// attach the WebSocket for the live delta — without them, a reopened page is blank.
func NewMux(mgr *conversation.Manager, events EventSource, opts MuxOptions) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /v1/conversations", func(w http.ResponseWriter, _ *http.Request) {
		refs := []ConversationRef{}
		for _, c := range mgr.List() {
			refs = append(refs, ConversationRef{ID: c.ID()})
		}
		writeJSON(w, http.StatusOK, refs)
	})

	mux.HandleFunc("POST /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		// Body is optional and its only field (workspace_path) is ignored in the
		// skeleton, so a decode error (e.g. an empty body) is not fatal.
		var req CreateConversationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		c, err := mgr.Create(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, ConversationRef{ID: c.ID()})
	})

	mux.HandleFunc("GET /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		recs, ok := loadEvents(r.Context(), events, mgr, id, w)
		if !ok {
			return
		}
		detail := ConversationDetail{ID: id}
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
		recs, ok := loadEvents(r.Context(), events, mgr, id, w)
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
		recs, ok := loadEvents(r.Context(), events, mgr, id, w)
		if !ok {
			return
		}
		frames := make([]json.RawMessage, 0, len(recs))
		for _, rec := range recs {
			ev, ok := decodeStoredEvent(rec)
			if !ok {
				continue
			}
			// Re-encode through the live wire path: history == live stream shape.
			// Historical events carry no event_id/parent_session_id (those are
			// stamped at WS-send time, not persisted), so they are omitted.
			if frame, err := Encode(ev, "", ""); err == nil {
				frames = append(frames, frame)
			}
		}
		writeJSON(w, http.StatusOK, frames)
	})

	ws := &WSHandler{
		Resolve: func(r *http.Request) (Session, error) {
			id := r.PathValue("id")
			c, ok := mgr.Get(id)
			if !ok {
				return nil, fmt.Errorf("conversation %q not found", id)
			}
			return c, nil
		},
		ServerName: opts.ServerName,
		Accept:     opts.Accept,
	}
	mux.Handle("GET /v1/conversations/{id}/stream", ws)

	return mux
}

// loadEvents fetches a session's recorded events and resolves existence: a 404 is
// written when the id is unknown to both the event log and the live registry (so a
// brand-new, turn-less conversation still reads as an empty—not missing—history).
func loadEvents(ctx context.Context, events EventSource, mgr *conversation.Manager, id string, w http.ResponseWriter) ([]session.EventRecord, bool) {
	recs, err := events.SessionEvents(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	if len(recs) == 0 {
		if _, live := mgr.Get(id); !live {
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
