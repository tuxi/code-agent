package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/jobs"
	"code-agent/internal/session"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// JobEventSink bridges jobs.Sink into the agent event stream (P8.7). Every
// event carries SessionID = job id (the child-stream identity, §8.4-1), and is
// persisted under the JOB's own partition — so
// GET /v1/conversations/{job_id}/events replays a job's full life, the exact
// trick the subagent uses for its sub-session transcript.
//
// The job_started / job_finished BRACKET events are additionally forwarded into
// the OWNING conversation (§8.4-2, client-confirmed): persisted under the
// parent's partition (so the entry card survives replay) and, when a live
// resolver is wired, fanned out to the parent's live subscribers with the
// parent-partition seq stamped — the same persist-then-stamp order the turn
// publisher (sequencingEmitter) guarantees. job_output stays child-only.
//
// Output is coalesced before emitting: a spinner-heavy install redraws the
// same line dozens of times a second, and one job_output event per Write would
// bloat the event log with hundreds of rows. Chunks accumulate per job and
// flush when the buffer passes jobFlushBytes or jobFlushDelay elapses, and
// always on JobFinished — the persisted stream stays faithful but bounded.
type JobEventSink struct {
	ctx   context.Context
	store session.EventStore

	mu      sync.Mutex
	live    func(sessionID string) agent.Emitter // nil until SetLiveResolver
	pending map[string]*pendingOutput
}

type pendingOutput struct {
	buf   []byte
	timer *time.Timer
}

const (
	jobFlushBytes = 4 * 1024
	jobFlushDelay = 750 * time.Millisecond
)

// NewJobEventSink builds the sink over the event store. ctx should be the
// process/server context (jobs outlive turns, so never a turn context).
func NewJobEventSink(ctx context.Context, store session.EventStore) *JobEventSink {
	return &JobEventSink{ctx: ctx, store: store, pending: make(map[string]*pendingOutput)}
}

// SetLiveResolver wires live fan-out for the parent-stream bracket copies —
// serve passes the SubscriptionManager's Emitter so a connected client sees
// job_started/job_finished in real time. Late-bound because the subscription
// manager is assembled after the tool registry; nil-safe (store-only until set).
func (s *JobEventSink) SetLiveResolver(f func(sessionID string) agent.Emitter) {
	s.mu.Lock()
	s.live = f
	s.mu.Unlock()
}

func (s *JobEventSink) JobStarted(snap jobs.Snapshot) {
	s.publish(agent.Event{
		Kind: agent.EventJobStarted, At: time.Now(),
		SessionID: snap.ID, TurnID: snap.Owner.TurnID,
		Text: snap.Command,
	}, snap.Owner)
}

func (s *JobEventSink) JobOutput(id string, chunk []byte) {
	s.mu.Lock()
	p := s.pending[id]
	if p == nil {
		p = &pendingOutput{}
		s.pending[id] = p
	}
	p.buf = append(p.buf, chunk...) // copy: chunk is only valid during the call
	if len(p.buf) >= jobFlushBytes {
		buf := s.takeLocked(id, p)
		s.mu.Unlock()
		s.emitOutput(id, buf)
		return
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(jobFlushDelay, func() { s.flush(id) })
	}
	s.mu.Unlock()
}

func (s *JobEventSink) JobFinished(snap jobs.Snapshot) {
	s.flush(snap.ID)
	ev := agent.Event{
		Kind: agent.EventJobFinished, At: time.Now(),
		SessionID: snap.ID, TurnID: snap.Owner.TurnID,
		Text:     string(snap.Status),
		Elapsed:  time.Duration(snap.DurationMS) * time.Millisecond,
		ExitCode: snap.ExitCode,
	}
	if snap.Status == jobs.Failed {
		ev.Err = fmt.Sprintf("exit code %d", snap.ExitCode)
	}
	s.publish(ev, snap.Owner)
}

// publish is the job bracket-event path: child partition always; parent
// partition + parent live stream when the job has an owner.
func (s *JobEventSink) publish(ev agent.Event, owner jobs.Owner) {
	s.record(ev.SessionID, ev)
	s.ForwardBracket(ev, owner.SessionID)
}

// ForwardBracket is the parent-stream half of a child-stream bracket (§8.4-2):
// it persists ev under the owning conversation's partition and fans it out to
// that conversation's live subscribers, with the PARENT-partition seq stamped
// on the live frame so the client's per-conversation cursor (max seq) keeps
// working. The payload keeps ev's own SessionID (the child-stream id).
//
// Used internally for job brackets and by the subagent (subagent.go) for
// task_started/task_finished — whose child-partition copies are written by its
// own sinks. Nil-receiver- and empty-owner-safe, so callers forward
// unconditionally.
func (s *JobEventSink) ForwardBracket(ev agent.Event, ownerSessionID string) {
	if s == nil || ownerSessionID == "" {
		return
	}
	if seq := s.record(ownerSessionID, ev); seq > 0 {
		ev.Seq = seq
	}
	s.mu.Lock()
	live := s.live
	s.mu.Unlock()
	if live != nil {
		if em := live(ownerSessionID); em != nil {
			em.Emit(ev)
		}
	}
}

// record persists ev under the given partition and returns the assigned seq.
// The partition is the ROW's session id; the payload keeps ev's own SessionID
// (= job id) — precedent: seq lives in the row, never the payload. Best-effort,
// like every event-store write.
func (s *JobEventSink) record(partition string, ev agent.Event) int64 {
	payload, err := json.Marshal(ev)
	if err != nil {
		return 0
	}
	seq, err := s.store.RecordEvent(s.ctx, session.EventRecord{
		SessionID: partition,
		TurnID:    ev.TurnID,
		Kind:      string(ev.Kind),
		At:        ev.At,
		Payload:   payload,
	})
	if err != nil {
		return 0
	}
	return seq
}

// flush emits whatever output is buffered for id, if any.
func (s *JobEventSink) flush(id string) {
	s.mu.Lock()
	p := s.pending[id]
	if p == nil {
		s.mu.Unlock()
		return
	}
	buf := s.takeLocked(id, p)
	s.mu.Unlock()
	s.emitOutput(id, buf)
}

// takeLocked detaches the pending buffer and disarms its timer. Caller holds mu.
func (s *JobEventSink) takeLocked(id string, p *pendingOutput) []byte {
	if p.timer != nil {
		p.timer.Stop()
	}
	buf := p.buf
	delete(s.pending, id)
	return buf
}

// emitOutput persists a coalesced output span — child partition only (§8.4-2:
// the parent stream gets brackets, never the output firehose).
func (s *JobEventSink) emitOutput(id string, buf []byte) {
	if len(buf) == 0 {
		return
	}
	s.record(id, agent.Event{
		Kind: agent.EventJobOutput, At: time.Now(), SessionID: id, Chunk: string(buf),
	})
}
