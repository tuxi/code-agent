package sessions

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type stubSessionIndex struct {
	sessions []tools.SessionIndexEntry
	detail   *tools.SessionIndexDetail
}

type stubSessionControl struct {
	sent          tools.SessionSendRequest
	targets       []tools.SessionWaitTarget
	createRequest tools.SessionCreateRequest
	forkRequest   tools.SessionForkRequest
}

func (s *stubSessionControl) Send(_ context.Context, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	s.sent = request
	return tools.SessionDelivery{Accepted: true, Delivery: "queued", SessionID: request.TargetSessionID, TurnID: "turn-target", Cursor: 7}, nil
}

func (s *stubSessionControl) WaitAny(_ context.Context, targets []tools.SessionWaitTarget, _ time.Duration) (tools.SessionWaitResult, error) {
	s.targets = append([]tools.SessionWaitTarget(nil), targets...)
	return tools.SessionWaitResult{Completed: []tools.SessionWaitCompletion{{SessionID: targets[0].SessionID, TurnID: targets[0].TurnID, Status: "completed", Cursor: 9}}}, nil
}

func (s *stubSessionControl) CreateSession(_ context.Context, request tools.SessionCreateRequest) (tools.SessionSpawnResult, error) {
	s.createRequest = request
	return tools.SessionSpawnResult{ID: "child", ParentSessionID: request.ParentSessionID, WorkspacePath: request.WorkspacePath, Kind: "spawn", Status: "open"}, nil
}

func (s *stubSessionControl) ForkSession(_ context.Context, request tools.SessionForkRequest) (tools.SessionSpawnResult, error) {
	s.forkRequest = request
	return tools.SessionSpawnResult{ID: "fork", ParentSessionID: request.ParentSessionID, SourceSessionID: request.SourceSessionID, WorkspacePath: "/fork", Kind: "fork", Status: "open"}, nil
}

func (s stubSessionIndex) ListAll() ([]tools.SessionIndexEntry, error) {
	return s.sessions, nil
}

func (s stubSessionIndex) Get(id string) (*tools.SessionIndexEntry, error) {
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			entry := s.sessions[i]
			return &entry, nil
		}
	}
	return nil, nil
}

func (s stubSessionIndex) Read(context.Context, string) (*tools.SessionIndexDetail, error) {
	return s.detail, nil
}

