package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/session"
	"time"
)

func modelCallSucceeded(resp model.Response, err error) *bool {
	ok := err == nil && resp.ValidateAssistantTurn() == nil
	return &ok
}

func (r *Runner) selectionForResponse(resp model.Response) *model.Selection {
	s := r.ModelSelection
	s.ActualModel = resp.ActualModel
	return &s
}

func observeModelVerification(e Event) {
	if e.Kind == EventModelFinished && e.ModelSucceeded != nil && *e.ModelSucceeded && e.ModelSelection != nil {
		model.RecordVerification(*e.ModelSelection, time.Now())
	}
}

// Session state is a last-call summary; per-turn/invocation events retain the
// full history and usage for attempts without introducing a Task schema.
func (r *Runner) recordModelCall(sess *session.Session, resp model.Response) {
	if sess.Metadata == nil {
		sess.Metadata = make(map[string]any)
	}
	sess.Metadata["last_model_call"] = struct {
		Selection *model.Selection `json:"model_selection"`
		Usage     model.Usage      `json:"usage"`
	}{r.selectionForResponse(resp), resp.AccountingUsage()}
}
