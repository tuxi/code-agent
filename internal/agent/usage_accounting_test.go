package agent

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

type usageStreamProvider struct {
	model.Provider
	response model.Response
}

func (p usageStreamProvider) CompleteStream(context.Context, model.Request, func(string), func(string)) (model.Response, error) {
	return p.response, nil
}

func TestFailedModelCallRetainsUsageWithoutExecutingTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream_fallback"}[stream], func(t *testing.T) {
			// Every response has malformed JSON. None may enter history or tools.
			bad := model.Response{ActualModel: "served", FinishReason: "length", Usage: model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, BillingUnits: 5}, ToolCalls: []model.ToolCall{{ID: "bad", Type: "function", Function: model.FunctionCall{Name: "danger", Arguments: `{"path":`}}}}
			inner := &scriptedProvider{responses: []model.Response{bad, bad}}
			var native model.Provider = inner
			attempts := 2
			if stream {
				native = usageStreamProvider{Provider: inner, response: bad}
				attempts++
			}
			provider := &model.ResilientProvider{Inner: native, MaxRetries: 1, LogWriter: io.Discard}
			em := &capturingEmitter{}
			registry := tools.NewRegistry()
			danger := &recordingTool{}
			if err := registry.Register(danger); err != nil {
				t.Fatal(err)
			}
			r := &Runner{Model: provider, Stream: stream, Tools: registry, Emitter: em, MaxSteps: 1}
			sess := newSession()
			turn, err := r.RunTurn(context.Background(), sess, "work")
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			e, ok := em.first(EventModelFinished)
			if !ok || e.ModelSucceeded == nil || *e.ModelSucceeded || e.PromptTokens != attempts*100 || e.CompletionTokens != attempts*20 || e.TotalTokens != attempts*120 || e.BillingUnits != int64(attempts*5) {
				t.Fatalf("event=%+v", e)
			}
			if turn.TokensUsed != attempts*120 || turn.ModelBillingUnits != int64(attempts*5) || sess.PromptTokens == attempts*100 || danger.ran {
				t.Fatalf("turn=%+v session context=%d", turn, sess.PromptTokens)
			}
			for _, msg := range sess.Messages {
				if msg.Role == model.RoleAssistant {
					t.Fatal("malformed tool response entered history")
				}
			}
			data, _ := json.Marshal(sess.Metadata["last_model_call"])
			var saved struct {
				Usage model.Usage `json:"usage"`
			}
			if json.Unmarshal(data, &saved) != nil || saved.Usage.TotalTokens != attempts*120 {
				t.Fatalf("saved=%s", data)
			}
		})
	}
}

func TestRetryAccountingDoesNotInflateSessionContext(t *testing.T) {
	inner := &scriptedProvider{responses: []model.Response{
		{Usage: model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, FinishReason: "length"},
		{Content: "done", Usage: model.Usage{PromptTokens: 150, CompletionTokens: 30, TotalTokens: 180}},
	}}
	em := &capturingEmitter{}
	r := &Runner{Model: &model.ResilientProvider{Inner: inner, MaxRetries: 1, LogWriter: io.Discard}, Tools: tools.NewRegistry(), Emitter: em, MaxSteps: 1}
	sess := newSession()
	turn, err := r.RunTurn(context.Background(), sess, "work")
	if err != nil || turn.TokensUsed != 300 || sess.PromptTokens != 150 || turn.PromptTokens != 150 {
		t.Fatalf("turn=%+v context=%d err=%v", turn, sess.PromptTokens, err)
	}
	e, _ := em.first(EventModelFinished)
	if e.PromptTokens != 250 || e.TotalTokens != 300 {
		t.Fatalf("event=%+v", e)
	}
}
