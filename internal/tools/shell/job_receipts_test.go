package shell

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/jobs"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

func TestJobWaitTerminalReceiptOwnershipAndOptIn(t *testing.T) {
	reg := jobs.NewRegistry()
	job := reg.Start(".", "echo done", []string{"echo", "done"}, jobs.Owner{SessionID: "owner", TurnID: "origin", CallID: "call", Notify: true})
	job.Wait()
	wait := &JobWaitTool{Jobs: reg}
	for _, tc := range []struct {
		session       string
		enabled, want bool
	}{
		{"owner", true, true}, {"owner", false, false}, {"other", true, false},
	} {
		result, err := wait.Execute(context.Background(), tools.ExecutionContext{SessionID: tc.session, RuntimeInputDelivery: tc.enabled}, json.RawMessage(`{"job_id":"`+job.Snapshot().ID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(result.RuntimeInputReceipts) > 0; got != tc.want {
			t.Fatalf("%+v: receipts=%v", tc, result.RuntimeInputReceipts)
		}
		if tc.want && result.RuntimeInputReceipts[0] != session.JobCompletionInputID(job.Snapshot().ID) {
			t.Fatal(result.RuntimeInputReceipts)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err = json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		if _, ok := wire["RuntimeInputReceipts"]; ok {
			t.Fatal("internal receipt leaked on tool wire")
		}
	}
}
