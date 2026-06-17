package shell

import (
	"code-agent/internal/jobs"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
func (t *JobStatusTool) Execute(_ context.Context, input json.RawMessage) (tools.ToolResult, error) {
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
func (t *JobLogsTool) Execute(_ context.Context, input json.RawMessage) (tools.ToolResult, error) {
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
func (t *JobCancelTool) Execute(_ context.Context, input json.RawMessage) (tools.ToolResult, error) {
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

var (
	_ tools.Tool = (*JobStatusTool)(nil)
	_ tools.Tool = (*JobLogsTool)(nil)
	_ tools.Tool = (*JobCancelTool)(nil)
)
