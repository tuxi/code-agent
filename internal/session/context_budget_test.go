package session

import (
	"code-agent/internal/model"
	"context"
	"errors"
	"strings"
	"testing"
)

type boundedSummaryProvider struct {
	calls  int
	failAt int
	window int
}

func (p *boundedSummaryProvider) Complete(_ context.Context, r model.Request) (model.Response, error) {
	p.calls++
	size := r.MaxOutputTokens
	for _, m := range r.Messages {
		size += len(m.Content)
	}
	if size > p.window {
		return model.Response{}, errors.New("summary exceeded window")
	}
	if p.calls == p.failAt {
		return model.Response{}, errors.New("summary failed")
	}
	return model.Response{Content: "cumulative digest"}, nil
}
func TestSummaryChunksBoundedAndAtomic(t *testing.T) {
	for _, failAt := range []int{0, 2} {
		p := &boundedSummaryProvider{window: 12000, failAt: failAt}
		c := &LLMCompactor{Provider: p, KeepRecentTokens: 1}
		old := strings.Repeat("中文 tool output", 4000)
		s := &Session{ContextWindow: p.window, Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}, {Role: model.RoleUser, Content: old}, {Role: model.RoleUser, Content: "latest"}}}
		err := c.Compact(context.Background(), s)
		if failAt > 0 {
			if err == nil || s.Summary != "" || s.Messages[1].Content != old {
				t.Fatal("failed summary mutated original history")
			}
		} else if err != nil || p.calls < 2 || s.Summary == "" {
			t.Fatalf("calls=%d err=%v", p.calls, err)
		}
	}
}
