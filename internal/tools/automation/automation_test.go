package automation

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/automation"
	"code-agent/internal/tools"
)

// fakeStore is a minimal in-memory AutomationStore for tool tests.
type fakeStore struct {
	items map[string]automation.Automation
	seq   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: map[string]automation.Automation{}}
}

func (f *fakeStore) Create(ctx context.Context, a automation.Automation) (automation.Automation, error) {
	f.seq++
	a.ID = "auto_" + string(rune('a'+f.seq))
	f.items[a.ID] = a
	return a, nil
}
func (f *fakeStore) Get(ctx context.Context, id string) (automation.Automation, error) {
	a, ok := f.items[id]
	if !ok {
		return automation.Automation{}, &notFoundError{id}
	}
	return a, nil
}
func (f *fakeStore) List(ctx context.Context) ([]automation.Automation, error) {
	out := make([]automation.Automation, 0, len(f.items))
	for _, a := range f.items {
		out = append(out, a)
	}
	return out, nil
}
func (f *fakeStore) Update(ctx context.Context, id string, patch automation.AutomationPatch) (automation.Automation, error) {
	a, ok := f.items[id]
	if !ok {
		return automation.Automation{}, &notFoundError{id}
	}
	if patch.Name != nil {
		a.Name = *patch.Name
	}
	f.items[id] = a
	return a, nil
}
func (f *fakeStore) Delete(ctx context.Context, id string) error {
	delete(f.items, id)
	return nil
}
func (f *fakeStore) ListRuns(ctx context.Context, automationID string, limit int) ([]automation.Run, error) {
	return nil, nil
}

type notFoundError struct{ id string }

func (e *notFoundError) Error() string { return "automation " + e.id + " not found" }

func execTool(t *testing.T, tool tools.Tool, ec tools.ExecutionContext, input string) (tools.ToolResult, error) {
	t.Helper()
	return tool.Execute(context.Background(), ec, json.RawMessage(input))
}

func TestAutomationCreate(t *testing.T) {
	store := newFakeStore()
	ec := tools.ExecutionContext{AutomationStore: store, WorkspaceRoot: "/ws"}
	tool := &AutomationTool{}
	res, err := execTool(t, tool, ec, `{"mode":"create","name":"daily","prompt":"summarize","schedule_type":"recurring","rrule":"FREQ=DAILY;BYHOUR=9","timezone":"UTC"}`)
	if err != nil {
		t.Fatal(err)
	}
	var created automation.Automation
	if err := json.Unmarshal(res.Output, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "daily" || created.CreatedFromWorkspace != "/ws" {
		t.Fatalf("unexpected created: %+v", created)
	}
}

func TestAutomationCreateValidation(t *testing.T) {
	store := newFakeStore()
	ec := tools.ExecutionContext{AutomationStore: store}
	tool := &AutomationTool{}
	// once without scheduled_at
	if _, err := execTool(t, tool, ec, `{"mode":"create","name":"x","prompt":"p","schedule_type":"once","timezone":"UTC"}`); err == nil {
		t.Fatal("expected error: once requires scheduled_at")
	}
	// recurring without rrule
	if _, err := execTool(t, tool, ec, `{"mode":"create","name":"x","prompt":"p","schedule_type":"recurring","timezone":"UTC"}`); err == nil {
		t.Fatal("expected error: recurring requires rrule")
	}
	// missing timezone
	if _, err := execTool(t, tool, ec, `{"mode":"create","name":"x","prompt":"p","schedule_type":"recurring","rrule":"FREQ=DAILY"}`); err == nil {
		t.Fatal("expected error: timezone required")
	}
}

func TestAutomationListViewUpdateDelete(t *testing.T) {
	store := newFakeStore()
	ec := tools.ExecutionContext{AutomationStore: store}
	tool := &AutomationTool{}
	created, err := execTool(t, tool, ec, `{"mode":"create","name":"a","prompt":"p","schedule_type":"recurring","rrule":"FREQ=DAILY","timezone":"UTC"}`)
	if err != nil {
		t.Fatal(err)
	}
	var a automation.Automation
	_ = json.Unmarshal(created.Output, &a)

	// list
	res, err := execTool(t, tool, ec, `{"mode":"list"}`)
	if err != nil {
		t.Fatal(err)
	}
	var list []automation.Automation
	if err := json.Unmarshal(res.Output, &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// view
	if _, err := execTool(t, tool, ec, `{"mode":"view","id":"`+a.ID+`"}`); err != nil {
		t.Fatal(err)
	}

	// update (partial)
	upd, err := execTool(t, tool, ec, `{"mode":"update","id":"`+a.ID+`","name":"renamed"}`)
	if err != nil {
		t.Fatal(err)
	}
	var updated automation.Automation
	_ = json.Unmarshal(upd.Output, &updated)
	if updated.Name != "renamed" || updated.Prompt != "p" {
		t.Fatalf("update partial failed: %+v", updated)
	}

	// delete
	if _, err := execTool(t, tool, ec, `{"mode":"delete","id":"`+a.ID+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := execTool(t, tool, ec, `{"mode":"view","id":"`+a.ID+`"}`); err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestAutomationCreateUsesSessionModel(t *testing.T) {
	store := newFakeStore()
	// ec.Model carries the parent turn's resolved model name; when the user does
	// not pass model_id, it must be used (Problem 1: don't fall to a settings
	// default whose provider may be out of quota).
	ec := tools.ExecutionContext{AutomationStore: store, WorkspaceRoot: "/ws", Model: "deepseek-v4-flash"}
	tool := &AutomationTool{}
	res, err := execTool(t, tool, ec, `{"mode":"create","name":"daily","prompt":"p","schedule_type":"recurring","rrule":"FREQ=DAILY","timezone":"UTC"}`)
	if err != nil {
		t.Fatal(err)
	}
	var created automation.Automation
	if err := json.Unmarshal(res.Output, &created); err != nil {
		t.Fatal(err)
	}
	if created.ModelID != "deepseek-v4-flash" {
		t.Fatalf("model_id = %q, want deepseek-v4-flash (from ec.Model)", created.ModelID)
	}
}

func TestAutomationNilStore(t *testing.T) {
	tool := &AutomationTool{}
	// nil store must error, not panic
	if _, err := execTool(t, tool, tools.ExecutionContext{}, `{"mode":"list"}`); err == nil {
		t.Fatal("expected error when store is nil")
	}
}

func TestGetCurrentTime(t *testing.T) {
	tool := &GetCurrentTimeTool{}
	res, err := execTool(t, tool, tools.ExecutionContext{}, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out["now"] == "" || out["timezone"] == "" {
		t.Fatalf("missing fields: %+v", out)
	}
	if _, ok := out["utc_offset"]; !ok {
		t.Fatalf("missing utc_offset: %+v", out)
	}
}
