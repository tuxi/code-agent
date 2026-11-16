package agent

import (
	"code-agent/internal/session"
	"context"
	"errors"
	"github.com/google/uuid"
)

// ContextMaintenance is a full observation snapshot, never an execution policy.
// Counts from preparation remain provisional until Committed is true.
type ContextMaintenance struct {
	OperationID          string `json:"operation_id"`
	Phase                string `json:"phase"`
	Status               string `json:"status"`
	Committed            bool   `json:"committed"`
	BeforeTokens         *int   `json:"before_tokens,omitempty"`
	AfterTokens          *int   `json:"after_tokens,omitempty"`
	EstimatedSavedTokens *int   `json:"estimated_saved_tokens,omitempty"`
	ClearedResults       int    `json:"cleared_results"`
	SummaryGenerated     bool   `json:"summary_generated"`
	MeasurementSource    string `json:"measurement_source"`
	Error                string `json:"error,omitempty"`
}

type contextMaintenanceObservation struct {
	runner    *Runner
	sessionID string
	value     ContextMaintenance
}

func (r *Runner) beginContextMaintenance(s *session.Session) *contextMaintenanceObservation {
	value := ContextMaintenance{OperationID: uuid.NewString(), Phase: "preparing", Status: "running", MeasurementSource: "unknown"}
	if s.PromptTokens > 0 {
		n := s.PromptTokens
		value.BeforeTokens = &n
	}
	if last := s.LastCompaction(); last != nil && !last.Finalized() && last.MaintenanceID != "" {
		value.OperationID = last.MaintenanceID
		value.ClearedResults = last.ClearedResults
		if last.PrunedTokens > 0 {
			n := last.PrunedTokens
			value.EstimatedSavedTokens = &n
		}
		n := last.BeforeTokens
		value.BeforeTokens = &n
	}
	o := &contextMaintenanceObservation{runner: r, value: value, sessionID: s.ID}
	o.emit()
	return o
}

func (o *contextMaintenanceObservation) emit() {
	// Copy the snapshot: an emitter may retain it after this method returns.
	value := o.value
	o.runner.emit(Event{Kind: EventContextMaintenance, ContextMaintenance: &value, SessionID: o.sessionID})
}

func (o *contextMaintenanceObservation) summarizing() {
	o.value.Phase = "summarizing"
	o.emit()
}

func (o *contextMaintenanceObservation) finish(s *session.Session, err error, generated bool) {
	o.value.Phase, o.value.Status = "finished", "completed"
	o.value.Committed = err == nil
	o.value.SummaryGenerated = generated && err == nil
	if err != nil {
		o.value.Status = "failed"
		if errors.Is(err, context.Canceled) {
			o.value.Status = "canceled"
		}
		o.value.Error = retryReasonText(err)
	} else if last := s.LastCompaction(); generated && last != nil && !last.Finalized() {
		last.MaintenanceID = o.value.OperationID
		last.ClearedResults = o.value.ClearedResults
		if o.value.EstimatedSavedTokens != nil {
			last.PrunedTokens = *o.value.EstimatedSavedTokens
		}
		o.value.Phase, o.value.Status = "measuring", "pending"
	} else if o.value.EstimatedSavedTokens != nil {
		o.value.MeasurementSource = "estimated"
	}
	o.emit()
}

func (r *Runner) measuredContextMaintenance(stat *session.CompactionStats, threshold int) {
	if stat.MaintenanceID == "" {
		return
	}
	before := stat.BeforeTokens
	value := ContextMaintenance{OperationID: stat.MaintenanceID, Phase: "finished", Status: "completed", Committed: true,
		BeforeTokens: &before, ClearedResults: stat.ClearedResults, SummaryGenerated: true, MeasurementSource: "measured"}
	if stat.PrunedTokens > 0 {
		n := stat.PrunedTokens
		value.EstimatedSavedTokens = &n
	}
	if stat.AfterTokens <= 0 {
		// Provider usage omitted its prompt count. Do not turn the sentinel into
		// an apparent complete reclamation of the context.
		value.Phase, value.Status, value.MeasurementSource = "measuring", "pending", "unknown"
	} else {
		after := stat.AfterTokens
		value.AfterTokens = &after
		if threshold > 0 && after >= threshold {
			value.Status = "limited"
		}
	}
	r.emit(Event{Kind: EventContextMaintenance, ContextMaintenance: &value})
}
