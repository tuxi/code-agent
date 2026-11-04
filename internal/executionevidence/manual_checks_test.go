package executionevidence

import (
	"slices"
	"testing"
)

func TestManualChecksCannotBeSatisfiedByCommandsOrProse(t *testing.T) {
	e := expected
	e.ManualChecks = []string{"User checks iPhone landscape"}
	events := append(validated(), Event{Kind: "turn_finished", Observation: "The user verified the UI"})
	for _, pending := range []bool{false, true} {
		r := Check(events, e, "completed", pending)
		want := "needs_validation"
		if pending {
			want = "attention_required"
		}
		if r.Status != want || len(r.MissingValidationCommands) != 0 || !slices.Contains(r.Reasons, "manual_checks_unverified") {
			t.Fatal(r)
		}
	}
}
