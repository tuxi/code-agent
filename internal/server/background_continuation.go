package server

import (
	"context"
	"encoding/json"

	"code-agent/internal/agent"
)

const BackgroundContinuationCapability = "background_job_notifications_v1"

type backgroundContinuationSession interface {
	BackgroundContinuationEnabled(context.Context) bool
	SetBackgroundContinuationSupport(bool)
	BackgroundContinuationState(context.Context) (string, *agent.BackgroundContinuationInfo, error)
}

// A connection can grant or revoke only its own revision's session capability.
// An old socket closing after a reconnect cannot revoke the replacement.
func (h *WSHandler) configureBackground(ctx context.Context, sid string, revision uint64, target backgroundContinuationSession, sink FrameSink, capabilities []string, advertised bool) {
	h.mu.Lock()
	if h.controlRevisions[sid] != revision {
		h.mu.Unlock()
		return
	}
	enabled := advertised && hasCapability(capabilities, BackgroundContinuationCapability) && target.BackgroundContinuationEnabled(ctx)
	tid, state, stateErr := target.BackgroundContinuationState(ctx)
	if stateErr != nil {
		enabled = false
	}
	target.SetBackgroundContinuationSupport(enabled)
	h.mu.Unlock()
	frame, _ := json.Marshal(struct {
		Type       string                            `json:"type"`
		Enabled    bool                              `json:"enabled"`
		TurnID     string                            `json:"turn_id,omitempty"`
		Background *agent.BackgroundContinuationInfo `json:"background,omitempty"`
	}{"background_continuation_ready", enabled, tid, state})
	_ = sink.Send(frame)
}
