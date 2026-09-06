package model

import "testing"

// The reserved "off" effort translates to each adapter's disable parameter;
// other levels forward verbatim; unset stays provider-default.
func TestReasoningEffortOffAdapterMapping(t *testing.T) {
	t.Run("ollama think=false", func(t *testing.T) {
		if got := reasoningEffortOrDefault(ReasoningEffortOff, "low"); got != false {
			t.Errorf("Complete-path think = %v, want false", got)
		}
		if got := reasoningEffortOrDefault(ReasoningEffortOff, false); got != false {
			t.Errorf("stream-path think = %v, want false", got)
		}
		if got := reasoningEffortOrDefault("", "low"); got != "low" {
			t.Errorf("unset effort = %v, want legacy fallback low", got)
		}
		if got := reasoningEffortOrDefault("high", nil); got != "high" {
			t.Errorf("level effort = %v, want verbatim high", got)
		}
	})
	t.Run("openai-compatible reasoning_effort none", func(t *testing.T) {
		if got := reasoningEffortToOpenAI(ReasoningEffortOff); got != "none" {
			t.Errorf("off = %q, want none", got)
		}
		if got := reasoningEffortToOpenAI("x-high"); got != "x-high" {
			t.Errorf("level = %q, want verbatim", got)
		}
		if got := reasoningEffortToOpenAI(""); got != "" {
			t.Errorf("unset = %q, want empty (omitted)", got)
		}
	})
	t.Run("responses reasoning.effort none", func(t *testing.T) {
		if got := reasoningEffortToResponses(ReasoningEffortOff); got == nil || got.Effort != "none" {
			t.Errorf("off = %+v, want effort none", got)
		}
		if got := reasoningEffortToResponses(""); got != nil {
			t.Errorf("unset = %+v, want nil", got)
		}
		if got := reasoningEffortToResponses("max"); got == nil || got.Effort != "max" {
			t.Errorf("level = %+v, want verbatim max", got)
		}
	})
}
