package goal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

// TestGoalRoundTripsThroughSessionStore is the §8.6 acceptance check: a goal
// stored in session.Metadata must survive a real Save → Load (the metadata
// column), so an interrupted session can be resumed with its goal intact.
func TestGoalRoundTripsThroughSessionStore(t *testing.T) {
	store, err := session.NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "sess-1", Metadata: map[string]any{}}
	want := &Goal{
		SessionID:   "sess-1",
		Objective:   "go test ./... 全绿",
		Status:      StatusActive,
		Budget:      Budget{MaxTurns: 20, MaxWall: 30 * time.Minute},
		Spent:       Spend{Turns: 3, Tokens: 12500, Wall: 5 * time.Minute},
		CheckerNote: "auth_test.go 还有 2 个用例红",
		CreatedAt:   time.Now().Truncate(time.Second),
		UpdatedAt:   time.Now().Truncate(time.Second),
	}
	want.IntoSession(sess)

	ctx := context.Background()
	if err := store.Save(ctx, sess); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := store.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, err := FromSession(loaded)
	if err != nil {
		t.Fatalf("FromSession: %v", err)
	}
	if got == nil {
		t.Fatal("expected a goal after resume, got nil")
	}
	if got.Objective != want.Objective || got.Status != want.Status {
		t.Errorf("objective/status mismatch: got %+v", got)
	}
	if got.Spent != want.Spent {
		t.Errorf("spend not preserved across resume: got %+v want %+v", got.Spent, want.Spent)
	}
	if got.Budget != want.Budget {
		t.Errorf("budget not preserved: got %+v want %+v", got.Budget, want.Budget)
	}
	if got.CheckerNote != want.CheckerNote {
		t.Errorf("checker gradient lost: got %q", got.CheckerNote)
	}
}

// TestFromSessionEmpty: a session with no goal yields (nil, nil), not an error.
func TestFromSessionEmpty(t *testing.T) {
	g, err := FromSession(&session.Session{Metadata: map[string]any{}})
	if err != nil || g != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", g, err)
	}
}

// TestClearRemovesGoal: /goal clear drops the active goal so FromSession is empty.
func TestClearRemovesGoal(t *testing.T) {
	sess := &session.Session{ID: "s", Metadata: map[string]any{}}
	(&Goal{SessionID: "s", Objective: "x", Status: StatusPaused}).IntoSession(sess)
	if g, _ := FromSession(sess); g == nil {
		t.Fatal("precondition: goal should be present")
	}
	Clear(sess)
	if g, err := FromSession(sess); err != nil || g != nil {
		t.Fatalf("after Clear want (nil,nil), got (%v,%v)", g, err)
	}
}

// TestParseCheckJSON: the judge's output is robust to fences/prose, and anything
// unparseable degrades to "not met" — never a false "achieved".
func TestParseCheckJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantMet bool
	}{
		{"plain", `{"met":true,"blocked":false,"reason":"all green"}`, true},
		{"fenced", "```json\n{\"met\":true,\"blocked\":false,\"reason\":\"ok\"}\n```", true},
		{"prose around", `Sure! Here is my verdict: {"met":false,"blocked":false,"reason":"2 failing"} hope that helps`, false},
		{"garbage degrades to not-met", `I think it passed`, false},
		{"empty degrades to not-met", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCheckJSON(c.raw); got.Met != c.wantMet {
				t.Errorf("parseCheckJSON(%q).Met = %v, want %v", c.raw, got.Met, c.wantMet)
			}
		})
	}
}

// TestEvidenceProjection: only whitelisted tool results appear, with FULL
// content (not a one-line summary), keyed off the assistant ToolCalls.
func TestEvidenceProjection(t *testing.T) {
	sess := &session.Session{
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "make tests pass"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
				{ID: "c1", Function: model.FunctionCall{Name: "run_command"}},
				{ID: "c2", Function: model.FunctionCall{Name: "read_file"}},
			}},
			{Role: model.RoleTool, ToolCallID: "c1", Content: "FAIL auth_test.go: 2 failed exit_code=1"},
			{Role: model.RoleTool, ToolCallID: "c2", Content: "package main\nfunc main(){}"}, // not whitelisted
		},
	}
	ev := NewTranscript(sess).Evidence()
	if !strings.Contains(ev, "FAIL auth_test.go") {
		t.Errorf("whitelisted run_command result missing from evidence:\n%s", ev)
	}
	if strings.Contains(ev, "package main") {
		t.Errorf("non-whitelisted read_file content leaked into evidence:\n%s", ev)
	}
}
