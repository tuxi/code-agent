package conversation

import (
	"context"
	"sync"
)

// Factory builds Conversations for a Manager. The Manager owns registration and
// lifecycle; the Factory owns assembly (runner + session), which needs config the
// conversation package deliberately does not depend on. cmd/codeagent wires a
// Factory around buildRunner + session.NewBuilder / store.Load.
type Factory interface {
	// Create builds a fresh conversation (a new session).
	Create(ctx context.Context) (*Conversation, error)
	// Resume builds a conversation around an existing stored session id. The
	// returned conversation's session id must equal sessionID.
	Resume(ctx context.Context, sessionID string) (*Conversation, error)
}

// Manager is the registry of live conversations — the seam a server's per-request
// resolver fills. It is transport- and config-agnostic: a Factory supplies the
// assembly, the Manager owns the map and lifecycle (lookup, create, resume,
// cancel, shutdown). Safe for concurrent use.
type Manager struct {
	factory Factory

	mu    sync.Mutex
	convs map[string]*Conversation
}

// NewManager returns a registry that builds conversations through f.
func NewManager(f Factory) *Manager {
	return &Manager{factory: f, convs: make(map[string]*Conversation)}
}

// Get returns the live conversation with the given id, if any.
func (m *Manager) Get(id string) (*Conversation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	return c, ok
}

// Create builds and registers a fresh conversation. The Factory runs outside the
// lock so a slow assembly never blocks other lookups.
func (m *Manager) Create(ctx context.Context) (*Conversation, error) {
	c, err := m.factory.Create(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.convs[c.ID()] = c
	m.mu.Unlock()
	return c, nil
}

// Resume returns the live conversation for sessionID, loading it through the
// Factory if it is not already in memory. Concurrent resumes of the same id
// collapse to a single registered conversation (the losers' freshly built ones
// are discarded).
func (m *Manager) Resume(ctx context.Context, sessionID string) (*Conversation, error) {
	m.mu.Lock()
	if c, ok := m.convs[sessionID]; ok {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	c, err := m.factory.Resume(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.convs[sessionID]; ok {
		return existing, nil // lost the race; keep the first, discard ours
	}
	m.convs[sessionID] = c
	return c, nil
}

// Remove tears the conversation down (cancels any in-flight turn, ends live
// subscriptions) and drops it from the registry. A no-op for an unknown id.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	c, ok := m.convs[id]
	delete(m.convs, id)
	m.mu.Unlock()
	if ok {
		c.Close()
	}
}

// List returns a snapshot of the live conversations.
func (m *Manager) List() []*Conversation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Conversation, 0, len(m.convs))
	for _, c := range m.convs {
		out = append(out, c)
	}
	return out
}

// Shutdown closes every conversation and empties the registry.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	convs := m.convs
	m.convs = make(map[string]*Conversation)
	m.mu.Unlock()
	for _, c := range convs {
		c.Close()
	}
}
