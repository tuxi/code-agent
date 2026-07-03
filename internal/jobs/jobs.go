// Package jobs runs commands in the background so a long build or test suite
// does not block the agent loop. A job is started, keeps running in its own
// goroutine while the agent does other work, and is later inspected by id
// (status / logs) or stopped (cancel).
//
// Jobs are deliberately detached from any single turn's context: a background
// command is meant to outlive the tool call that launched it. They are bounded
// only by an output cap and explicit cancellation (or process exit).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Status is a job's lifecycle state.
type Status string

const (
	Running  Status = "running"
	Exited   Status = "exited"   // finished with exit code 0
	Failed   Status = "failed"   // finished with a nonzero exit / start error
	Canceled Status = "canceled" // stopped via Cancel
)

// Owner identifies the conversation turn that started a job, so its lifecycle
// events can be forwarded into that conversation's stream (P8.7 §8.4-2: the
// client's entry card discovers a job from the parent stream's bracket events).
// Zero value = unowned (tests, headless): no forwarding.
type Owner struct {
	SessionID string
	TurnID    string
}

// Snapshot is an immutable, concurrency-safe view of a job's state. Owner is
// runtime routing metadata, not model-facing state — excluded from the JSON the
// job_* tools return.
type Snapshot struct {
	ID         string `json:"job_id"`
	Command    string `json:"command"`
	Status     Status `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Owner      Owner  `json:"-"`
}

// Sink observes job lifecycle transitions and output as they happen (P8.7
// Phase A). It is the seam that lets the runtime layer turn a job's life into
// agent events WITHOUT this package importing the agent — the same direction of
// dependency RequestObserver keeps for telemetry.
//
// Contract: callbacks fire from the job's own goroutines, so implementations
// must be concurrency-safe; Output's chunk is only valid for the duration of
// the call (copy it if retained); a Started is always eventually paired with
// exactly one Finished (including start failures and cancellation).
type Sink interface {
	JobStarted(snap Snapshot)
	JobOutput(id string, chunk []byte)
	JobFinished(snap Snapshot)
}

// Job is one background command. Its mutable fields are guarded by mu; callers
// read them through Snapshot, never directly.
type Job struct {
	id      string
	command string
	owner   Owner

	mu       sync.Mutex
	status   Status
	exitCode int
	started  time.Time
	ended    time.Time

	output *cappedBuffer
	cancel context.CancelFunc
	done   chan struct{}
}

// Snapshot returns the job's current state. Safe to call from any goroutine.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	dur := time.Since(j.started)
	if !j.ended.IsZero() {
		dur = j.ended.Sub(j.started)
	}
	return Snapshot{
		ID:         j.id,
		Command:    j.command,
		Status:     j.status,
		ExitCode:   j.exitCode,
		DurationMS: dur.Milliseconds(),
		Owner:      j.owner,
	}
}

// Logs returns the job's captured output so far (stdout and stderr interleaved).
func (j *Job) Logs() string { return j.output.String() }

// Wait blocks until the job finishes. Intended for tests and shutdown.
func (j *Job) Wait() { <-j.done }

// Done returns a channel closed when the job finishes — the select-friendly
// form of Wait, so a caller (job_wait) can bound the wait with a timeout.
func (j *Job) Done() <-chan struct{} { return j.done }

// Registry holds running and finished jobs for the process. One shared instance
// is wired into run_command (to start jobs) and the job_* tools (to inspect them).
type Registry struct {
	mu        sync.Mutex
	jobs      map[string]*Job
	order     []string
	seq       atomic.Uint64
	maxOutput int

	// Sink, when non-nil, observes every job's lifecycle and output. Set it
	// once at wiring time, before the first Start — it is read from job
	// goroutines without a lock.
	Sink Sink
}

// NewRegistry returns an empty registry with a default per-job output cap.
func NewRegistry() *Registry {
	return &Registry{jobs: make(map[string]*Job), maxOutput: 256 * 1024}
}

// Start launches command (already split into argv) in dir and returns
// immediately with a Job in the Running state. The job's context is detached
// from any caller context so it survives the tool call that started it; it ends
// on its own, on Cancel, or on process exit. owner routes the job's lifecycle
// events back to the starting conversation (zero value = no routing).
func (r *Registry) Start(dir, command string, argv []string, owner Owner) *Job {
	id := fmt.Sprintf("job_%d", r.seq.Add(1))
	ctx, cancel := context.WithCancel(context.Background())
	out := &cappedBuffer{max: r.maxOutput}

	job := &Job{
		id:      id,
		command: command,
		owner:   owner,
		status:  Running,
		started: time.Now(),
		output:  out,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	r.mu.Lock()
	r.jobs[id] = job
	r.order = append(r.order, id)
	r.mu.Unlock()

	if r.Sink != nil {
		r.Sink.JobStarted(job.Snapshot())
	}

	// The job writes to its capped buffer (backing job_logs) and, when a Sink is
	// wired, tees each chunk to it live — the buffer stays the durable tail, the
	// sink gets the stream.
	var w io.Writer = out
	if r.Sink != nil {
		w = &sinkWriter{buf: out, sink: r.Sink, id: id}
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "failed to start: %v\n", err)
		job.finish(Failed, -1)
		if r.Sink != nil {
			r.Sink.JobFinished(job.Snapshot())
		}
		return job
	}

	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		if job.status != Canceled {
			var exitErr *exec.ExitError
			switch {
			case err == nil:
				job.status, job.exitCode = Exited, 0
			case errors.As(err, &exitErr):
				job.status, job.exitCode = Failed, exitErr.ExitCode()
			default:
				job.status, job.exitCode = Failed, -1
			}
		}
		job.ended = time.Now()
		job.mu.Unlock()
		// Notify the sink BEFORE closing done: anyone unblocked by Wait/Done must
		// find the finished event already emitted (Sink pairing contract).
		if r.Sink != nil {
			r.Sink.JobFinished(job.Snapshot())
		}
		close(job.done)
	}()

	return job
}

// sinkWriter tees every write to the capped buffer AND the sink. It preserves
// the buffer's always-succeed contract: buffer first, then notify.
type sinkWriter struct {
	buf  io.Writer
	sink Sink
	id   string
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.sink.JobOutput(w.id, p)
	return n, err
}

// finish marks a job terminal (used for jobs that never started).
func (j *Job) finish(status Status, code int) {
	j.mu.Lock()
	j.status, j.exitCode, j.ended = status, code, time.Now()
	j.mu.Unlock()
	close(j.done)
}

// Get returns the job with id, if any.
func (r *Registry) Get(id string) (*Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

// Cancel stops a running job. It is a no-op error if the job is unknown or
// already finished.
func (r *Registry) Cancel(id string) error {
	job, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("unknown job: %s", id)
	}
	job.mu.Lock()
	if job.status != Running {
		st := job.status
		job.mu.Unlock()
		return fmt.Errorf("job %s is not running (status %s)", id, st)
	}
	job.status = Canceled
	job.mu.Unlock()
	job.cancel()
	return nil
}

// List returns a snapshot of every job, in start order.
func (r *Registry) List() []Snapshot {
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	jobsByID := make(map[string]*Job, len(r.jobs))
	for k, v := range r.jobs {
		jobsByID[k] = v
	}
	r.mu.Unlock()

	out := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, jobsByID[id].Snapshot())
	}
	return out
}

// cappedBuffer is a thread-safe io.Writer that retains only the most recent max
// bytes — the tail is what matters for a long-running command's logs.
type cappedBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int // total bytes discarded from the front
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if c.max > 0 && len(c.buf) > c.max {
		c.dropped += len(c.buf) - c.max
		c.buf = append([]byte(nil), c.buf[len(c.buf)-c.max:]...)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return string(c.buf)
	}
	// The byte-level trim in Write can land inside a multi-byte rune; skip
	// forward to the next rune start so the returned string is valid UTF-8.
	start := 0
	for start < len(c.buf) && start < utf8.UTFMax && !utf8.RuneStart(c.buf[start]) {
		start++
	}
	return fmt.Sprintf("...<truncated: %d bytes omitted>\n", c.dropped+start) + string(c.buf[start:])
}
