package shell

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/jobs"
	"code-agent/internal/tools"
)

func TestCommandOutcomesCarryExplicitFailures(t *testing.T) {
	tool := NewRunCommandTool()
	for _, tc := range []struct {
		result commandResult
		kind   string
	}{
		{commandResult{ExitCode: 0, Status: "completed"}, ""},
		{commandResult{ExitCode: 1, Status: "failed"}, "runtime"},
		{commandResult{ExitCode: -1, Status: "blocked"}, "blocked"},
		{commandResult{ExitCode: -1, Status: "failed"}, "environment"},
		{commandResult{ExitCode: -1, Status: "timed_out"}, "timeout"},
		{commandResult{ExitCode: -1, Status: "cancelled"}, "canceled"},
	} {
		result, err := tool.result(tc.result)
		if err != nil {
			t.Fatal(err)
		}
		kind := ""
		if result.Failure != nil {
			kind = result.Failure.Kind
		}
		if kind != tc.kind {
			t.Fatalf("status=%s kind=%s want=%s", tc.result.Status, kind, tc.kind)
		}
	}
}

func TestWaitFailedJobDoesNotReportSuccess(t *testing.T) {
	reg := jobs.NewRegistry()
	job := reg.Start(t.TempDir(), "false", []string{"false"}, jobs.Owner{})
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not exit")
	}
	input, _ := json.Marshal(map[string]string{"job_id": job.Snapshot().ID})
	for _, tool := range []tools.Tool{&JobStatusTool{Jobs: reg}, &JobWaitTool{Jobs: reg}, &JobLogsTool{Jobs: reg}} {
		result, err := tool.Execute(context.Background(), tools.ExecutionContext{}, input)
		if err != nil || result.Failure == nil {
			t.Fatalf("%s failure lost: %+v %v", tool.Name(), result, err)
		}
	}
	result, err := marshalJobOutcome(map[string]string{"status": "running"}, jobs.Snapshot{Status: jobs.Running})
	if err != nil || result.Failure != nil {
		t.Fatal("waiting treated as failed execution")
	}
}