func TestListSessionsContentMatchesStructuredOutput(t *testing.T) {
	index := stubSessionIndex{sessions: []tools.SessionIndexEntry{
		{ID: "session-1", WorkspacePath: "/workspace/one", Name: "First"},
		{ID: "session-2", WorkspacePath: "/workspace/two", Name: "Second"},
	}}

	result, err := (&ListSessionsTool{}).Execute(
		context.Background(),
		tools.ExecutionContext{SessionIndex: index},
		json.RawMessage(`{"limit": 1}`),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Content == "" {
		t.Fatal("Content is empty; the model would not see the session list")
	}
	if result.Content != string(result.Output) {
		t.Fatalf("Content and Output differ:\nContent: %s\nOutput: %s", result.Content, result.Output)
	}

	var sessions []tools.SessionIndexEntry
	if err := json.Unmarshal([]byte(result.Content), &sessions); err != nil {
		t.Fatalf("Content is not valid session JSON: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "session-1" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
}

func TestReadSessionContentMatchesStructuredOutput(t *testing.T) {
	index := stubSessionIndex{detail: &tools.SessionIndexDetail{
		ID:            "session-1",
		WorkspacePath: "/workspace/one",
		Name:          "First",
		LastTurn:      "Completed the requested review.",
	}}

	result, err := (&ReadSessionTool{}).Execute(
		context.Background(),
		tools.ExecutionContext{SessionIndex: index},
		json.RawMessage(`{"id": "session-1"}`),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Content == "" {
		t.Fatal("Content is empty; the model would not see the session detail")
	}
	if result.Content != string(result.Output) {
		t.Fatalf("Content and Output differ:\nContent: %s\nOutput: %s", result.Content, result.Output)
	}

	var detail tools.SessionIndexDetail
	if err := json.Unmarshal([]byte(result.Content), &detail); err != nil {
		t.Fatalf("Content is not valid session JSON: %v", err)
	}
	if detail.ID != "session-1" || detail.LastTurn == "" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
}

func TestListSessionsFiltersIdleAndArchived(t *testing.T) {
	index := stubSessionIndex{sessions: []tools.SessionIndexEntry{
		{ID: "idle-active", WorkspacePath: "/workspace/one"},
		{ID: "running", WorkspacePath: "/workspace/one", TurnStatus: "running"},
		{ID: "idle-archived", WorkspacePath: "/workspace/one", ArchivedAt: "2026-08-01T00:00:00Z"},
	}}

	result, err := (&ListSessionsTool{}).Execute(context.Background(), tools.ExecutionContext{SessionIndex: index}, json.RawMessage(`{"status":"idle"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got []tools.SessionIndexEntry
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].ID != "idle-active" {
		t.Fatalf("default archive/idle filtering = %+v", got)
	}

	result, err = (&ListSessionsTool{}).Execute(context.Background(), tools.ExecutionContext{SessionIndex: index}, json.RawMessage(`{"status":"idle","include_archived":true}`))
	if err != nil {
		t.Fatalf("Execute include archived: %v", err)
	}
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal include archived: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("include_archived filtering = %+v", got)
	}
}

func TestListSessionsClampsLimitToMaximum(t *testing.T) {
	entries := make([]tools.SessionIndexEntry, 205)
	for i := range entries {
		entries[i] = tools.SessionIndexEntry{ID: fmt.Sprintf("session-%03d", i)}
	}
	result, err := (&ListSessionsTool{}).Execute(
		context.Background(),
		tools.ExecutionContext{SessionIndex: stubSessionIndex{sessions: entries}},
		json.RawMessage(`{"limit":999}`),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got []tools.SessionIndexEntry
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 200 {
		t.Fatalf("len = %d, want 200", len(got))
	}
}

func TestSendAndWaitSessionsPreserveTurnCursorScope(t *testing.T) {
	control := &stubSessionControl{}
	ec := tools.ExecutionContext{SessionID: "sender", TurnID: "sender-turn", SessionControl: control}
	sent, err := (&SendToSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"id":"target","message":"do work","correlation_id":"corr","intent":"notification"}`))
	if err != nil {
		t.Fatal(err)
	}
	var delivery tools.SessionDelivery
	if err := json.Unmarshal(sent.Output, &delivery); err != nil {
		t.Fatal(err)
	}
	if delivery.TurnID != "turn-target" || delivery.Cursor != 7 || control.sent.SenderSessionID != "sender" || control.sent.SenderTurnID != "sender-turn" || control.sent.MessageID == "" || control.sent.CorrelationID != "corr" || control.sent.Intent != "notification" {
		t.Fatalf("delivery=%+v request=%+v", delivery, control.sent)
	}
	waited, err := (&WaitSessionsTool{}).Execute(context.Background(), ec, json.RawMessage(`{"targets":[{"id":"target","turn_id":"turn-target","cursor":7}],"timeout_ms":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var result tools.SessionWaitResult
	if err := json.Unmarshal(waited.Output, &result); err != nil {
		t.Fatal(err)
	}
	if len(control.targets) != 1 || control.targets[0].Cursor != 7 || len(result.Completed) != 1 || result.Completed[0].Cursor != 9 {
		t.Fatalf("targets=%+v result=%+v", control.targets, result)
	}
}

func TestCreateAndForkSessionPassCallerScope(t *testing.T) {
	control := &stubSessionControl{}
	ec := tools.ExecutionContext{SessionID: "parent", TurnID: "turn", CallID: "call-create", WorkspaceRoot: "/workspace/default", SessionControl: control}

	created, err := (&CreateSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"name":"child","execution_policy":"read_only"}`))
	if err != nil {
		t.Fatal(err)
	}
	var createResult tools.SessionSpawnResult
	if err := json.Unmarshal(created.Output, &createResult); err != nil {
		t.Fatal(err)
	}
	if createResult.ID != "child" || control.createRequest.ParentSessionID != "parent" || control.createRequest.WorkspacePath != "/workspace/default" || control.createRequest.ExecutionPolicy != "read_only" || control.createRequest.RequestID == "" {
		t.Fatalf("result=%+v request=%+v", createResult, control.createRequest)
	}
	createRequestID := control.createRequest.RequestID
	if _, err := (&CreateSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"name":"child","execution_policy":"read_only"}`)); err != nil || control.createRequest.RequestID != createRequestID {
		t.Fatalf("create retry request_id=%q, want %q, err=%v", control.createRequest.RequestID, createRequestID, err)
	}

	ec.CallID = "call-fork"
	forked, err := (&ForkSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"id":"source","name":"branch"}`))
	if err != nil {
		t.Fatal(err)
	}
	var forkResult tools.SessionSpawnResult
	if err := json.Unmarshal(forked.Output, &forkResult); err != nil {
		t.Fatal(err)
	}
	if forkResult.ID != "fork" || control.forkRequest.ParentSessionID != "parent" || control.forkRequest.SourceSessionID != "source" || control.forkRequest.Name != "branch" || control.forkRequest.RequestID == "" {
		t.Fatalf("result=%+v request=%+v", forkResult, control.forkRequest)
	}
}

func TestCreateSessionPassesManagedWorktreeOptions(t *testing.T) {
	control := &stubSessionControl{}
	ec := tools.ExecutionContext{SessionID: "parent", TurnID: "turn", CallID: "call-worktree", WorkspaceRoot: "/workspace/source", SessionControl: control}
	_, err := (&CreateSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"name":"child","execution_policy":"isolated_worktree","worktree_name":"feature","base_ref":"fresh"}`))
	if err != nil {
		t.Fatal(err)
	}
	request := control.createRequest
	if request.ExecutionPolicy != "isolated_worktree" || request.WorkspacePath != "/workspace/source" || request.WorktreeName != "feature" || request.BaseRef != "fresh" {
		t.Fatalf("request = %+v", request)
	}
	if _, err := (&CreateSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"base_ref":"head"}`)); err == nil {
		t.Fatal("non-isolated create accepted worktree options")
	}
}
