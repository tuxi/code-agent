package conversation

import (
	"testing"

	"code-agent/internal/agent"
)

// recEmitter records every event it receives. Used as a hub downstream sink.
type recEmitter struct{ got []agent.Event }

func (r *recEmitter) Emit(e agent.Event) { r.got = append(r.got, e) }

func TestHubFanOutAndDownstream(t *testing.T) {
	down := &recEmitter{}
	h := newHub(down)
	ch1, _ := h.subscribe()
	ch2, _ := h.subscribe()

	h.Emit(agent.Event{Kind: agent.EventThinking, Text: "hi"})

	if len(down.got) != 1 || down.got[0].Text != "hi" {
		t.Fatalf("downstream did not receive event: %+v", down.got)
	}
	for i, ch := range []<-chan agent.Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Text != "hi" {
				t.Errorf("subscriber %d wrong event: %+v", i, got)
			}
		default:
			t.Errorf("subscriber %d received no event", i)
		}
	}
}

func TestHubUnsubscribeClosesAndStops(t *testing.T) {
	h := newHub(nil)
	ch, unsub := h.subscribe()
	unsub()

	if _, ok := <-ch; ok {
		t.Error("channel should be closed after unsubscribe")
	}
	// Emitting after unsubscribe must not panic (no send on a closed channel).
	h.Emit(agent.Event{Kind: agent.EventThinking})
	// Unsubscribe is idempotent.
	unsub()
}

func TestHubCloseAllClosesSubscribersAndRejectsLate(t *testing.T) {
	h := newHub(nil)
	ch, _ := h.subscribe()

	h.closeAll()
	if _, open := <-ch; open {
		t.Error("existing subscriber channel not closed by closeAll")
	}

	// A subscribe after close gets an already-closed channel, not a hang.
	late, unsub := h.subscribe()
	if _, open := <-late; open {
		t.Error("late subscriber should receive an already-closed channel")
	}
	unsub() // must not panic

	// closeAll is idempotent.
	h.closeAll()
}

func TestHubDropsWhenFullNeverBlocks(t *testing.T) {
	h := newHub(nil)
	ch, _ := h.subscribe()

	// Far exceed the 256-deep buffer without draining: Emit must never block.
	for i := 0; i < 1000; i++ {
		h.Emit(agent.Event{Kind: agent.EventTokenDelta})
	}

	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			if n == 0 {
				t.Error("expected buffered events")
			}
			if n > 256 {
				t.Errorf("buffer exceeded its bound: %d", n)
			}
			return
		}
	}
}
