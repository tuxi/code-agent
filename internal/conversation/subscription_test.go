package conversation

import (
	"testing"

	"code-agent/internal/agent"
)

func TestSubscriptionManager_SubscribeEmit(t *testing.T) {
	m := NewSubscriptionManager()
	defer m.Shutdown()

	ch, unsub := m.Subscribe("s1")
	defer unsub()

	// Emit through the manager's publisher.
	emitter := m.Emitter("s1")
	e := agent.Event{SessionID: "s1", Kind: agent.EventThinking, Text: "hello"}
	emitter.Emit(e)

	// Should arrive on the subscriber channel.
	select {
	case received := <-ch:
		if received.Text != "hello" {
			t.Errorf("Text = %q, want hello", received.Text)
		}
	default:
		t.Error("expected event on channel, got none")
	}
}

func TestSubscriptionManager_MultipleSubscribers(t *testing.T) {
	m := NewSubscriptionManager()
	defer m.Shutdown()

	ch1, unsub1 := m.Subscribe("s1")
	defer unsub1()
	ch2, unsub2 := m.Subscribe("s1")
	defer unsub2()

	emitter := m.Emitter("s1")
	emitter.Emit(agent.Event{Kind: agent.EventThinking, Text: "fan"})

	// Both should receive.
	for i, ch := range []<-chan agent.Event{ch1, ch2} {
		select {
		case <-ch:
			// ok
		default:
			t.Errorf("subscriber %d missed event", i)
		}
	}
}

func TestSubscriptionManager_Unsubscribe(t *testing.T) {
	m := NewSubscriptionManager()
	defer m.Shutdown()

	ch, unsub := m.Subscribe("s1")
	unsub()

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after unsubscribe")
	}

	// Emitter is still valid (bus exists until RemoveIfIdle).
	emitter := m.Emitter("s1")
	emitter.Emit(agent.Event{Kind: agent.EventThinking}) // no-op, no subscribers
}

func TestSubscriptionManager_DifferentSessions(t *testing.T) {
	m := NewSubscriptionManager()
	defer m.Shutdown()

	ch1, unsub1 := m.Subscribe("s1")
	defer unsub1()
	ch2, unsub2 := m.Subscribe("s2")
	defer unsub2()

	// Emit only to s1.
	m.Emitter("s1").Emit(agent.Event{Kind: agent.EventTurnStarted, Text: "only s1"})

	select {
	case e := <-ch1:
		if e.Text != "only s1" {
			t.Errorf("s1 got %q", e.Text)
		}
	default:
		t.Error("s1 missed event")
	}
	select {
	case <-ch2:
		t.Error("s2 should not receive s1's event")
	default:
		// ok
	}
}

func TestSubscriptionManager_Shutdown(t *testing.T) {
	m := NewSubscriptionManager()

	ch, _ := m.Subscribe("s1")
	m.Shutdown()

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after Shutdown")
	}
}

func TestSessionBus_NonBlockingEmit(t *testing.T) {
	b := newSessionBus()

	// Fill the buffer (256 events) by subscribing and not reading.
	_, unsub := b.Subscribe()
	defer unsub()
	for i := 0; i < 300; i++ {
		b.Emit(agent.Event{Kind: agent.EventThinking, Text: "msg"})
	}
	// Should not block or panic — events beyond buffer cap are dropped.
}

func TestSessionBus_LateSubscriber(t *testing.T) {
	b := newSessionBus()
	b.Close()

	ch, unsub := b.Subscribe()
	defer unsub()

	// Late subscriber gets an already-closed channel.
	_, ok := <-ch
	if ok {
		t.Error("late subscriber should get closed channel")
	}
}
