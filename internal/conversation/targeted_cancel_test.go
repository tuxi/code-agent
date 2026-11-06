package conversation

import (
	"context"
	"testing"
)

func TestActiveTurnTargetCannotCancelReplacement(t *testing.T) {
	r := NewActiveTurnRegistry()
	_, oldCancel, err := r.BeginTurnWithID("session", "old", context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldCancel()
	r.FinishTurn("session")
	current, cancel, err := r.BeginTurnWithID("session", "new", context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer r.FinishTurn("session")
	if r.CancelTarget("session", "old") || current.Err() != nil {
		t.Fatal("late stop cancelled replacement")
	}
	if !r.CancelTarget("session", "new") || current.Err() != context.Canceled {
		t.Fatal("current target was not cancelled")
	}
}
