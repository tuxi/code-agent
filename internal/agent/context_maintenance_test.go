package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"context"
	"errors"
	"reflect"
	"testing"
)

type mutatingFailedMaintenance struct{}

func (mutatingFailedMaintenance) Compact(_ context.Context, s *session.Session) error {
	s.Messages[0].Content = "discarded"
	s.Summary = "discarded"
	return errors.New("summary provider unavailable")
}
func maintenanceEvents(e *capturingEmitter) []*ContextMaintenance {
	var out []*ContextMaintenance
	for _, event := range e.events {
		if event.ContextMaintenance != nil {
			out = append(out, event.ContextMaintenance)
		}
	}
	return out
}
func TestMaintenanceFailureRollsBackAndPublishesNoSavings(t *testing.T) {
	for _, manual := range []bool{false, true} {
		s := overBudgetSession()
		original := append([]model.Message(nil), s.Messages...)
		rec := &capturingEmitter{}
		r := &Runner{Compactor: mutatingFailedMaintenance{}, Emitter: rec}
		var err error
		if manual {
			_, err = r.CompactContext(context.Background(), s)
		} else {
			err = r.maybeCompact(context.Background(), s)
		}
		if err == nil || !reflect.DeepEqual(original, s.Messages) || s.Summary != "" {
			t.Fatalf("manual=%v: failure did not restore context", manual)
		}
		events := maintenanceEvents(rec)
		last := events[len(events)-1]
		if last.Status != "failed" || last.Committed || last.AfterTokens != nil || last.SummaryGenerated {
			t.Fatalf("misleading failed snapshot: %+v", last)
		}
		if events[0].Status != "running" || events[0].OperationID != last.OperationID {
			t.Fatal("operation identity changed")
		}
	}
}
func TestMaintenanceMeasuresSameOperationAndKeepsLegacyCompactedEvent(t *testing.T) {
	for _, measured := range []int{25000, 92000, 0} {
		rec := &capturingEmitter{}
		r := &Runner{Compactor: shrinkCompactor{}, Emitter: rec, Tools: tools.NewRegistry(), MaxSteps: 1,
			Model: &scriptedProvider{responses: []model.Response{{Content: "done", FinishReason: "stop", Usage: model.Usage{PromptTokens: measured}}}}}
		s := overBudgetSession()
		if _, err := r.RunTurn(context.Background(), s, "hi"); err != nil {
			t.Fatal(err)
		}
		events := maintenanceEvents(rec)
		last := events[len(events)-1]
		expected := "completed"
		if measured >= 90000 {
			expected = "limited"
		}
		if measured == 0 {
			expected = "pending"
		}
		if last.Status != expected || !last.Committed {
			t.Fatalf("got %+v", last)
		}
		sawPending := false
		for _, v := range events {
			if v.OperationID != last.OperationID {
				t.Fatal("identity changed")
			}
			if v.Status == "pending" {
				sawPending = true
			}
		}
		if !sawPending {
			t.Fatal("missing unmeasured stage")
		}
		if measured == 0 && last.AfterTokens != nil {
			t.Fatal("unknown usage became zero")
		}
		if measured > 0 && (last.AfterTokens == nil || *last.AfterTokens != measured) {
			t.Fatal("measurement missing")
		}
		for _, e := range rec.events {
			if e.Kind == EventCompacted && e.ContextOperationID != last.OperationID {
				t.Fatal("legacy event lost correlation")
			}
		}
	}
}
func TestMaintenanceNoOpDoesNotInventSummaryOrTokenDelta(t *testing.T) {
	rec := &capturingEmitter{}
	r := &Runner{Compactor: noopCompactor{}, Emitter: rec}
	s := overBudgetSession()
	if err := r.maybeCompact(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	events := maintenanceEvents(rec)
	last := events[len(events)-1]
	if last.Status != "completed" || last.SummaryGenerated || last.AfterTokens != nil || last.EstimatedSavedTokens != nil {
		t.Fatalf("no-op fabricated results: %+v", last)
	}
}
