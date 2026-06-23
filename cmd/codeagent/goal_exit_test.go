package main

import (
	"errors"
	"testing"

	"code-agent/internal/goal"
)

// TestGoalExitError pins the headless exit-code contract CI branches on:
// achieved=0, budget=2, blocked=3, errored=4, paused=5, and that the error
// satisfies the ExitCoder interface main() detects.
func TestGoalExitError(t *testing.T) {
	cases := []struct {
		status goal.Status
		want   int
	}{
		{goal.StatusAchieved, 0},
		{goal.StatusBudgetLimited, 2},
		{goal.StatusBlocked, 3},
		{goal.StatusErrored, 4},
		{goal.StatusPaused, 5},
	}
	for _, c := range cases {
		err := goalExitError(&goal.Goal{Status: c.status})
		if c.want == 0 {
			if err != nil {
				t.Errorf("%s: want nil (exit 0), got %v", c.status, err)
			}
			continue
		}
		var ec interface{ ExitCode() int }
		if !errors.As(err, &ec) {
			t.Fatalf("%s: want an ExitCoder, got %v", c.status, err)
		}
		if ec.ExitCode() != c.want {
			t.Errorf("%s: exit code = %d, want %d", c.status, ec.ExitCode(), c.want)
		}
	}
	// A missing terminal state is a non-zero failure, not a silent success.
	if err := goalExitError(nil); err == nil {
		t.Error("nil goal: want a non-nil exit error")
	}
}
