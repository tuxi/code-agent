package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type auditErrorProvider struct{}

func (auditErrorProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, errors.New("fixture failure")
}

func TestModelCallAuditPersistsIdentityAndDoesNotVerifyFailure(t *testing.T) {
	requested := "corp/vendor/model"
	for _, success := range []bool{true, false} {
		selection := model.Selection{RequestedModel: &requested, ResolvedModel: requested, Provider: "corp", ConfigurationID: t.TempDir()}
		var p model.Provider = &scriptedProvider{responses: []model.Response{{Content: "done", ActualModel: "served-revision", Usage: model.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}}}}
		if !success {
			p = auditErrorProvider{}
		}
		em := &capturingEmitter{}
		runner := &Runner{Model: p, ModelName: "wire-alias", ModelSelection: selection, Tools: tools.NewRegistry(), Emitter: em, MaxSteps: 1}
		sess := newSession()
		_, err := runner.RunTurn(context.Background(), sess, "audit")
		if (err == nil) != success {
			t.Fatalf("run error=%v", err)
		}
		e, ok := em.first(EventModelFinished)
		if !ok || e.ModelSelection == nil || e.ModelSucceeded == nil || *e.ModelSucceeded != success {
			t.Fatalf("event=%+v", e)
		}
		_, verified := model.RecentVerification(selection.ConfigurationID, time.Now())
		if verified != success {
			t.Fatalf("verified=%v success=%v", verified, success)
		}
		if success {
			if e.ModelSelection.ActualModel != "served-revision" || e.PromptTokens != 12 || e.TotalTokens != 15 {
				t.Fatalf("call evidence=%+v", e)
			}
			data, err := json.Marshal(sess.Metadata["last_model_call"])
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Selection model.Selection `json:"model_selection"`
				Usage     model.Usage     `json:"usage"`
			}
			if err = json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Selection.RequestedModel == nil || *saved.Selection.RequestedModel != requested || saved.Selection.ActualModel != "served-revision" || saved.Usage.TotalTokens != 15 {
				t.Fatalf("saved=%s", data)
			}
		}
	}
}
