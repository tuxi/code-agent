package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"context"
	"encoding/json"
)

// RequestObserver records each model request to the telemetry store for
// transport telemetry. Best-effort: a telemetry write never fails the run.
type RequestObserver struct {
	Ctx   context.Context
	Store session.TelemetryStore
}

func (o RequestObserver) Observe(s model.RequestStat) {
	trace := make([]session.AttemptRecord, len(s.Trace))
	for i, a := range s.Trace {
		result := a.ErrorClass
		if result == "" {
			result = "success"
		}
		trace[i] = session.AttemptRecord{LatencyMs: a.Latency.Milliseconds(), Result: result}
	}
	_ = o.Store.RecordRequest(o.Ctx, session.RequestRecord{
		At:                 s.At,
		Model:              s.Model,
		PromptTokens:       s.PromptTokens,
		CachedPromptTokens: s.CachedPromptTokens,
		CompletionTokens:   s.CompletionTokens,
		Attempts:           s.Attempts,
		Retries:            s.Retries,
		TimedOut:           s.TimedOut,
		Success:            s.Success,
		ErrorClass:         s.ErrorClass,
		LatencyMs:          s.Latency.Milliseconds(),
		Trace:              trace,
	})
}

// AttachObserver wires request telemetry into a provider once the store is open
// (BuildProvider always returns a *ResilientProvider, so the assertion holds).
func AttachObserver(provider model.Provider, store session.TelemetryStore, ctx context.Context) {
	if rp, ok := provider.(*model.ResilientProvider); ok {
		rp.Observer = RequestObserver{Ctx: ctx, Store: store}
	}
}

// EventStoreEmitter persists each agent event to the event store (the P7
// EventStore — the raw, replayable runtime stream) and forwards it to the next
// renderer unchanged. A pure decorator, the same shape as liveProgress: it adds
// persistence with zero changes to the loop or the renderer it wraps. Best-effort
// like RequestObserver — a telemetry write never fails a run.
type EventStoreEmitter struct {
	Ctx   context.Context
	Store session.EventStore
	Next  agent.Emitter
}

func (e EventStoreEmitter) Emit(ev agent.Event) {
	// Token deltas (8.6) are an ephemeral live preview, not part of the durable
	// stream — the finalized answer is captured by EventTurnFinished. Persisting
	// every delta would bloat the event log (hundreds per answer), so skip them.
	if ev.Kind != agent.EventTokenDelta {
		if payload, err := json.Marshal(ev); err == nil {
			_ = e.Store.RecordEvent(e.Ctx, session.EventRecord{
				SessionID: ev.SessionID,
				TurnID:    ev.TurnID,
				Kind:      string(ev.Kind),
				At:        ev.At,
				Payload:   payload,
			})
		}
	}
	if e.Next != nil {
		e.Next.Emit(ev)
	}
}

// WithEventStore wraps a renderer so every event is persisted before it renders.
// Shared by run/repl/tui so all three log the event stream identically.
func WithEventStore(next agent.Emitter, store session.EventStore, ctx context.Context) agent.Emitter {
	return EventStoreEmitter{Ctx: ctx, Store: store, Next: next}
}
