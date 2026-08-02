package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"code-agent/internal/session"
	"code-agent/internal/sessionfork"
	"code-agent/internal/tools"
)

type stubSessionIndex struct {
	sessions []tools.SessionIndexEntry
	detail   *tools.SessionIndexDetail
}

type stubSessionControl struct {
	sent          tools.SessionSendRequest
	targets       []tools.SessionWaitTarget
	createRequest tools.SessionCreateRequest
	forkRequest   sessionfork.Request
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

func (s *stubSessionControl) PollTurn(_ context.Context, sessionID, turnID string, cursor int64) (*tools.SessionWaitCompletion, int64, error) {
	return &tools.SessionWaitCompletion{SessionID: sessionID, TurnID: turnID, Status: "completed", Cursor: cursor + 1}, cursor + 1, nil
}

func (s *stubSessionControl) ForkSession(_ context.Context, request sessionfork.Request) (sessionfork.Result, error) {
	s.forkRequest = request
	return sessionfork.Result{ID: "fork", ParentSessionID: request.ParentSessionID, SourceSessionID: request.SourceSessionID, WorkspacePath: "/fork", Kind: "fork", Status: "open"}, nil
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
	var forkResult sessionfork.Result
	if err := json.Unmarshal(forked.Output, &forkResult); err != nil {
		t.Fatal(err)
	}
	if forkResult.ID != "fork" || control.forkRequest.ParentSessionID != "parent" || control.forkRequest.SourceSessionID != "source" || control.forkRequest.Name != "branch" || control.forkRequest.ExecutionPolicy != session.ExecutionPolicySharedWorkspace || control.forkRequest.RequestID == "" {
		t.Fatalf("result=%+v request=%+v", forkResult, control.forkRequest)
	}

	ec.CallID = "call-managed-fork"
	if _, err := (&ForkSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"id":"source","execution_policy":"isolated_worktree","worktree_name":"snapshot"}`)); err != nil {
		t.Fatal(err)
	}
	if control.forkRequest.ExecutionPolicy != session.ExecutionPolicyIsolatedWorktree || control.forkRequest.WorktreeName != "snapshot" {
		t.Fatalf("managed fork request=%+v", control.forkRequest)
	}
	if _, err := (&ForkSessionTool{}).Execute(context.Background(), ec, json.RawMessage(`{"id":"source","worktree_name":"invalid"}`)); err == nil {
		t.Fatal("shared fork accepted worktree_name")
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

// TestSessionToolOutputSchemasMatchMarshaledStructs verifies each OutputSchema
// is a faithful contract for the JSON the tool actually emits: every field the
// marshaled struct can produce must be declared, and every declared field must
// exist on the struct. A planner consuming these schemas depends on that
// bijection, so drift here is a real contract bug.
func TestSessionToolOutputSchemasMatchMarshaledStructs(t *testing.T) {
	type nestedCase struct {
		array string
		item  any
	}
	cases := []struct {
		name     string
		tool     tools.OutputSchemaProvider
		topLevel any
		nested   []nestedCase
	}{
		{name: "send_to_session", tool: &SendToSessionTool{}, topLevel: tools.SessionDelivery{}},
		{
			name:     "wait_sessions",
			tool:     &WaitSessionsTool{},
			topLevel: tools.SessionWaitResult{},
			nested: []nestedCase{
				{"completed", tools.SessionWaitCompletion{}},
				{"waiting", tools.SessionWaitTarget{}},
			},
		},
		{name: "read_session", tool: &ReadSessionTool{}, topLevel: tools.SessionIndexDetail{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tc.tool.OutputSchema(), &schema); err != nil {
				t.Fatalf("OutputSchema is not valid JSON: %v", err)
			}
			if len(schema.Properties) == 0 {
				t.Fatal("OutputSchema declares no properties")
			}
			checkSchemaFieldContract(t, "top-level", schema.Properties, jsonFieldNames(reflect.TypeOf(tc.topLevel)))
			for _, nested := range tc.nested {
				fieldRaw, ok := schema.Properties[nested.array]
				if !ok {
					t.Errorf("%s[]: array field is not declared in OutputSchema", nested.array)
					continue
				}
				var field struct {
					Items json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(fieldRaw, &field); err != nil {
					t.Fatalf("%s[] field schema is not valid JSON: %v", nested.array, err)
				}
				var items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				}
				if err := json.Unmarshal(field.Items, &items); err != nil {
					t.Fatalf("%s[] items schema is not valid JSON: %v", nested.array, err)
				}
				checkSchemaFieldContract(t, nested.array+"[]", items.Properties, jsonFieldNames(reflect.TypeOf(nested.item)))
			}
		})
	}
}

// checkSchemaFieldContract asserts the schema and the marshaled struct agree on
// the set of JSON field names: the schema must describe every field the struct
// can emit, and every declared field must actually exist on the struct.
func checkSchemaFieldContract(t *testing.T, label string, schemaProperties map[string]json.RawMessage, structFields map[string]struct{}) {
	t.Helper()
	for field := range structFields {
		if _, ok := schemaProperties[field]; !ok {
			t.Errorf("%s: struct emits %q but OutputSchema does not declare it (schema: %v)", label, field, sortedFieldNames(schemaProperties))
		}
	}
	for field := range schemaProperties {
		if _, ok := structFields[field]; !ok {
			t.Errorf("%s: OutputSchema declares %q but the marshaled struct has no such JSON field", label, field)
		}
	}
}

// jsonFieldNames returns the JSON field names a struct marshals, honoring the
// json tag and skipping fields tagged "-".
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	names := map[string]struct{}{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names[name] = struct{}{}
	}
	return names
}

func sortedFieldNames(properties map[string]json.RawMessage) []string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
