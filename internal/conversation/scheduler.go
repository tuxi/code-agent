package conversation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// WorkspaceExecutionMode describes the isolation a turn has while it is
// executing. SharedWorkspace is deliberately the default: until a worktree has
// been provisioned, two turns must not mutate the same checkout concurrently.
type WorkspaceExecutionMode string

const (
	SharedWorkspace   WorkspaceExecutionMode = "shared_workspace"
	IsolatedWorktree  WorkspaceExecutionMode = "isolated_worktree"
	ReadOnlyWorkspace WorkspaceExecutionMode = "read_only"
)

// TurnScheduleRequest is the scheduling identity of one turn. WorkspacePath is
// the resolved workspace for the session, not a client supplied display value.
type TurnScheduleRequest struct {
	SessionID     string
	TurnID        string
	WorkspacePath string
	Mode          WorkspaceExecutionMode
}

// SchedulerSnapshot is intentionally small and process-local. It is the basis
// for the activity endpoint; durable history remains the event store.
type SchedulerSnapshot struct {
	Running int
	Queued  int
}

// ScheduledTurnActivity is an in-memory activity projection. QueuePosition is
// one-based and meaningful only when State is "queued".
type ScheduledTurnActivity struct {
	SessionID     string
	TurnID        string
	State         string
	QueuePosition int
}

// TurnScheduler admits turns fairly across sessions. It enforces three rules:
// one running turn per session, a process-wide concurrency limit, and an
// exclusive lease for a shared workspace. It owns no goroutines: callers wait
// in Acquire, making cancellation and shutdown deterministic.
type TurnScheduler struct {
	mu sync.Mutex

	maxRunning int
	running    int
	lastStart  string
	shutdown   bool

	pending         []*scheduledTurn
	runningSessions map[string]bool
	workspaceLeases map[string]bool
	active          map[string]*scheduledTurn
	turnSequence    atomic.Uint64
}

type scheduledTurn struct {
	req      TurnScheduleRequest
	ready    chan struct{}
	err      error
	running  bool
	released bool
}

// NewTurnScheduler creates a conservative scheduler. Non-positive limits are
// normalized to one, which is the safe compatibility mode for old clients and
// unverified runtime deployments.
func NewTurnScheduler(maxConcurrentTurns int) *TurnScheduler {
	if maxConcurrentTurns < 1 {
		maxConcurrentTurns = 1
	}
	return &TurnScheduler{
		maxRunning:      maxConcurrentTurns,
		runningSessions: make(map[string]bool),
		workspaceLeases: make(map[string]bool),
		active:          make(map[string]*scheduledTurn),
	}
}

// Acquire blocks until the requested turn owns all required execution permits.
// The returned release must be called exactly once. If ctx is cancelled while
// queued, the request is removed and can never start afterwards.
func (s *TurnScheduler) Acquire(ctx context.Context, req TurnScheduleRequest, queuedCallbacks ...func(position int)) (func(), error) {
	if req.SessionID == "" {
		return nil, errors.New("conversation: scheduler requires a session id")
	}
	w := &scheduledTurn{req: req, ready: make(chan struct{})}

	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil, errors.New("conversation: scheduler is shut down")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.pending = append(s.pending, w)
	s.dispatchLocked()
	queuedPosition := 0
	if !w.running {
		for i, pending := range s.pending {
			if pending == w {
				queuedPosition = i + 1
				break
			}
		}
	}
	s.mu.Unlock()
	if queuedPosition > 0 && len(queuedCallbacks) > 0 && queuedCallbacks[0] != nil {
		queuedCallbacks[0](queuedPosition)
	}

	select {
	case <-w.ready:
		if w.err != nil {
			return nil, w.err
		}
		return s.releaseFunc(w), nil
	case <-ctx.Done():
		s.mu.Lock()
		if w.running {
			// Dispatch won the race with cancellation. Relinquish the permits
			// before returning so this cancelled turn cannot execute.
			s.releaseLocked(w)
		} else {
			s.removePendingLocked(w)
			s.dispatchLocked()
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// ReserveTurnID allocates an opaque durable-event identity before a turn is
// admitted. It deliberately does not use session.Metadata: two queued requests
// can load independent session snapshots, while this process-wide monotonic
// suffix remains collision-free without a pre-run session write.
func (s *TurnScheduler) ReserveTurnID() string {
	return fmt.Sprintf("turn_%x_%x", time.Now().UnixNano(), s.turnSequence.Add(1))
}

// Cancel removes a queued turn for this session. A running turn is intentionally
// not touched here; ActiveTurnRegistry owns its context cancellation.
func (s *TurnScheduler) Cancel(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.pending {
		if w.req.SessionID == sessionID {
			s.removePendingLocked(w)
			w.err = context.Canceled
			close(w.ready)
			s.dispatchLocked()
			return true
		}
	}
	return false
}

func (s *TurnScheduler) Snapshot() SchedulerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SchedulerSnapshot{Running: s.running, Queued: len(s.pending)}
}

// Activity returns the scheduler's current session-scoped execution state.
func (s *TurnScheduler) Activity() []ScheduledTurnActivity {
	s.mu.Lock()
	defer s.mu.Unlock()
	activities := make([]ScheduledTurnActivity, 0, len(s.active)+len(s.pending))
	for sessionID := range s.active {
		activities = append(activities, ScheduledTurnActivity{SessionID: sessionID, TurnID: s.active[sessionID].req.TurnID, State: "running"})
	}
	sort.Slice(activities, func(i, j int) bool { return activities[i].SessionID < activities[j].SessionID })
	for position, w := range s.pending {
		activities = append(activities, ScheduledTurnActivity{
			SessionID:     w.req.SessionID,
			TurnID:        w.req.TurnID,
			State:         "queued",
			QueuePosition: position + 1,
		})
	}
	return activities
}

// Shutdown wakes every queued caller. Running turns are stopped by the active
// turn registry, which owns their contexts.
func (s *TurnScheduler) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return
	}
	s.shutdown = true
	for _, w := range s.pending {
		w.err = errors.New("conversation: scheduler is shut down")
		close(w.ready)
	}
	s.pending = nil
}

