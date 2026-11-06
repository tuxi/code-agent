package shell

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/jobs"
	"code-agent/internal/tools"
)

func TestJobYieldIsOptInOwnedAndNonblocking(t *testing.T) {
	reg := jobs.NewRegistry()
	job := reg.Start(".", "sleep 30", []string{"sleep", "30"}, jobs.Owner{SessionID: "owner", TurnID: "source", CallID: "call", Notify: true})
	defer func() { _ = reg.Cancel(job.Snapshot().ID); job.Wait() }()
	wait := &JobWaitTool{Jobs: reg, AllowYield: true}
	input := json.RawMessage(`{"job_id":"` + job.Snapshot().ID + `","yield":true}`)
	ec := tools.ExecutionContext{SessionID: "owner", RuntimeInputDelivery: true, CanYield: true}
	result, err := wait.Execute(context.Background(), ec, input)
	if err != nil || len(result.RuntimeWaitFor) != 1 || !strings.Contains(result.Content, "waiting_background") {
		t.Fatal(result, err)
	}
	ec.SessionID = "other"
	if _, err = wait.Execute(context.Background(), ec, input); err == nil {
		t.Fatal("foreign job yielded")
	}
	ec.SessionID = "owner"
	if _, err = wait.Execute(context.Background(), ec, json.RawMessage(`{"job_id":"`+job.Snapshot().ID+`","yield":true,"timeout_seconds":0}`)); err == nil {
		t.Fatal("ambiguous wait accepted")
	}
	legacy := &JobWaitTool{Jobs: reg}
	if strings.Contains(string(legacy.InputSchema()), `"yield"`) {
		t.Fatal("legacy schema changed")
	}
	if _, err = legacy.Execute(context.Background(), ec, input); err == nil {
		t.Fatal("legacy execution yielded")
	}
	if err = reg.Cancel(job.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	job.Wait()
	result, err = wait.Execute(context.Background(), ec, input)
	if err != nil || len(result.RuntimeWaitFor) != 0 || len(result.RuntimeInputReceipts) != 1 {
		t.Fatal("terminal job parked", result, err)
	}
}
