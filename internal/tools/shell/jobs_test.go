package shell

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCommandBackground(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"echo hi","background":true}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var br backgroundResult
	if err := json.Unmarshal([]byte(res.Content), &br); err != nil {
		t.Fatalf("background result is not JSON: %v\n%s", err, res.Content)
	}
	if br.JobID == "" {
		t.Fatal("no job_id returned for a background command")
	}
	if br.Decision != "allow" {
		t.Errorf("decision = %q, want allow", br.Decision)
	}

	job, ok := tool.Jobs.Get(br.JobID)
	if !ok {
		t.Fatal("job not registered")
	}
	job.Wait()

	// job_status reports completion.
	status := &JobStatusTool{Jobs: tool.Jobs}
	sres, err := status.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sres.Content, "exited") {
		t.Errorf("job_status = %s, want it to show exited", sres.Content)
	}

	// job_logs returns the output.
	logs := &JobLogsTool{Jobs: tool.Jobs}
	lres, err := logs.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lres.Content, "hi") {
		t.Errorf("job_logs = %s, want it to contain hi", lres.Content)
	}
}

func TestBackgroundStillPolicyGated(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"rm -rf /","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, `"decision": "block"`) {
		t.Errorf("a blocked command must be refused even in background mode: %s", res.Content)
	}
	if strings.Contains(res.Content, "job_id") {
		t.Error("a blocked command must not start a background job")
	}
}

func TestJobCancelTool(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"sleep 30","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var br backgroundResult
	if err := json.Unmarshal([]byte(res.Content), &br); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Content)
	}

	cancel := &JobCancelTool{Jobs: tool.Jobs}
	cres, err := cancel.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(cres.Content, "canceled") {
		t.Errorf("job_cancel = %s, want it to show canceled", cres.Content)
	}
}

func TestJobToolsUnknownID(t *testing.T) {
	tool := NewRunCommandTool()
	status := &JobStatusTool{Jobs: tool.Jobs}
	if _, err := status.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"job_nope"}`)); err == nil {
		t.Error("job_status on an unknown id should error")
	}
	wait := &JobWaitTool{Jobs: tool.Jobs}
	if _, err := wait.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"job_nope"}`)); err == nil {
		t.Error("job_wait on an unknown id should error")
	}
}

// TestJobWaitFinished proves the P8.7 core claim: ONE job_wait call carries the
// model across the whole job, returning the terminal status + output tail —
// no job_status polling loop.
func TestJobWaitFinished(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"echo done-marker","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var br backgroundResult
	if err := json.Unmarshal([]byte(res.Content), &br); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Content)
	}

	wait := &JobWaitTool{Jobs: tool.Jobs}
	wres, err := wait.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatalf("job_wait: %v", err)
	}
	for _, want := range []string{`"status": "exited"`, `"exit_code": 0`, "done-marker"} {
		if !strings.Contains(wres.Content, want) {
			t.Errorf("job_wait result missing %s:\n%s", want, wres.Content)
		}
	}
}

// TestJobWaitTimeout: an unfinished job returns status "running" with guidance,
// not an error — the model decides whether to wait again or move on.
func TestJobWaitTimeout(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"sleep 30","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var br backgroundResult
	if err := json.Unmarshal([]byte(res.Content), &br); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Content)
	}
	defer tool.Jobs.Cancel(br.JobID)

	wait := &JobWaitTool{Jobs: tool.Jobs}
	wres, err := wait.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"`+br.JobID+`","timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("job_wait: %v", err)
	}
	if !strings.Contains(wres.Content, `"status": "running"`) {
		t.Errorf("job_wait on a running job should report running:\n%s", wres.Content)
	}
	if !strings.Contains(wres.Content, "note") {
		t.Errorf("timeout result should carry guidance for the model:\n%s", wres.Content)
	}
	if strings.Contains(wres.Content, "output_tail") {
		t.Errorf("timeout result should not include output_tail:\n%s", wres.Content)
	}
}

// TestJobWaitCancelledContext: a cancelled turn context aborts the wait
// immediately instead of blocking out the timeout.
func TestJobWaitCancelledContext(t *testing.T) {
	tool := NewRunCommandTool()
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"command":"sleep 30","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var br backgroundResult
	if err := json.Unmarshal([]byte(res.Content), &br); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Content)
	}
	defer tool.Jobs.Cancel(br.JobID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wait := &JobWaitTool{Jobs: tool.Jobs}
	if _, err := wait.Execute(ctx, tools.ExecutionContext{WorkspaceRoot: "."}, json.RawMessage(`{"job_id":"`+br.JobID+`"}`)); err == nil {
		t.Error("job_wait with a cancelled context should return the context error")
	}
}
