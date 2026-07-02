package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/jobs"
	"fmt"
	"sync"
	"time"
)

// jobEventSink bridges jobs.Sink into the agent event stream: each callback
// becomes an agent.Event stamped with SessionID = job id, so the wrapped
// emitter (typically EventStoreEmitter) persists the job's life under its own
// partition — the exact trick the subagent uses for its sub-session transcript
// (P8.7 Phase A).
//
// Output is coalesced before emitting: a spinner-heavy install redraws the
// same line dozens of times a second, and one job_output event per Write would
// bloat the event log with hundreds of rows. Chunks accumulate per job and
// flush when the buffer passes flushBytes or flushDelay elapses, and always on
// JobFinished — so the persisted stream stays faithful but bounded.
type jobEventSink struct {
	emitter agent.Emitter

	mu      sync.Mutex
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

// NewJobEventSink wraps emitter as a jobs.Sink. The emitter must be safe for
// concurrent use — callbacks arrive from job goroutines (EventStoreEmitter over
// the sqlite store is; a bare TUI renderer would not be).
func NewJobEventSink(emitter agent.Emitter) jobs.Sink {
	return &jobEventSink{emitter: emitter, pending: make(map[string]*pendingOutput)}
}

func (s *jobEventSink) JobStarted(id, command string) {
	s.emitter.Emit(agent.Event{
		Kind: agent.EventJobStarted, At: time.Now(), SessionID: id, Text: command,
	})
}

func (s *jobEventSink) JobOutput(id string, chunk []byte) {
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

func (s *jobEventSink) JobFinished(id string, snap jobs.Snapshot) {
	s.flush(id)
	ev := agent.Event{
		Kind: agent.EventJobFinished, At: time.Now(), SessionID: id,
		Text:     string(snap.Status),
		Elapsed:  time.Duration(snap.DurationMS) * time.Millisecond,
		ExitCode: snap.ExitCode,
	}
	if snap.Status == jobs.Failed {
		ev.Err = fmt.Sprintf("exit code %d", snap.ExitCode)
	}
	s.emitter.Emit(ev)
}

// flush emits whatever output is buffered for id, if any.
func (s *jobEventSink) flush(id string) {
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
func (s *jobEventSink) takeLocked(id string, p *pendingOutput) []byte {
	if p.timer != nil {
		p.timer.Stop()
	}
	buf := p.buf
	delete(s.pending, id)
	return buf
}

func (s *jobEventSink) emitOutput(id string, buf []byte) {
	if len(buf) == 0 {
		return
	}
	s.emitter.Emit(agent.Event{
		Kind: agent.EventJobOutput, At: time.Now(), SessionID: id, Chunk: string(buf),
	})
}
