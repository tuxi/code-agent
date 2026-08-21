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

	// ToolStreams bounds per-tool-call stdout/stderr persistence (P1), mirroring
	// the daemon's sequencingEmitter cap: the first 64KB per call is stored
	// verbatim, overflow is dropped, and a head+tail marker is persisted at
	// tool_finished / turn end. Nil disables the cap (tests, callers that opt out).
	ToolStreams *agent.ToolStreamCapper
}

func (e EventStoreEmitter) Emit(ev agent.Event) {
	// Text and reasoning deltas are ephemeral live previews, not part of the
	// durable stream. EventTurnFinished and EventThinking carry their respective
	// authoritative snapshots, so persisting every delta would only bloat logs.
	if ev.Kind != agent.EventTokenDelta && ev.Kind != agent.EventReasoningDelta {
		// Tool stream persistence cap: overflow chunks are still forwarded to the
		// renderer but not persisted; the tail lands as a marker at tool_finished.
		if e.ToolStreams != nil && (ev.Kind == agent.EventToolStdout || ev.Kind == agent.EventToolStderr) {
			if ev.CallID != "" && !e.ToolStreams.NoteChunk(ev.CallID, ev.Kind, ev.Chunk) {
				if e.Next != nil {
					e.Next.Emit(ev)
				}
				return
			}
		}
		if e.ToolStreams != nil && ev.Kind == agent.EventToolFinished {
			for _, m := range e.ToolStreams.FlushCall(ev.CallID) {
				m.SessionID = ev.SessionID
				m.TurnID = ev.TurnID
				m.CallID = ev.CallID
				e.record(m)
			}
		}
		if e.ToolStreams != nil && (ev.Kind == agent.EventTurnFinished || ev.Kind == agent.EventTurnFailed || ev.Kind == agent.EventTurnCancelled) {
			for _, m := range e.ToolStreams.FlushAll() {
				m.SessionID = ev.SessionID
				m.TurnID = ev.TurnID
				e.record(m)
			}
		}
		e.record(ev)
	}
	if e.Next != nil {
		e.Next.Emit(ev)
	}
}

// record persists one event (markers included) without forwarding it. Markers
// are not re-forwarded: the renderer already saw the full stream live.
func (e EventStoreEmitter) record(ev agent.Event) {
	if payload, err := json.Marshal(ev); err == nil {
		_, _ = e.Store.RecordEvent(e.Ctx, session.EventRecord{
			SessionID: ev.SessionID,
			TurnID:    ev.TurnID,
			Kind:      string(ev.Kind),
			At:        ev.At,
			Payload:   payload,
		})
	}
}

// WithEventStore wraps a renderer so every event is persisted before it renders.
// Shared by run/repl/tui so all three log the event stream identically. The
// wrapper carries a per-call tool-stream cap so CLI/TUI sessions never persist
// unbounded tool output into the event store (the same P1 bound the daemon uses).
func WithEventStore(next agent.Emitter, store session.EventStore, ctx context.Context) agent.Emitter {
	return EventStoreEmitter{Ctx: ctx, Store: store, Next: next, ToolStreams: agent.NewToolStreamCapper()}
}
