package conversation

import (
	"sync"

	"code-agent/internal/agent"
)

// hub is a fan-out agent.Emitter. Every event the Runner emits goes to an
// optional downstream sink (the emitter the Runner was built with — typically the
// persistence/telemetry recorder, which must stay lossless) and to every live
// subscriber channel. This is the seam that turns the Runner's single Emitter
// into the multi-subscriber stream a server, a TUI, and a log can share at once.
type hub struct {
	downstream agent.Emitter // may be nil; receives every event synchronously

	mu     sync.Mutex
	subs   map[int]chan agent.Event
	next   int
	closed bool
}

func newHub(downstream agent.Emitter) *hub {
	return &hub{downstream: downstream, subs: make(map[int]chan agent.Event)}
}

// compile-time guarantee: a hub is itself a drop-in core Emitter.
var _ agent.Emitter = (*hub)(nil)

// Emit forwards to the downstream sink first (synchronous, lossless — persistence
// must not miss events), then to each subscriber. Subscriber delivery is
// non-blocking: a full buffer drops the event for that subscriber only, so a slow
// or gone consumer can never stall the agent loop. Durable replay is the event
// store's job (and seq-based resume is reserved for protocol v2), not the live
// fan-out's.
func (h *hub) Emit(e agent.Event) {
	if h.downstream != nil {
		h.downstream.Emit(e)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default: // subscriber buffer full: drop rather than block the loop
		}
	}
}

// subscribe registers a new subscriber and returns its event channel plus an
// unsubscribe func. The channel is buffered so a brief consumer stall is
// absorbed; unsubscribe is idempotent and closes the channel.
func (h *hub) subscribe() (<-chan agent.Event, func()) {
	ch := make(chan agent.Event, 256)
	h.mu.Lock()
	if h.closed {
		// The conversation is torn down: hand back an already-closed channel so a
		// late subscriber observes end-of-stream immediately rather than hanging.
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(c)
			}
		})
	}
	return ch, unsub
}

// closeAll closes every subscriber channel and marks the hub closed, so live
// bridges observe end-of-stream and any later subscribe gets an already-closed
// channel. The downstream sink is left untouched. Idempotent.
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}
