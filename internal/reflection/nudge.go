package reflection

import "strings"

// Nudge renders the ephemeral self-check message for a turn. Since P4.3-R Move 2
// it surfaces only the ONE remaining fact-question — a test file edited after a
// failure ("did you fix the cause or the test?") — grounded in the real work and
// phrased so the model judges. It returns "" when there is nothing to ask.
//
// The retired UnverifiedMutation signal no longer produces a nudge here: the
// runtime does not GUESS that a change is unverified. That case is handled by the
// loop's deterministic finalize verify (Move 2, option 2a) when a VerifyCommand
// is configured, and is silent otherwise (2b) — see the loop's finalize boundary.
//
// The output is appended to the next request only and never persisted — exactly
// like the loop's convergence nudge. The model decides what to do with it; this
// method neither retries nor judges.
func (c ReflectionContext) Nudge() string {
	if !c.TestEditedAfterFailure {
		return ""
	}

	var b strings.Builder
	b.WriteString("[reflection] Before you finish, check your work against what actually happened this turn:\n")
	b.WriteString("- You edited a test file (")
	b.WriteString(strings.Join(c.TestFilesMutated, ", "))
	b.WriteString(") after a test failed. Did you fix the root cause, or change the test to accept a wrong value? If the test was correct, fix the source instead.\n")
	b.WriteString("If everything genuinely checks out, give your final answer now. Otherwise, fix it.")
	return b.String()
}
