package conversation

import (
	"context"
	"testing"

	"code-agent/internal/model"
)

// titledProvider records the request the title generator sends.
type titledProvider struct {
	lastReq model.Request
	reply   string
}

func (p *titledProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.lastReq = req
	return model.Response{Content: p.reply}, nil
}

// TestTitleGeneratorPropagatesSessionID pins the same OpenCode Go regression the
// compactor covers: title generation is a direct provider call that bypasses the
// agent loop's request stamping, so it must carry the session id itself or
// x-opencode-session goes out empty and the call is rejected with
// 400 MissingSessionID.
func TestTitleGeneratorPropagatesSessionID(t *testing.T) {
	p := &titledProvider{reply: "Fix opencode routing"}
	g := NewLLMTitleGenerator(p, "m")

	if _, err := g.GenerateTitle(context.Background(), "sess-42", "hi", "hello"); err != nil {
		t.Fatal(err)
	}
	if p.lastReq.SessionID != "sess-42" {
		t.Fatalf("title request session id = %q, want %q", p.lastReq.SessionID, "sess-42")
	}
}
