package session

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrContinuationStale = errors.New("continuation no longer owns this work")

// Continuation carries no credentials. Binding and Checkpoint are owned by the
// executor and runner respectively. Storage arbitrates identity/revision and
// retains the checkpoint's terminal accounting projection for an idle stop.
type Continuation struct {
	SessionID, TurnID   string
	Revision            int64
	State               string
	Await               []string
	Binding, Checkpoint json.RawMessage
}

type ContinuationStore interface {
	AdmitContinuationWork(context.Context, string, string) error
	ParkContinuation(context.Context, *Session, Continuation) (Continuation, error)
	Continuation(context.Context, string) (Continuation, error)
	ClaimContinuation(context.Context, Continuation) (bool, error)
	EndContinuation(context.Context, string, string, string) (bool, error)
}

type ContinuationProvider interface{ ContinuationStore() ContinuationStore }
