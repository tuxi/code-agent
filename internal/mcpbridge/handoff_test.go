package mcpbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/executionevidence"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type uxRoundTrip func(*http.Request) (*http.Response, error)

func (f uxRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadUsesOneSnapshotForLifecycleDeltaAndCursor(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	original := c.http.Transport
	requests := 0
	c.http.Transport = uxRoundTrip(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/events") {
			return original.RoundTrip(req)
		}
		requests++
		events := []Event{{Kind: "turn_accepted", SessionID: ref.ID, TurnID: "t1", RequestID: "r", Seq: 10}, {Kind: "turn_started", SessionID: ref.ID, TurnID: "t1", Seq: 12}}
		if requests > 1 {
			events = append(events, Event{Kind: "turn_finished", SessionID: ref.ID, TurnID: "t1", Seq: 14})
		}
		body, _ := json.Marshal(events)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	r, err := c.Read(ctx, ref.ID, "t1", 12)
	if err != nil || requests != 1 || r.Status != "running" || r.Cursor != 12 || len(r.Events) != 0 {
		t.Fatalf("inconsistent observation: %+v requests=%d err=%v", r, requests, err)
	}
}

func TestQuietUpdateSkipsTranscriptAndPreservesTerminal(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "quiet")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Send(ctx, ref.ID, "quiet-turn", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = append(f.events, Event{Kind: "text_delta", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 13, Text: strings.Repeat("private transcript ", 10000)})
	f.mu.Unlock()
	u, err := c.WaitForUpdate(ctx, ref.ID, receipt.TurnID, 12, 0)
	if err != nil || u.Changed || u.Cursor != 13 || u.ExecutionStatus != "running" {
		t.Fatalf("%+v %v", u, err)
	}
	b, _ := json.Marshal(u)
	r, err := c.Read(ctx, ref.ID, receipt.TurnID, 12)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := json.Marshal(r)
	if len(b) > 512 || strings.Contains(string(b), "private transcript") {
		t.Fatalf("unbounded update: %d", len(b))
	}
	t.Logf("same event stream: read=%d bytes quiet=%d bytes", len(full), len(b))
	start := time.Now()
	u, err = c.WaitForUpdate(ctx, ref.ID, receipt.TurnID, 13, 20*time.Millisecond)
	if err != nil || u.Changed || u.Cursor != 13 || time.Since(start) < 15*time.Millisecond {
		t.Fatalf("%+v %v", u, err)
	}
	f.mu.Lock()
	f.events = append(f.events, Event{Kind: "turn_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 14, Text: "MODEL CLAIM: all accepted"})
	f.mu.Unlock()
	u, err = c.WaitForUpdate(ctx, ref.ID, receipt.TurnID, 14, 0)
	if err != nil || !u.Terminal || u.Changed || u.ExecutionStatus != "completed" || u.HandoffTool != "codeagent_get_handoff" {
		t.Fatalf("terminal lost: %+v %v", u, err)
	}
	if _, err = c.WaitForUpdate(ctx, "foreign", receipt.TurnID, 0, 0); err == nil {
		t.Fatal("foreign Session accepted")
	}
	if _, err = c.WaitForUpdate(ctx, ref.ID, receipt.TurnID, -1, 0); err == nil {
		t.Fatal("negative cursor accepted")
	}
	if _, err = c.WaitForUpdate(ctx, ref.ID, receipt.TurnID, 0, 31*time.Second); err == nil {
		t.Fatal("unbounded wait")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = c.WaitForUpdate(cancelled, ref.ID, receipt.TurnID, 0, time.Second); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestAdmissionLookupDoesNotInferAbsenceOrResubmit(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.LookupAdmission(ctx, ref.ID, "uncertain")
	if err != nil || a.Outcome != "unknown" || a.Receipt != nil {
		t.Fatalf("%+v %v", a, err)
	}
	receipt, err := c.Send(ctx, ref.ID, "uncertain", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err = c.LookupAdmission(ctx, ref.ID, "uncertain")
	if err != nil || a.Outcome != "admitted" || a.Receipt == nil || *a.Receipt != receipt {
		t.Fatalf("%+v %v", a, err)
	}
	f.mu.Lock()
	if f.inputs != 1 {
		t.Fatal("lookup dispatched")
	}
	f.events = append(f.events, Event{Kind: "turn_accepted", SessionID: ref.ID, TurnID: "other", RequestID: "uncertain", Seq: 16})
	f.mu.Unlock()
	if _, err = c.LookupAdmission(ctx, ref.ID, "uncertain"); err == nil {
		t.Fatal("ambiguous admission accepted")
	}
}

func TestHandoffSeparatesExecutionValidationAcceptance(t *testing.T) {
	for _, mode := range []string{"complete", "missing", "failed", "cancelled", "undeclared", "later_mutation"} {
		t.Run(mode, func(t *testing.T) {
			c, f := setup(t)
			ctx := context.Background()
			ref, err := c.Create(ctx, "handoff")
			if err != nil {
				t.Fatal(err)
			}
			var expected *executionevidence.Expectations
			if mode != "undeclared" {
				expected = &executionevidence.Expectations{ValidationCommands: []string{"swift test"}}
			}
			receipt, err := c.SendWithExpectations(ctx, ref.ID, "handoff-turn", "work", "", expected)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Handoff(ctx, ref.ID, receipt.TurnID); err == nil {
				t.Fatal("active handoff accepted")
			}
			if err = os.WriteFile(filepath.Join(c.root, "new file.txt"), []byte("real diff content\n"), 0600); err != nil {
				t.Fatal(err)
			}
			end := "turn_finished"
			code := 0
			if mode == "failed" {
				code = 1
			}
			if mode == "cancelled" {
				end = "turn_cancelled"
			}
			f.mu.Lock()
			if mode != "missing" {
				f.events = append(f.events,
					Event{Kind: "tool_started", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 13, CallID: "test", ToolName: "run_command", ToolArgs: map[string]any{"command": "swift test"}},
					Event{Kind: "tool_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 14, CallID: "test", ToolName: "run_command", Output: map[string]any{"command": "swift test", "status": "completed", "decision": "confirm", "exit_code": code, "stdout": strings.Repeat("long log ", 10000)}},
				)
			}
			if mode == "later_mutation" {
				f.events = append(f.events,
					Event{Kind: "tool_started", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 15, CallID: "edit", ToolName: "edit_file", ToolArgs: map[string]any{"path": "new file.txt"}},
					Event{Kind: "tool_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 16, CallID: "edit", ToolName: "edit_file"})
			}
			f.events = append(f.events, Event{Kind: end, SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 20, Text: "Tests passed, task accepted"})
			f.mu.Unlock()
			h, err := c.Handoff(ctx, ref.ID, receipt.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			want := "partial"
			if mode == "complete" {
				want = "complete"
			}
			if mode == "undeclared" {
				want = "unknown"
			}
			if h.ValidationStatus != want || h.AcceptanceStatus != "not_reviewed" || h.AttemptID != nil || h.DiffSHA256 == "" || h.DiffTruncated || len(h.ChangedFiles) != 1 || h.ChangedFiles[0] != "new file.txt" {
				t.Fatalf("%+v", h)
			}
			if mode == "cancelled" && h.ExecutionStatus != "cancelled" {
				t.Fatal(h.ExecutionStatus)
			}
			if mode != "missing" && (len(h.Commands) != 1 || h.Commands[0].Evidence.SourceSeq != 14 || h.Commands[0].Evidence.TurnID != receipt.TurnID) {
				t.Fatalf("bad references: %+v", h.Commands)
			}
			b, _ := json.Marshal(h)
			if strings.Contains(string(b), "long log") || strings.Contains(string(b), "real diff content") || strings.Contains(string(b), "Tests passed, task accepted") {
				t.Fatal("handoff leaked transcript/log/diff")
			}
			a, err := c.Artifacts(ctx, ref.ID, receipt.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			full, _ := json.Marshal(a)
			t.Logf("mode=%s artifacts=%d handoff=%d", mode, len(full), len(b))
		})
	}
}

func TestQuietAttentionAndNativeToolContract(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	f.approval = true
	ref, err := c.Create(ctx, "attention-ux")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Send(ctx, ref.ID, "attention-ux-turn", "edit", "")
	if err != nil {
		t.Fatal(err)
	}
	a, b := mcp.NewInMemoryTransports()
	s, err := Server(c).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "native-contract"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name == "codeagent_send_to_session" && !strings.Contains(tool.Description, "END THIS RESPONSE") {
			t.Fatal("send defaults to waiting")
		}
		if tool.Name == "codeagent_wait_for_update" && !strings.Contains(tool.Description, "explicitly requests waiting") {
			t.Fatal("wait is not opt-in")
		}
	}
	for _, name := range []string{"codeagent_connection_info", "codeagent_wait_for_update", "codeagent_lookup_admission", "codeagent_get_handoff"} {
		found := false
		for _, tool := range list.Tools {
			if tool.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatal("missing tool", name)
		}
	}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_wait_for_update", Arguments: UpdateInput{SessionID: ref.ID, TurnID: receipt.TurnID, AfterCursor: 12, TimeoutSeconds: 0}})
	if err != nil || result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	data, _ := json.Marshal(result.StructuredContent)
	var update Update
	if err = json.Unmarshal(data, &update); err != nil {
		t.Fatal(err)
	}
	if update.Attention == nil || update.Attention.SessionID != ref.ID || update.Terminal {
		t.Fatalf("%+v", update)
	}
	if strings.Contains(string(data), "tool_args") {
		t.Fatal("approval payload leaked")
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_connection_info", Arguments: struct{}{}})
	if err != nil || result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	data, _ = json.Marshal(result.StructuredContent)
	if strings.Contains(string(data), "test-token") {
		t.Fatal("credential leaked")
	}
	f.mu.Lock()
	inputs, cancels := f.inputs, f.cancels
	f.mu.Unlock()
	if inputs != 1 || cancels != 0 {
		t.Fatal("read tools changed execution")
	}
}

func TestHandoffRejectsAttentionAndDoesNotCompleteFailedTurn(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	f.approval = true
	ref, err := c.Create(ctx, "handoff-attention")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.SendWithExpectations(ctx, ref.ID, "handoff-attention-turn", "work", "", &executionevidence.Expectations{ValidationCommands: []string{"go version"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.root, "partial.txt"), []byte("not an accepted result"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Handoff(ctx, ref.ID, receipt.TurnID); err == nil {
		t.Fatal("waiting_for_human produced final handoff")
	}
	f.mu.Lock()
	f.approval = false
	f.events = append(f.events, Event{Kind: "turn_failed", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 20, Reason: "fixture failure"})
	f.mu.Unlock()
	h, err := c.Handoff(ctx, ref.ID, receipt.TurnID)
	if err != nil || h.ExecutionStatus != "failed" || h.ValidationStatus != "partial" || h.AcceptanceStatus != "not_reviewed" || len(h.ChangedFiles) != 1 {
		t.Fatalf("failed partial edit became completion: %+v %v", h, err)
	}
}

func TestHandoffCommandRefsSurviveRepeatedCallIDs(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "reused-call-id")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Send(ctx, ref.ID, "reused-call-turn", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = append(f.events,
		Event{Kind: "tool_started", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 13, CallID: "reused", ToolName: "run_command", ToolArgs: map[string]any{"command": "go version"}},
		Event{Kind: "tool_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 14, CallID: "reused", ToolName: "run_command", Output: map[string]any{"command": "go version", "exit_code": 0, "status": "completed", "decision": "allow"}},
		Event{Kind: "tool_started", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 15, CallID: "reused", ToolName: "run_command", ToolArgs: map[string]any{"command": "go env GOVERSION"}},
		Event{Kind: "tool_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 16, CallID: "reused", ToolName: "run_command", Output: map[string]any{"command": "go env GOVERSION", "exit_code": 0, "status": "completed", "decision": "allow"}},
		Event{Kind: "turn_finished", SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 20})
	f.mu.Unlock()
	h, err := c.Handoff(ctx, ref.ID, receipt.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Commands) != 2 || h.Commands[0].Evidence.SourceSeq != 14 || h.Commands[1].Evidence.SourceSeq != 16 {
		t.Fatalf("command references alias repeated call IDs: %+v", h.Commands)
	}
}

func TestHandoffTaskBindingUsesExactPersistedAttempt(t *testing.T) {
	c := &Client{binding: &connectionBinding{verifiedServerID: "server", verifiedStoreID: "store"}}
	snapshot := collab.Snapshot{CodingTask: collab.CodingTask{TaskID: "task", Revision: "4", TaskAcceptance: "accepted"}, Attempts: []collab.TaskAttempt{{AttemptID: "attempt", TaskID: "task", SourceRef: collab.SourceRef{Kind: "session_event", ServerID: "server", StoreID: "store", SessionID: "session", TurnID: "turn"}}}}
	h := Handoff{SessionID: "session", TurnID: "turn", AcceptanceStatus: "not_reviewed"}
	if err := c.bindHandoffTask(&h, snapshot); err != nil || h.AttemptID == nil || *h.AttemptID != "attempt" || h.TaskAcceptance != "accepted" || h.AcceptanceStatus != "not_reviewed" {
		t.Fatalf("%+v %v", h, err)
	}
	for _, mode := range []string{"wrong_turn", "wrong_store", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			s := snapshot
			s.Attempts = append([]collab.TaskAttempt{}, snapshot.Attempts...)
			switch mode {
			case "wrong_turn":
				s.Attempts[0].SourceRef.TurnID = "other"
			case "wrong_store":
				s.Attempts[0].SourceRef.StoreID = "other"
			case "duplicate":
				s.Attempts = append(s.Attempts, s.Attempts[0])
			}
			h := Handoff{SessionID: "session", TurnID: "turn"}
			if err := c.bindHandoffTask(&h, s); err == nil {
				t.Fatal("ambiguous/foreign binding accepted")
			}
		})
	}
}