func (s *TurnScheduler) releaseFunc(w *scheduledTurn) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.releaseLocked(w)
			s.mu.Unlock()
		})
	}
}

func (s *TurnScheduler) releaseLocked(w *scheduledTurn) {
	if !w.running || w.released {
		return
	}
	w.released = true
	s.running--
	delete(s.runningSessions, w.req.SessionID)
	delete(s.active, w.req.SessionID)
	if key := workspaceLeaseKey(w.req); key != "" {
		delete(s.workspaceLeases, key)
	}
	s.dispatchLocked()
}

func (s *TurnScheduler) dispatchLocked() {
	for s.running < s.maxRunning {
		i := s.nextRunnableLocked()
		if i < 0 {
			return
		}
		w := s.pending[i]
		s.pending = append(s.pending[:i], s.pending[i+1:]...)
		w.running = true
		s.running++
		s.runningSessions[w.req.SessionID] = true
		s.active[w.req.SessionID] = w
		if key := workspaceLeaseKey(w.req); key != "" {
			s.workspaceLeases[key] = true
		}
		s.lastStart = w.req.SessionID
		close(w.ready)
	}
}

// nextRunnableLocked uses a round-robin tie-breaker: when several sessions are
// eligible, prefer a session different from the one most recently started. The
// queue order inside each session is preserved because a later same-session
// entry cannot be eligible while its predecessor is waiting/running.
func (s *TurnScheduler) nextRunnableLocked() int {
	first := -1
	for i, w := range s.pending {
		if !s.runnableLocked(w) {
			continue
		}
		if first < 0 {
			first = i
		}
		if w.req.SessionID != s.lastStart {
			return i
		}
	}
	return first
}

func (s *TurnScheduler) runnableLocked(w *scheduledTurn) bool {
	if s.runningSessions[w.req.SessionID] || s.hasEarlierSessionEntryLocked(w) {
		return false
	}
	key := workspaceLeaseKey(w.req)
	return key == "" || !s.workspaceLeases[key]
}

func (s *TurnScheduler) hasEarlierSessionEntryLocked(target *scheduledTurn) bool {
	for _, w := range s.pending {
		if w == target {
			return false
		}
		if w.req.SessionID == target.req.SessionID {
			return true
		}
	}
	return false
}

func (s *TurnScheduler) removePendingLocked(target *scheduledTurn) {
	for i, w := range s.pending {
		if w == target {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

func workspaceLeaseKey(req TurnScheduleRequest) string {
	// A read-only turn still shares a mutable checkout with another turn, so it
	// keeps the conservative lease. Only a separately provisioned worktree may
	// execute without contending on the source workspace.
	if req.Mode == IsolatedWorktree {
		return ""
	}
	path := req.WorkspacePath
	if path == "" {
		return "default"
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}
