package main

import (
	"errors"
	"testing"
	"time"

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

// TestParseObjective pins the budget-flag grammar: leading --turns/--tokens/--wall
// are stripped into the budget; the first non-flag (or a bad value) ends parsing.
func TestParseObjective(t *testing.T) {
	cases := []struct {
		in         string
		wantObj    string
		wantTurns  int
		wantTokens int
		wantWall   time.Duration
	}{
		{"make tests pass", "make tests pass", 0, 0, 0},
		{"--turns 20 make tests pass", "make tests pass", 20, 0, 0},
		{"--turns 5 --tokens 1000 fix the bug", "fix the bug", 5, 1000, 0},
		{"--wall 30m clean up", "clean up", 0, 0, 30 * time.Minute},
		{"--turns nope do x", "--turns nope do x", 0, 0, 0}, // bad value → not consumed
		{"--turns 5", "", 5, 0, 0},                          // only flags → empty objective
	}
	for _, c := range cases {
		obj, b := parseObjective(c.in)
		if obj != c.wantObj || b.MaxTurns != c.wantTurns || b.MaxTokens != c.wantTokens || b.MaxWall != c.wantWall {
			t.Errorf("parseObjective(%q) = (%q, %+v); want obj=%q turns=%d tokens=%d wall=%s",
				c.in, obj, b, c.wantObj, c.wantTurns, c.wantTokens, c.wantWall)
		}
	}
}
