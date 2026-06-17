package reflection

import "strings"

// Nudge renders the ephemeral self-check message for a turn, surfacing only the
// signals that actually fired, as questions grounded in the real work. It
// returns "" when there is nothing to ask (Concerns() == false), so the loop can
// accept the final answer with no extra model call.
//
// The output is appended to the next request only and never persisted — exactly
// like the loop's convergence nudge. The model decides what to do with it; this
// method neither retries nor judges.
func (c ReflectionContext) Nudge() string {
	if !c.Concerns() {
		return ""
	}

	var b strings.Builder
	b.WriteString("[reflection] Before you finish, check your work against what actually happened this turn:\n")

	if c.TestEditedAfterFailure {
		b.WriteString("- You edited a test file (")
		b.WriteString(strings.Join(c.TestFilesMutated, ", "))
		b.WriteString(") after a test failed. Did you fix the root cause, or change the test to accept a wrong value? If the test was correct, fix the source instead.\n")
	}
	if c.UnverifiedMutation {
		b.WriteString("- You changed ")
		b.WriteString(strings.Join(c.MutatedFiles, ", "))
		b.WriteString(" but no build or test has confirmed it since. Run a verification before claiming done.\n")
	}

	b.WriteString("If everything genuinely checks out, give your final answer now. Otherwise, fix it.")
	return b.String()
}
