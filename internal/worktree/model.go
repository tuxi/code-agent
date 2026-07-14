package worktree

import (
	"context"
	"errors"
	"time"
)

type State string

const (
	StateReserved     State = "reserved"
	StateProvisioning State = "provisioning"
	StateReady        State = "ready"
	StateFailed       State = "failed"
	StateMissing      State = "missing"
	StateRemoving     State = "removing"
	StateRemoveFailed State = "remove_failed"
	StateRetained     State = "retained"
	StateRemoved      State = "removed"
)

type BaseRef string

const (
	BaseRefHead  BaseRef = "head"
	BaseRefFresh BaseRef = "fresh"
)

// Record is the durable reservation and lifecycle of one Runtime-managed Git
// worktree. It exists before the conversation row so provisioning can recover
// after a crash between git worktree add and session persistence.
type Record struct {
	SessionID           string
	ClientRequestID     string
	BaseWorkspaceID     string
	SourceWorkspaceID   string
	CheckoutWorkspaceID string
	SourceWorkspacePath string
	WorktreePath        string
	Name                string
	Branch              string
	BaseRef             BaseRef
	BaseCommit          string
	State               State
	LastErrorCode       string
	LastErrorMessage    string
	RemoveRequestID     string
	RemoveForce         bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

var ErrNotFound = errors.New("managed worktree reservation not found")

// Store is an optional persistence extension. Runtime capability must remain
// disabled when the configured session backend does not implement it.
type Store interface {
	// Reserve atomically inserts by ClientRequestID. A duplicate returns the
	// original record and created=false, including across process restarts.
	ReserveWorktree(ctx context.Context, record Record) (stored Record, created bool, err error)
	WorktreeByClientRequestID(ctx context.Context, requestID string) (Record, error)
	WorktreeBySessionID(ctx context.Context, sessionID string) (Record, error)
	ListWorktrees(ctx context.Context) ([]Record, error)
	UpdateWorktree(ctx context.Context, record Record) error
}
