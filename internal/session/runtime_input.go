package session

import (
	"context"
	"errors"
	"time"
)

var ErrRuntimeInputClosed = errors.New("runtime input boundary is closed")

// RuntimeInput is trusted routing metadata around untrusted result data. It is
// not a user submission, permission grant or instruction from another session.
type RuntimeInput struct {
	ID, SessionID, SourceTurnID, SourceCallID, Kind, Content string
	CreatedAt                                                time.Time
}

type RuntimeInputBoundary struct {
	TurnID   string
	Receipts []string
	Deliver  bool
	Finalize bool
}

// RuntimeInputStore is optional. Implementations atomically save model history
// and delivery receipts; producers cannot depend on event fan-out for delivery.
type RuntimeInputStore interface {
	RecordRuntimeInput(context.Context, RuntimeInput, []EventRecord) (seqs []int64, created bool, err error)
	CheckpointRuntimeInputs(context.Context, *Session, RuntimeInputBoundary) ([]string, error)
}

// RuntimeInputProvider does not enlarge the mandatory repository/store port.
type RuntimeInputProvider interface{ RuntimeInputStore() RuntimeInputStore }

func JobCompletionInputID(jobID string) string { return "job:" + jobID + ":finished" }
