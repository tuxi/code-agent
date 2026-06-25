package server

import "code-agent/internal/agent"

// FrameSink is the minimal transport seam: send one encoded agent-wire frame. A
// WebSocket conn, an SSE writer, or a test buffer all satisfy it. The concrete
// WebSocket adapter (ws.go) is deliberately kept out of this file so the contract
// layer stays dependency-free.
type FrameSink interface {
	Send(frame []byte) error
}

// StreamEmitter implements agent.Emitter by encoding each core event into an
// agent-wire frame and writing it to a FrameSink. It is the single bridge from
// Layer 1 (agent.Event) to a wire transport — agent.Event never learns about JSON.
type StreamEmitter struct {
	sink            FrameSink
	parentSessionID string        // stamped on every frame; "" for the root session
	newID           func() string // injectable for tests; defaults to newEventID
	OnError         func(error)   // optional; invoked on encode/send failure
}

// compile-time guarantee that StreamEmitter is a drop-in core Emitter.
var _ agent.Emitter = (*StreamEmitter)(nil)

// NewStreamEmitter wires an emitter to a sink. Use WithParent for a subagent run.
func NewStreamEmitter(sink FrameSink) *StreamEmitter {
	return &StreamEmitter{sink: sink, newID: newEventID}
}

// WithParent configures the emitter to stamp parent_session_id on every frame,
// for a subagent run nested under a root session.
func (s *StreamEmitter) WithParent(parentSessionID string) *StreamEmitter {
	s.parentSessionID = parentSessionID
	return s
}

// Emit encodes the event and writes one frame. Errors are reported via OnError
// (if set) and otherwise dropped — a failed frame must never panic the agent loop.
func (s *StreamEmitter) Emit(e agent.Event) {
	frame, err := Encode(e, s.newID(), s.parentSessionID)
	if err != nil {
		s.fail(err)
		return
	}
	if err := s.sink.Send(frame); err != nil {
		s.fail(err)
	}
}

func (s *StreamEmitter) fail(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}
