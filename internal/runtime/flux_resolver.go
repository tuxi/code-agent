// Package runtime — AwaitBinding external resolver for cross-session turn waits.
//
// Implements worker.ExternalResolver so the flux-workflow AwaitPollWorker can
// reconcile AwaitTypeExternal bindings against code-agent session event stores.
// Instead of blocking a task worker inside wait_sessions, the Map child workflow
// creates an AwaitBinding, the task worker is freed, and the poll worker
// periodically calls this resolver to check the target turn's terminal state.

package runtime

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/worker"

	"code-agent/internal/controlplane"
)

// SetFluxExternalResolver installs the cross-session resolver on the flux
// workflow runtime so AwaitTypeExternal bindings are reconciled against the
// code-agent event store. Call once after the control plane manager is created.
// Passing nil disables external binding resolution (bindings will stay pending
// until timeout).
func SetFluxExternalResolver(mgr *controlplane.Manager) {
	if mgr == nil {
		fluxResolverMu.Lock()
		fluxResolver = nil
		fluxResolverMu.Unlock()
		return
	}
	fluxResolverMu.Lock()
	fluxResolver = &externalResolver{mgr: mgr}
	fluxResolverMu.Unlock()
}

// fluxResolver returns the installed ExternalResolver for AwaitPollWorker.
func getFluxExternalResolver() worker.ExternalResolver {
	fluxResolverMu.RLock()
	defer fluxResolverMu.RUnlock()
	return fluxResolver
}

var (
	fluxResolverMu sync.RWMutex
	fluxResolver   worker.ExternalResolver
)

// externalResolver implements worker.ExternalResolver using the control plane
// manager's target event store.
type externalResolver struct {
	mgr *controlplane.Manager
}

// ResolveAwait checks whether the turn identified by binding.ProviderTaskID
// has reached a terminal state. Returns (payload, nil) on completion,
// (nil, err) on failure, and (nil, nil) if the turn is still running.
//
// binding.Correlation is expected to carry:
//
//	session_id  — target session UUID
//	turn_id     — the dispatched turn identity
//	cursor      — the admission event cursor (int string)
func (r *externalResolver) ResolveAwait(ctx context.Context, binding *domain.AwaitBinding) (map[string]any, error) {
	if binding == nil {
		return nil, nil
	}
	sessionID := stringFromCorrelation(binding.Correlation, "session_id")
	turnID := stringFromCorrelation(binding.Correlation, "turn_id")
	cursor := intFromCorrelation(binding.Correlation, "cursor")
	if sessionID == "" || turnID == "" {
		return nil, fmt.Errorf("flux external resolver: missing session_id or turn_id in correlation")
	}

	// Resolve the workspace path via the index so we can open the session's
	// per-workspace event store.
	if IndexDB() == nil {
		return nil, nil // index unavailable — retry later
	}
	entry, err := GetSessionIndex(IndexDB(), sessionID)
	if err != nil || entry == nil {
		return nil, nil // session not found — retry later
	}

	store, err := OpenStore(entry.WorkspacePath)
	if err != nil {
		return nil, nil // store unavailable — retry later
	}
	defer store.Close()

	// Replay events since the admission cursor.
	records, err := store.SessionEventsSince(ctx, sessionID, cursor)
	if err != nil {
		return nil, nil
	}
	for _, record := range records {
		if record.TurnID != turnID {
			continue
		}
		switch record.Kind {
		case "turn_finished":
			return map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"status":     "completed",
				"cursor":     float64(record.Seq),
			}, nil
		case "turn_failed":
			return nil, fmt.Errorf("turn %s failed", turnID)
		case "turn_cancelled":
			return nil, fmt.Errorf("turn %s cancelled", turnID)
		}
	}
	return nil, nil // still running
}

func stringFromCorrelation(correlation map[string]any, key string) string {
	if correlation == nil {
		return ""
	}
	v, _ := correlation[key].(string)
	return v
}

func intFromCorrelation(correlation map[string]any, key string) int64 {
	s := stringFromCorrelation(correlation, key)
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
