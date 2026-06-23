package goal

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/model"
)

// recordingProvider captures the request so a test can inspect what the judge saw.
type recordingProvider struct {
	got     model.Request
	content string
}

func (p *recordingProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.got = req
	return model.Response{Content: p.content}, nil
}

func userMsg(req model.Request) string {
	for _, m := range req.Messages {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}

// The full diff must reach the judge so it can catch test-file edits the worker
// never surfaced (anti-gaming, §9.3).
func TestLLMCheckerIncludesDiff(t *testing.T) {
	rp := &recordingProvider{content: `{"met":false,"blocked":false,"reason":"x"}`}
	c := &LLMChecker{Provider: rp}
	g := &Goal{Objective: "make tests pass, don't edit tests", diff: "DIFF: edited auth_test.go"}

	if _, err := c.Check(context.Background(), g, fakeTrans{ev: "evidence"}); err != nil {
		t.Fatal(err)
	}
	if u := userMsg(rp.got); !strings.Contains(u, "DIFF: edited auth_test.go") {
		t.Errorf("judge request omitted the git diff:\n%s", u)
	}
}

// With no diff captured, the diff section is absent (no empty header noise).
func TestLLMCheckerNoDiffSection(t *testing.T) {
	rp := &recordingProvider{content: `{"met":true,"blocked":false,"reason":"ok"}`}
	c := &LLMChecker{Provider: rp}
	if _, err := c.Check(context.Background(), &Goal{Objective: "x"}, fakeTrans{}); err != nil {
		t.Fatal(err)
	}
	if u := userMsg(rp.got); strings.Contains(u, "git diff") {
		t.Errorf("expected no diff section when diff is empty:\n%s", u)
	}
}
