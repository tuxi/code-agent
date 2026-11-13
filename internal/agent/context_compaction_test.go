package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"context"
	"errors"
	"strings"
	"testing"
)

type contextLimitProvider struct {
	calls      int
	alwaysFail bool
}

func (p *contextLimitProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.calls++
	if req.MaxOutputTokens <= 0 {
		panic("missing output budget")
	}
	if p.calls == 1 || p.alwaysFail {
		return model.Response{}, &model.APIError{StatusCode: 400, Message: "This model's maximum context length is 1048576 tokens"}
	}
	return model.Response{Content: "recovered", Usage: model.Usage{PromptTokens: 100}}, nil
}
func TestContextLimitRecoveryIsBounded(t *testing.T) {
	for _, fail := range []bool{false, true} {
		p := &contextLimitProvider{alwaysFail: fail}
		r := &Runner{Model: p, Tools: tools.NewRegistry(), MaxSteps: 1, Compactor: shrinkCompactor{}}
		s := overBudgetSession()
		s.PromptTokens = 1
		_, err := r.RunTurn(context.Background(), s, "continue")
		if (err != nil) != fail || p.calls != 2 {
			t.Fatalf("fail=%v calls=%d err=%v", fail, p.calls, err)
		}
	}
}
func TestContextPressureBypassesCooldown(t *testing.T) {
	s := overBudgetSession()
	s.ContextWindow = 1000000
	s.CompactThreshold = 750000
	s.PromptTokens = 957379
	s.RecordCompaction(959505, 10)
	s.FinalizeCompaction(910301)
	c := &countingCompactor{}
	r := &Runner{Compactor: c}
	if err := r.maybeCompact(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Fatal("near-full context was suppressed by cooldown")
	}
}
func TestContextPressureCountsReasoning(t *testing.T) {
	s := &session.Session{ContextWindow: 1000, Messages: []model.Message{{ReasoningContent: string(make([]byte, 3000))}}}
	if !contextPressure(s) {
		t.Fatal("reasoning missing from pressure estimate")
	}
}

type failingContextCompactor struct{}

func (failingContextCompactor) Compact(context.Context, *session.Session) error {
	return errors.New("summary provider unavailable")
}
func TestFailedCompactionRestoresPreparedContext(t *testing.T) {
	s := overBudgetSession()
	s.Messages = []model.Message{{Role: model.RoleSystem, Content: "sys"}, {Role: model.RoleTool, Content: strings.Repeat("old output", 1000)}, {Role: model.RoleAssistant, Content: "older"}, {Role: model.RoleUser, Content: "recent"}, {Role: model.RoleAssistant, Content: "latest"}}
	original := s.Messages[1].Content
	r := &Runner{Compactor: failingContextCompactor{}, ContextEditor: &session.ContextEditor{KeepTurns: 1}}
	if err := r.maybeCompact(context.Background(), s); err == nil {
		t.Fatal("summary failure ignored")
	}
	if s.Messages[1].Content != original {
		t.Fatal("summary failure persisted cleared tool result")
	}
}
