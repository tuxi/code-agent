package mcpbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"code-agent/internal/executionevidence"
)

func TestAwaitCompletionStopsBeforeArtifactsOnAttentionAndFailure(t *testing.T) {
	for _, status := range []string{"waiting_for_human", "paused", "failed", "cancelled", "unknown"} {
		t.Run(status, func(t *testing.T) {
			polls := 0
			r, err := AwaitCompletion(context.Background(), func(context.Context) (ReadResult, error) {
				polls++
				return ReadResult{SessionID: "s", TurnID: "t", Status: status}, nil
			}, nil)
			if err == nil || polls != 1 || r.Status != status {
				t.Fatalf("state=%+v err=%v polls=%d", r, err, polls)
			}
		})
	}
	observed := 0
	polls := 0
	r, err := AwaitCompletion(context.Background(), func(context.Context) (ReadResult, error) {
		polls++
		status := "running"
		if polls == 2 {
			status = "completed"
		}
		return ReadResult{Status: status}, nil
	}, func(ReadResult) { observed++ })
	if err != nil || r.Status != "completed" || observed != 2 {
		t.Fatalf("%+v %v observations=%d", r, err, observed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AwaitCompletion(ctx, func(context.Context) (ReadResult, error) {
		t.Fatal("polled after cancellation")
		return ReadResult{}, nil
	}, nil); err == nil {
		t.Fatal("missing cancellation")
	}
}

func TestBridgeAttentionIsNonterminalWithReferences(t *testing.T) {
	c, f := setup(t)
	f.approval = true
	ctx := context.Background()
	ref, err := c.Create(ctx, "attention")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Send(ctx, ref.ID, "req", "write", "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Wait(ctx, ref.ID, receipt.TurnID, receipt.Cursor, time.Second)
	if err != nil || r.Terminal || !r.AttentionRequired || r.Attention == nil || r.Attention.SessionID != ref.ID || r.Attention.TurnID != receipt.TurnID || len(r.Attention.Controls) != 1 || r.Attention.Controls[0].ID != "a1" {
		t.Fatalf("missing attention references: %+v %v", r, err)
	}
	if _, err := c.Artifacts(ctx, ref.ID, receipt.TurnID); err == nil {
		t.Fatal("approval wait collected as final artifact")
	}
	f.mu.Lock()
	inputs, cancels := f.inputs, f.cancels
	f.mu.Unlock()
	if inputs != 1 || cancels != 0 {
		t.Fatal("read attention changed execution")
	}
}

func TestDevelopmentEvidenceIsOptInExactTurnAndImmutableOnRetry(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	expected := &executionevidence.Expectations{ValidationCommands: []string{"go test ./example"}}
	receipt, err := c.SendWithExpectations(ctx, ref.ID, "dev-turn", "work", "", expected)
	if err != nil {
		t.Fatal(err)
	}
	// A caller cannot change validation semantics under an existing request id.
	if _, err = c.SendWithExpectations(ctx, ref.ID, "dev-turn", "work", "", nil); err == nil {
		t.Fatal("idempotency ignored evidence contract")
	}
	expected.ValidationCommands[0] = "mutated by caller"
	f.mu.Lock()
	f.events = append(f.events,
		Event{Kind: "turn_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 20, Text: "All tests passed"},
		Event{Kind: "tool_finished", SessionID: ref.ID, TurnID: "other-turn", Seq: 21, ToolName: "run_command", Output: map[string]any{"command": "go test ./example", "exit_code": 0, "decision": "allow"}},
	)
	f.mu.Unlock()
	r, err := c.Read(ctx, ref.ID, receipt.TurnID, 0)
	if err != nil || !r.Terminal || r.AttentionRequired || r.ExecutionEvidence == nil || r.ExecutionEvidence.Status != "needs_validation" || len(r.ExecutionEvidence.MissingValidationCommands) != 1 || r.ExecutionEvidence.MissingValidationCommands[0] != "go test ./example" {
		t.Fatalf("state=%+v err=%v", r, err)
	}
	a, err := c.Artifacts(ctx, ref.ID, receipt.TurnID)
	if err != nil || a.TaskAcceptance != "not_reviewed" || a.ExecutionEvidence == nil || a.ExecutionEvidence.Status != "needs_validation" {
		t.Fatalf("artifacts=%+v err=%v", a, err)
	}
	r, err = c.Read(ctx, ref.ID, receipt.TurnID, 20)
	if err != nil || r.Status != "completed" || !r.Terminal || len(r.Events) != 0 || r.ExecutionEvidence == nil || r.ExecutionEvidence.Status != "needs_validation" {
		t.Fatalf("cursor hid completion/evidence: %+v %v", r, err)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "mutated by caller") {
		t.Fatal("mutable caller contract leaked")
	}
}

func TestAttentionSurvivesMissingLiveRequestAndFiltersForeignControls(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "offline-attention")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.approval = true
	f.events = append(f.events, Event{Kind: "turn_started", SessionID: ref.ID, TurnID: "t1", Seq: 1})
	f.mu.Unlock()
	c.mu.Lock()
	c.controls["foreign"] = json.RawMessage(`{"type":"approval_request","id":"foreign","session_id":"other","turn_id":"t1"}`)
	c.controls["old-turn"] = json.RawMessage(`{"type":"approval_request","id":"old-turn","session_id":"s1","turn_id":"old"}`)
	c.mu.Unlock()
	r, err := c.Read(ctx, ref.ID, "t1", 0)
	if err != nil || r.Status != "waiting_for_human" || r.Terminal || !r.AttentionRequired || r.StreamConnected || len(r.PendingControls) != 0 || r.Attention == nil || len(r.Attention.Controls) != 0 {
		t.Fatalf("missing safe disconnected attention: %+v %v", r, err)
	}
}
