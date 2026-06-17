package shell

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCommandBackground(t *testing.T) {
	tool := NewRunCommandTool(".")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hi","background":true}`))
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
	sres, err := status.Execute(context.Background(), json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sres.Content, "exited") {
		t.Errorf("job_status = %s, want it to show exited", sres.Content)
	}

	// job_logs returns the output.
	logs := &JobLogsTool{Jobs: tool.Jobs}
	lres, err := logs.Execute(context.Background(), json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lres.Content, "hi") {
		t.Errorf("job_logs = %s, want it to contain hi", lres.Content)
	}
}

func TestBackgroundStillPolicyGated(t *testing.T) {
	tool := NewRunCommandTool(".")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"rm -rf /","background":true}`))
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
	tool := NewRunCommandTool(".")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"sleep 30","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var br backgroundResult
	if err := json.Unmarshal([]byte(res.Content), &br); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, res.Content)
	}

	cancel := &JobCancelTool{Jobs: tool.Jobs}
	cres, err := cancel.Execute(context.Background(), json.RawMessage(`{"job_id":"`+br.JobID+`"}`))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(cres.Content, "canceled") {
		t.Errorf("job_cancel = %s, want it to show canceled", cres.Content)
	}
}

func TestJobToolsUnknownID(t *testing.T) {
	tool := NewRunCommandTool(".")
	status := &JobStatusTool{Jobs: tool.Jobs}
	if _, err := status.Execute(context.Background(), json.RawMessage(`{"job_id":"job_nope"}`)); err == nil {
		t.Error("job_status on an unknown id should error")
	}
}
