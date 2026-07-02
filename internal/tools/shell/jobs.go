package shell

import (
	"code-agent/internal/jobs"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The job_* tools inspect and control background commands started by run_command
// with "background": true. They share a single *jobs.Registry with run_command,
// so a job_id returned there is resolvable here. All three are read/observe
// tools — they manage the agent's own jobs and never touch the workspace — so
// they run without a confirmation prompt.

type jobInput struct {
	JobID string `json:"job_id"`
}

func jobIDInputSchema(desc string) json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"job_id": {Type: "string", Description: desc},
	}, "job_id").JSON()
}

func parseJobID(input json.RawMessage) (string, error) {
	var in jobInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
	}
	return strings.TrimSpace(in.JobID), nil
}

func marshal(v any) (tools.ToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: string(data)}, nil
}

// JobStatusTool reports a background job's status, or lists all jobs when no
// job_id is given.
type JobStatusTool struct{ Jobs *jobs.Registry }

func (t *JobStatusTool) Name() string { return "job_status" }
func (t *JobStatusTool) Description() string {
	return "Report the status of a background job by job_id (status, exit_code, duration_ms). Omit job_id to list all jobs."
}
func (t *JobStatusTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"job_id": {Type: "string", Description: "The job to report on. Omit to list all jobs."},
	}).JSON()
}
func (t *JobStatusTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	id, err := parseJobID(input)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if id == "" {
		return marshal(t.Jobs.List())
	}
	job, ok := t.Jobs.Get(id)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("unknown job: %s", id)
	}
	return marshal(job.Snapshot())
}

// JobLogsTool returns the captured output (stdout+stderr) of a background job.
type JobLogsTool struct{ Jobs *jobs.Registry }

func (t *JobLogsTool) Name() string { return "job_logs" }
func (t *JobLogsTool) Description() string {
	return "Return the captured output (stdout and stderr) of a background job by job_id. Safe to call repeatedly while it runs."
}
func (t *JobLogsTool) InputSchema() json.RawMessage {
	return jobIDInputSchema("The job whose logs to fetch.")
}
func (t *JobLogsTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	id, err := parseJobID(input)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if id == "" {
		return tools.ToolResult{}, fmt.Errorf("job_id is required")
	}
	job, ok := t.Jobs.Get(id)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("unknown job: %s", id)
	}
	snap := job.Snapshot()
	logs := job.Logs()
	if strings.TrimSpace(logs) == "" {
		logs = "(no output yet)"
	}
	return tools.ToolResult{Content: fmt.Sprintf("job %s [%s]\n%s", snap.ID, snap.Status, logs)}, nil
}

// JobCancelTool stops a running background job.
type JobCancelTool struct{ Jobs *jobs.Registry }

func (t *JobCancelTool) Name() string { return "job_cancel" }
func (t *JobCancelTool) Description() string {
	return "Stop a running background job by job_id."
}
func (t *JobCancelTool) InputSchema() json.RawMessage {
	return jobIDInputSchema("The job to stop.")
}
func (t *JobCancelTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	id, err := parseJobID(input)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if id == "" {
		return tools.ToolResult{}, fmt.Errorf("job_id is required")
	}
	if err := t.Jobs.Cancel(id); err != nil {
		return tools.ToolResult{}, err
	}
	job, _ := t.Jobs.Get(id)
	return marshal(job.Snapshot())
}

// JobWaitTool blocks until a background job finishes or a timeout elapses
// (P8.7 Phase B). One call replaces a whole polling loop of job_status calls:
// waiting consumes ONE step of the turn's budget regardless of how long the
// job takes, instead of one step per poll — the direct fix for a slow install
// exhausting MaxSteps on job_status calls.
type JobWaitTool struct{ Jobs *jobs.Registry }

// The cap keeps one call comfortably under the 2-minute client-tool lease and
// gives the loop a cancellation checkpoint at least every 90s; the model can
// simply call job_wait again for longer jobs.
const (
	jobWaitDefaultTimeout = 30 * time.Second
	jobWaitMaxTimeout     = 90 * time.Second
	jobWaitTailBytes      = 2000
)

type jobWaitInput struct {
	JobID          string `json:"job_id"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (t *JobWaitTool) Name() string { return "job_wait" }
func (t *JobWaitTool) Description() string {
	return "Block until a background job finishes, up to timeout_seconds (default 30, max 90). Returns the final status and output tail; on timeout returns status \"running\" — call again or keep working. Prefer this over polling job_status in a loop: one job_wait costs one step no matter how long it waits."
}
func (t *JobWaitTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"job_id":          {Type: "string", Description: "The job to wait for."},
		"timeout_seconds": {Type: "integer", Description: "Max seconds to wait (default 30, capped at 90)."},
	}, "job_id").JSON()
}

func (t *JobWaitTool) Execute(ctx context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in jobWaitInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid input: %w", err)
		}
	}
	id := strings.TrimSpace(in.JobID)
	if id == "" {
		return tools.ToolResult{}, fmt.Errorf("job_id is required")
	}
	job, ok := t.Jobs.Get(id)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("unknown job: %s", id)
	}

	timeout := jobWaitDefaultTimeout
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
		if timeout > jobWaitMaxTimeout {
			timeout = jobWaitMaxTimeout
		}
	}

	started := time.Now()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-job.Done():
		return marshal(jobWaitResult(job, started, true))
	case <-timer.C:
		return marshal(jobWaitResult(job, started, false))
	case <-ctx.Done():
		return tools.ToolResult{}, ctx.Err()
	}
}

// jobWaitResult shapes the job_wait outcome: the snapshot plus how long we
// waited, and — once terminal — the output tail so the model usually needs no
// follow-up job_logs call. On timeout a note tells the model its options.
func jobWaitResult(job *jobs.Job, started time.Time, finished bool) map[string]any {
	snap := job.Snapshot()
	out := map[string]any{
		"job_id":      snap.ID,
		"status":      snap.Status,
		"waited_ms":   time.Since(started).Milliseconds(),
		"duration_ms": snap.DurationMS,
	}
	if finished {
		out["exit_code"] = snap.ExitCode
		logs := job.Logs()
		if len(logs) > jobWaitTailBytes {
			logs = "...<truncated>\n" + logs[len(logs)-jobWaitTailBytes:]
		}
		out["output_tail"] = logs
	} else {
		out["note"] = "still running — call job_wait again, do other work meanwhile, or job_cancel if no longer needed"
	}
	return out
}

var (
	_ tools.Tool = (*JobStatusTool)(nil)
	_ tools.Tool = (*JobLogsTool)(nil)
	_ tools.Tool = (*JobCancelTool)(nil)
	_ tools.Tool = (*JobWaitTool)(nil)
)
