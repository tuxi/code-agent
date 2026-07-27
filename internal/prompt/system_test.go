package prompt

import (
	"strings"
	"testing"
)

func TestAgentStoppingPolicyIsPhaseAware(t *testing.T) {
	for _, required := range []string{
		"Answering (ordinary questions",
		"Planning (after enter_plan_mode)",
		"Readiness-complete",
		"Executing (an approved plan",
		"Reviewing (plan_critic or change_review)",
		"VERDICT: REQUEST_CHANGES",
	} {
		if !strings.Contains(AgentSystemPrompt, required) {
			t.Fatalf("system prompt missing phase-aware stopping rule %q", required)
		}
	}
	for _, obsolete := range []string{
		"bias STRONGLY toward answering",
		"One result that answers the question is enough",
		"A direct answer at reasonable confidence beats exhaustive verification",
		"Never repeat a tool call you have already made",
	} {
		if strings.Contains(AgentSystemPrompt, obsolete) {
			t.Fatalf("system prompt still contains obsolete global convergence rule %q", obsolete)
		}
	}
}

func TestSubAgentStoppingPolicyDistinguishesReviewFromInvestigation(t *testing.T) {
	for _, required := range []string{
		"ordinary investigation",
		"plan_critic",
		"change_review",
		"do not stop at the first plausible result",
		"before choosing a verdict",
	} {
		if !strings.Contains(SubAgentSystemPrompt, required) {
			t.Fatalf("subagent prompt missing review-aware rule %q", required)
		}
	}
}
