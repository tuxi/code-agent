package conversation

import (
	"sync"

	"code-agent/internal/agent"
)

// SubscriptionManager manages per-session event subscribers. It creates and
// destroys internal sessionBus instances (lazily, on first Subscribe), exposed
// through two operations: Subscribe (for WS bridges) and Emitter (for TurnExecutor
// to publish events). When a session has zero subscribers and zero active turns,
// its bus is cleaned up.
type SubscriptionManager struct {
	mu    sync.Mutex
	buses map[string]*sessionBus
}

// sessionBus is a per-session event fan-out — extracted from the old hub. It is
// deliberately NOT exported: SubscriptionManager owns its lifecycle. It
// implements agent.Emitter (push — for the per-turn Runtime to publish) and
// provides Subscribe (pull — for WS bridges).
type sessionBus struct {
	mu     sync.Mutex
	subs   map[int]chan agent.Event
	nextID int
	closed bool
}

func newSessionBus() *sessionBus {
	return &sessionBus{subs: make(map[int]chan agent.Event)}
}

// Emit fans the event out to all subscribers. Non-blocking: if a subscriber's
// buffer is full, the event is dropped for that subscriber only. This ensures
// a slow consumer can never stall the agent loop.
func (b *sessionBus) Emit(e agent.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber buffer full: drop
		}
	}
}

// Subscribe returns a buffered event channel and an unsubscribe func. If the
// bus is already closed, the returned channel is closed (late subscriber sees
// immediate EOF).
func (b *sessionBus) Subscribe() (<-chan agent.Event, func()) {
	ch := make(chan agent.Event, 256)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
	return ch, unsub
}

// Close closes all subscriber channels. Idempotent.
func (b *sessionBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}

// subscriberCount returns the number of active subscribers (caller must hold mu).
func (b *sessionBus) subscriberCount() int {
	return len(b.subs)
}

// ---- SubscriptionManager ----

// NewSubscriptionManager creates an empty manager.
func NewSubscriptionManager() *SubscriptionManager {
	return &SubscriptionManager{buses: make(map[string]*sessionBus)}
}

// Subscribe returns a live event channel for a session, creating a bus if this
// is the first subscriber. The returned func unsubscribes and cleans up the bus
// if it becomes idle (zero subscribers).
func (m *SubscriptionManager) Subscribe(sessionID string) (<-chan agent.Event, func()) {
	bus := m.getOrCreate(sessionID)
	ch, unsub := bus.Subscribe()
	return ch, func() {
		unsub()
		m.removeIfIdle(sessionID)
	}
}

// Emitter returns an agent.Emitter for a session's bus. TurnExecutor uses this
// to fan events to all live subscribers during a turn. It always returns a
// valid emitter (the bus is created if absent), so the agent loop can emit
// unconditionally.
func (m *SubscriptionManager) Emitter(sessionID string) agent.Emitter {
	return m.getOrCreate(sessionID)
}

// Shutdown closes all buses.
func (m *SubscriptionManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.buses {
		b.Close()
	}
	m.buses = nil
}

// ---- internal ----

func (m *SubscriptionManager) getOrCreate(sessionID string) *sessionBus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.buses[sessionID]; ok {
		return b
	}
	b := newSessionBus()
	m.buses[sessionID] = b
	return b
}

func (m *SubscriptionManager) removeIfIdle(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buses[sessionID]
	if !ok {
		return
	}
	if b.subscriberCount() == 0 {
		b.Close()
		delete(m.buses, sessionID)
	}
}

// Compile-time checks.
var _ agent.Emitter = (*sessionBus)(nil)
