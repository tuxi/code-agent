package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wsSink adapts a WebSocket connection to FrameSink: one agent-wire frame per
// text message. Two producers write to one connection — the Bridge (events) and
// the RemoteApprover (approval_request) — but coder/websocket allows only a
// single concurrent writer, so Send serializes them with a mutex. A per-write
// timeout keeps a stalled client from blocking a producer indefinitely.
type wsSink struct {
	conn         *websocket.Conn
	ctx          context.Context
	writeTimeout time.Duration

	mu sync.Mutex
}

var _ FrameSink = (*wsSink)(nil)

func (s *wsSink) Send(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := s.ctx
	if s.writeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.writeTimeout)
		defer cancel()
	}
	return s.conn.Write(ctx, websocket.MessageText, frame)
}

// WSHandler serves the agent-wire protocol over WebSocket. Each accepted
// connection resolves to a session, streams its events out via a Bridge, and
// reads the inbound control/message plane: send_message drives a turn,
// approval_response answers the blocking remote approver, cancel_turn cancels.
//
// The RemoteApprover is session-scoped: it survives WebSocket disconnects so a
// user switching between conversations does not lose pending approvals. On
// connect the handler updates the session approver's sink; on disconnect it only
// clears the sink — the approver stays registered on the session so a blocked
// turn waits for the next connection instead of being immediately denied.
type WSHandler struct {
	// Resolve maps a request to the session to drive. This is the seam the
	// Conversation Manager fills; a nil/error result is reported as 404.
	Resolve func(r *http.Request) (Session, error)

	// ServerName is reported in the hello handshake. Defaults to "codeagent".
	ServerName string

	// Capabilities is the server capability list declared in the hello handshake.
	// Nil means "no capabilities advertised".
	Capabilities []string

	// WriteTimeout bounds a single frame write. Zero uses a 30s default.
	WriteTimeout time.Duration

	// ApprovalTimeout bounds how long a side-effecting tool waits for the client's
	// verdict before denying. Zero uses a 2m default.
	ApprovalTimeout time.Duration

	// ClientToolTimeout bounds how long a client-executed tool waits for the
	// tool_result before timing out. Zero uses a 2m default.
	ClientToolTimeout time.Duration

	// Accept carries origin policy / subprotocols. Nil = coder/websocket defaults
	// (same-origin only).
	Accept *websocket.AcceptOptions

	// Session-scoped approvers that survive connection changes. Keyed by session ID.
	mu        sync.Mutex
	approvers map[string]*RemoteApprover
}

func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, err := h.Resolve(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, h.Accept)
	if err != nil {
		return // Accept already wrote the error response
	}
	defer conn.CloseNow()

	// The request context is unreliable once the connection is hijacked (see
	// websocket.Accept), so the stream runs on its own context. The read loop
	// cancels it the moment the client disconnects.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writeTimeout := h.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 30 * time.Second
	}
	sink := &wsSink{conn: conn, ctx: ctx, writeTimeout: writeTimeout}

	sessionID := r.PathValue("id")

	// Get or create the session-scoped RemoteApprover. On a first connection
	// this creates a new one; on a reconnect after a page switch this returns the
	// existing approver (still holding pending requests, if any) and re-sends
	// them over the new sink.
	approver := h.ensureApprover(sessionID, sink)

	sess.SetApprover(approver)
	sess.SetPlanApprover(approver)

	// v1.1: wire a RemoteToolResultWaiter so client-executed tools can deliver
	// results back into the blocked agent loop.
	waiter := NewRemoteToolResultWaiter()
	sess.SetClientToolWaiter(waiter)

	defer func() {
		waiter.CancelAll()            // wake all pending Wait calls on disconnect
		sess.SetClientToolWaiter(nil) // restore nil (no client to execute tools)
		// Clear the sink so future sends don't target a dead connection, but
		// do NOT close the approver or replace it with deny-all. Pending
		// approvals stay registered — the agent loop keeps blocking until
		// the user reconnects and responds, or the timeout fires.
		approver.ClearSink()
	}()

	// Inbound command/control routing is transport-agnostic (see Router); this read
	// loop owns only the WS read and disconnect detection.
	router := Router{Commands: sess, Approvals: approver, ToolResults: waiter}
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return // client closed or errored: stop the stream
			}
			router.Route(ctx, data)
		}
	}()

	bridge := NewBridge(sink).WithCapabilities(h.Capabilities)
	runErr := bridge.Run(ctx, sess, h.serverName())
	if runErr == nil || ctx.Err() != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	conn.Close(websocket.StatusInternalError, "stream error")
}

// ensureApprover returns the session-scoped RemoteApprover for sessionID,
// creating one on first use. On a reconnect it calls UpdateSink to re-send any
// pending approval requests over the new connection.
func (h *WSHandler) ensureApprover(sessionID string, sink FrameSink) *RemoteApprover {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.approvers == nil {
		h.approvers = make(map[string]*RemoteApprover)
	}
	ra, ok := h.approvers[sessionID]
	if !ok {
		ra = NewRemoteApprover(sink, h.approvalTimeout())
		h.approvers[sessionID] = ra
	} else {
		ra.UpdateSink(sink)
	}
	return ra
}

// RemoveApprover closes and removes the session-scoped approver for sessionID.
// Called on conversation deletion so blocked turns wake to a denial and the
// approver is garbage-collected.
func (h *WSHandler) RemoveApprover(sessionID string) {
	h.mu.Lock()
	ra, ok := h.approvers[sessionID]
	if ok {
		delete(h.approvers, sessionID)
	}
	h.mu.Unlock()
	if ok {
		ra.Close()
	}
}

func (h *WSHandler) serverName() string {
	if h.ServerName != "" {
		return h.ServerName
	}
	return "codeagent"
}

func (h *WSHandler) approvalTimeout() time.Duration {
	if h.ApprovalTimeout > 0 {
		return h.ApprovalTimeout
	}
	return 2 * time.Minute
}
