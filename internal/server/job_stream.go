package server

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// JobStreamHandler serves a background job's own event stream over WebSocket
// (P8.7 Phase C): GET /v1/jobs/{id}/stream. Unlike WSHandler it is READ-ONLY —
// a job is observed, not driven — so there is no approver, no client-tool
// waiter, and no inbound command plane; the read loop exists only to detect
// disconnect. Frames are byte-identical to the conversation stream (same Bridge
// + Encode), so a client reuses its WireFrame decoder (client condition (a)).
//
// Backlog is fetched separately over GET /v1/jobs/{id}/events (same REST shape
// as the conversation events endpoint); this socket is live-only, the same
// split the conversation stream uses. A client reconnecting de-dupes live
// frames against the backlog's max seq.
type JobStreamHandler struct {
	// Resolve maps a request to a read-only Subscriber for the job's stream. A
	// nil/error result is reported as 404 (unknown job id).
	Resolve func(r *http.Request) (Subscriber, error)

	// ServerName is reported in the hello handshake. Defaults to "codeagent".
	ServerName string

	// Capabilities is the server capability list declared in the hello handshake.
	Capabilities []string

	// WriteTimeout bounds a single frame write. Zero uses a 30s default.
	WriteTimeout time.Duration

	// Accept carries origin policy / subprotocols. Nil = same-origin only.
	Accept *websocket.AcceptOptions
}

func (h *JobStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub, err := h.Resolve(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, h.Accept)
	if err != nil {
		return // Accept already wrote the error response
	}
	defer conn.CloseNow()

	// The request context is unreliable once hijacked; the stream runs on its own
	// context, cancelled by the read loop on disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writeTimeout := h.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 30 * time.Second
	}
	sink := &wsSink{conn: conn, ctx: ctx, writeTimeout: writeTimeout}

	// Read-only: drain inbound only to detect disconnect (no Router, no control
	// plane). Any frame the client sends is ignored.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	bridge := NewBridge(sink).WithCapabilities(h.Capabilities)
	runErr := bridge.Run(ctx, sub, h.serverName())
	if runErr == nil || ctx.Err() != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	conn.Close(websocket.StatusInternalError, "stream error")
}

func (h *JobStreamHandler) serverName() string {
	if h.ServerName != "" {
		return h.ServerName
	}
	return "codeagent"
}
