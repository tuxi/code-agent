package sessions

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

type stubSessionIndex struct {
	sessions []tools.SessionIndexEntry
	detail   *tools.SessionIndexDetail
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
