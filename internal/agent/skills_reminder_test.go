package agent

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

func msgsContain(msgs []model.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

func TestSkillsReminderInjectedAndEphemeral(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, RemindSkills: true}

	sess := newSession()
	if _, err := runner.RunTurn(context.Background(), sess, "fix the failing test"); err != nil {
		t.Fatal(err)
	}

	// Sent to the model on the first call...
	if !msgsContain(provider.lastMessages, skillsReminder) {
		t.Error("skills reminder was not injected on the first model call")
	}
	// ...but never persisted to history (ephemeral, like the other nudges).
	if msgsContain(sess.Messages, skillsReminder) {
		t.Error("skills reminder must be ephemeral, not persisted to the session")
	}
}

func TestSkillsReminderOmittedWhenDisabled(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, RemindSkills: false}

	if _, err := runner.RunTurn(context.Background(), newSession(), "x"); err != nil {
		t.Fatal(err)
	}
	if msgsContain(provider.lastMessages, skillsReminder) {
		t.Error("skills reminder must not appear when RemindSkills is false")
	}
}

func TestParallelReminderInjectedAndEphemeral(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, RemindParallel: true}

	sess := newSession()
	if _, err := runner.RunTurn(context.Background(), sess, "research 5 unrelated topics"); err != nil {
		t.Fatal(err)
	}
	if !msgsContain(provider.lastMessages, parallelReminder) {
		t.Error("parallel reminder was not injected on the first model call")
	}
	if msgsContain(sess.Messages, parallelReminder) {
		t.Error("parallel reminder must be ephemeral, not persisted to the session")
	}
}

func TestParallelReminderOmittedWhenDisabled(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 3, RemindParallel: false}

	if _, err := runner.RunTurn(context.Background(), newSession(), "x"); err != nil {
		t.Fatal(err)
	}
	if msgsContain(provider.lastMessages, parallelReminder) {
		t.Error("parallel reminder must not appear when RemindParallel is false (max_parallel_tools<=1)")
	}
}
