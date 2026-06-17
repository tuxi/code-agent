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
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Status is a job's lifecycle state.
type Status string

const (
	Running  Status = "running"
	Exited   Status = "exited"   // finished with exit code 0
	Failed   Status = "failed"   // finished with a nonzero exit / start error
	Canceled Status = "canceled" // stopped via Cancel
)

// Snapshot is an immutable, concurrency-safe view of a job's state.
type Snapshot struct {
	ID         string `json:"job_id"`
	Command    string `json:"command"`
	Status     Status `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

// Job is one background command. Its mutable fields are guarded by mu; callers
// read them through Snapshot, never directly.
type Job struct {
	id      string
	command string

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
	}
}

// Logs returns the job's captured output so far (stdout and stderr interleaved).
func (j *Job) Logs() string { return j.output.String() }

// Wait blocks until the job finishes. Intended for tests and shutdown.
func (j *Job) Wait() { <-j.done }

// Registry holds running and finished jobs for the process. One shared instance
// is wired into run_command (to start jobs) and the job_* tools (to inspect them).
type Registry struct {
	mu        sync.Mutex
	jobs      map[string]*Job
	order     []string
	seq       atomic.Uint64
	maxOutput int
}

// NewRegistry returns an empty registry with a default per-job output cap.
func NewRegistry() *Registry {
	return &Registry{jobs: make(map[string]*Job), maxOutput: 256 * 1024}
}

// Start launches command (already split into argv) in dir and returns
// immediately with a Job in the Running state. The job's context is detached
// from any caller context so it survives the tool call that started it; it ends
// on its own, on Cancel, or on process exit.
func (r *Registry) Start(dir, command string, argv []string) *Job {
	id := fmt.Sprintf("job_%d", r.seq.Add(1))
	ctx, cancel := context.WithCancel(context.Background())
	out := &cappedBuffer{max: r.maxOutput}

	job := &Job{
		id:      id,
		command: command,
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

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(out, "failed to start: %v\n", err)
		job.finish(Failed, -1)
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
		close(job.done)
	}()

	return job
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
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if c.max > 0 && len(c.buf) > c.max {
		c.buf = append([]byte(nil), c.buf[len(c.buf)-c.max:]...)
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return "...<truncated>\n" + string(c.buf)
	}
	return string(c.buf)
}
