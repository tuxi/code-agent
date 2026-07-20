package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/tools"
)

func TestAskUserToolSchema(t *testing.T) {
	tool := NewAskUserTool(&RunnerRef{})

	if tool.Name() != "ask_user" {
		t.Errorf("Name() = %q, want %q", tool.Name(), "ask_user")
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}

	schema := tool.InputSchema()
	var s struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if s.Type != "object" {
		t.Errorf("schema type = %q, want %q", s.Type, "object")
	}
	required := make(map[string]bool)
	for _, r := range s.Required {
		required[r] = true
	}
	for _, field := range []string{"question", "header", "options"} {
		if !required[field] {
			t.Errorf("field %q should be required", field)
		}
	}
	if _, ok := s.Properties["options"]; !ok {
		t.Error("schema missing 'options' property")
	}
}

func TestAskUserToolHeadlessFallback(t *testing.T) {
	// No RunnerRef → runner not wired.
	tool := NewAskUserTool(&RunnerRef{})
	_, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(`{
		"question": "Which approach?",
		"header": "Approach",
		"options": [
			{"label": "Option A", "description": "First way"},
			{"label": "Option B", "description": "Second way"}
		]
	}`))
	if err == nil {
		t.Fatal("expected error when runner is not wired")
	}
	if !strings.Contains(err.Error(), "runner not wired") {
		t.Errorf("error = %q, want 'runner not wired'", err.Error())
	}
}

func TestAskUserToolNoApproverFallback(t *testing.T) {
	// Runner wired but no AskUserApprover → fallback message.
	r := &Runner{Approver: nil} // no AskUserApprover set
	ref := &RunnerRef{R: r}
	tool := NewAskUserTool(ref)

	result, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(`{
		"question": "Which approach?",
		"header": "Approach",
		"options": [
			{"label": "Option A", "description": "First way"},
			{"label": "Option B", "description": "Second way"}
		]
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != fallbackMessage {
		t.Errorf("got %q, want fallbackMessage", result.Content)
	}
}

func TestAskUserToolValidation(t *testing.T) {
	r := &Runner{} // no AskUserApprover
	ref := &RunnerRef{R: r}
	tool := NewAskUserTool(ref)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty question", `{"question":"","header":"H","options":[{"label":"A","description":"d"}]}`, "question is required"},
		{"empty header", `{"question":"Q?","header":"","options":[{"label":"A","description":"d"}]}`, "header is required"},
		{"too few options", `{"question":"Q?","header":"H","options":[{"label":"A","description":"d"}]}`, "at least 2 options"},
		{"too many options", `{"question":"Q?","header":"H","options":[{"label":"A","description":"d"},{"label":"B","description":"d"},{"label":"C","description":"d"},{"label":"D","description":"d"},{"label":"E","description":"d"}]}`, "at most 4 options"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(tt.input))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want containing %q", err.Error(), tt.want)
			}
		})
	}
}

func TestAskUserToolWithApprover(t *testing.T) {
	r := &Runner{
		AskUserApprover: &stubAskUserApprover{
			answer: AskUserAnswer{
				Selected: []string{"Option B"},
			},
		},
	}
	ref := &RunnerRef{R: r}
	tool := NewAskUserTool(ref)

	result, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(`{
		"question": "Which approach?",
		"header": "Approach",
		"options": [
			{"label": "Option A", "description": "First way"},
			{"label": "Option B", "description": "Second way"}
		]
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Option B") {
		t.Errorf("result should mention the selected option, got: %s", result.Content)
	}
}

func TestAskUserToolWithNotes(t *testing.T) {
	r := &Runner{
		AskUserApprover: &stubAskUserApprover{
			answer: AskUserAnswer{
				Selected: []string{"Other"},
				Notes:    "Use Redis for caching",
			},
		},
	}
	ref := &RunnerRef{R: r}
	tool := NewAskUserTool(ref)

	result, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(`{
		"question": "Which caching?",
		"header": "Cache",
		"options": [
			{"label": "Option A", "description": "Memory"},
			{"label": "Other", "description": "Type your own"}
		],
		"allowCustom": true
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Redis for caching") {
		t.Errorf("result should contain notes, got: %s", result.Content)
	}
}

func TestAskUserToolMultiSelect(t *testing.T) {
	r := &Runner{
		AskUserApprover: &stubAskUserApprover{
			answer: AskUserAnswer{
				Selected: []string{"Option A", "Option C"},
			},
		},
	}
	ref := &RunnerRef{R: r}
	tool := NewAskUserTool(ref)

	result, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(`{
		"question": "Which features?",
		"header": "Features",
		"multiSelect": true,
		"options": [
			{"label": "Option A", "description": "First"},
			{"label": "Option B", "description": "Second"},
			{"label": "Option C", "description": "Third"}
		]
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Option A") || !strings.Contains(result.Content, "Option C") {
		t.Errorf("result should mention both selected options, got: %s", result.Content)
	}
}

func TestAskUserToolAutoModeFallback(t *testing.T) {
	// An auto-mode enabled approver should cause fallback.
	r := &Runner{
		Approver: &stubAutoModeApprover{enabled: true},
	}
	// Set AskUserApprover non-nil to prove the auto check fires first.
	r.AskUserApprover = &stubAskUserApprover{
		answer: AskUserAnswer{Selected: []string{"B"}},
	}
	ref := &RunnerRef{R: r}
	tool := NewAskUserTool(ref)

	result, err := tool.Execute(nil, tools.ExecutionContext{}, json.RawMessage(`{
		"question": "Q?",
		"header": "H",
		"options": [
			{"label": "A", "description": "d"},
			{"label": "B", "description": "d"}
		]
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != fallbackMessage {
		t.Errorf("got %q, want fallbackMessage in auto mode", result.Content)
	}
}

func TestNewAskUserID(t *testing.T) {
	id := newAskUserID()
	if len(id) == 0 {
		t.Error("id should not be empty")
	}
	if !strings.HasPrefix(id, "ask_") {
		t.Errorf("id should start with 'ask_', got %q", id)
	}
	// Uniqueness check.
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id2 := newAskUserID()
		if ids[id2] {
			t.Errorf("duplicate id: %s", id2)
		}
		ids[id2] = true
	}
}

func TestFormatAnswer(t *testing.T) {
	tests := []struct {
		name   string
		answer AskUserAnswer
		want   []string
	}{
		{"single selection", AskUserAnswer{Selected: []string{"A"}}, []string{"A"}},
		{"with notes", AskUserAnswer{Selected: []string{"A"}, Notes: "note"}, []string{"A", "note"}},
		{"skipped", AskUserAnswer{}, []string{"best judgment"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAnswer(tt.answer)
			for _, w := range tt.want {
				if !strings.Contains(strings.ToLower(got), strings.ToLower(w)) {
					t.Errorf("formatAnswer(%v) = %q, want containing %q", tt.answer, got, w)
				}
			}
		})
	}
}

// --- test helpers -----------------------------------------------------------

type stubAskUserApprover struct {
	answer AskUserAnswer
	err    error
}

func (s *stubAskUserApprover) AskUser(q AskUserQuestion) (AskUserAnswer, error) {
	return s.answer, s.err
}

var _ AskUserApprover = (*stubAskUserApprover)(nil)

type stubAutoModeApprover struct {
	enabled bool
}

func (s *stubAutoModeApprover) Approve(string, json.RawMessage) Verdict { return VerdictDeny }
func (s *stubAutoModeApprover) IsAutoModeEnabled() bool                 { return s.enabled }

var _ AutoModeDetector = (*stubAutoModeApprover)(nil)
