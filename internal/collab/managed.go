package collab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

var (
	ErrManagedWorkspace         = boundaryError{"managed_workspace", "workspace is owned by a managed task"}
	ErrRecoveryRequired         = boundaryError{"recovery_required", "managed execution requires recovery reconciliation"}
	ErrUnavailable              = boundaryError{"managed_unavailable", "managed task capability is unavailable"}
	ErrExecutorModelUnavailable = boundaryError{"executor_model_unavailable", "selected executor model is unavailable"}
)

type boundaryError struct{ code, message string }

func (e boundaryError) Error() string               { return e.message }
func (e boundaryError) AgentInputErrorCode() string { return e.code }
func (e boundaryError) LifecycleErrorCode() string  { return e.code }
func (e boundaryError) SafeMessage() string         { return e.message }
func (e boundaryError) Unwrap() error               { return ErrConflict }

// ManagedTurnRef is host routing data, never accepted from Agent Wire input.
// It names a durable reservation, not permission to execute arbitrary messages.
type ManagedTurnRef struct {
	ServerID   string `json:"server_id"`
	StoreID    string `json:"store_id"`
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id"`
	Epoch      string `json:"control_epoch"`
	DispatchID string `json:"dispatch_id"`
}

type WorkflowBinding struct {
	Name      string `json:"name"`
	VersionID string `json:"version_id"`
	Hash      string `json:"hash"`
}

type ManagedRun struct {
	ExecutorModel           string           `json:"executor_model,omitempty"`
	RunID                   string           `json:"run_id"`
	TaskID                  string           `json:"task_id"`
	ServerID                string           `json:"server_id"`
	StoreID                 string           `json:"store_id"`
	Epoch                   string           `json:"control_epoch"`
	State                   string           `json:"state"`
	Workspace               WorkspaceAnchor  `json:"workspace"`
	PlanArtifactID          string           `json:"plan_artifact_id"`
	PlanVersion             string           `json:"plan_version"`
	PlanHash                string           `json:"plan_hash"`
	ExecutorParticipationID string           `json:"executor_participation_id"`
	ReviewerParticipationID string           `json:"reviewer_participation_id"`
	MaxIterations           int              `json:"max_iterations"`
	Workflow                WorkflowBinding  `json:"workflow"`
	CommandID               string           `json:"command_id"`
	FluxTaskID              string           `json:"flux_task_id,omitempty"`
	ReviewPolicy            *ReviewPolicy    `json:"review_policy,omitempty"`
	Baseline                *ArtifactBinding `json:"baseline,omitempty"`
	CurrentIteration        int              `json:"current_iteration,omitempty"`
	CurrentBundle           *ArtifactBinding `json:"current_bundle,omitempty"`
	RecoveryReason          string           `json:"recovery_reason,omitempty"`
}

type PrepareRunInput struct {
	ExecutorModel           string `json:"executor_model,omitempty"`
	RequestID               string
	PlanArtifactID          string
	ExecutorParticipationID string
	ReviewerParticipationID string
	MaxIterations           int
	Workflow                WorkflowBinding
	ReviewPolicy            *ReviewPolicy
}

type ManagedCommand struct {
	CommandID   string          `json:"command_id"`
	StoreID     string          `json:"source_store_id"`
	Kind        string          `json:"kind"`
	RunID       string          `json:"run_id"`
	Payload     json.RawMessage `json:"payload"`
	PayloadHash string          `json:"payload_hash"`
}

func PayloadHash(payload []byte) string {
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

type SubmissionReceipt struct {
	StoreID     string          `json:"source_store_id"`
	CommandID   string          `json:"command_id"`
	PayloadHash string          `json:"payload_hash"`
	Workflow    WorkflowBinding `json:"workflow"`
	FluxTaskID  string          `json:"flux_task_id"`
}

type ReserveDispatchInput struct {
	RequestID      string
	Epoch          string
	Iteration      int
	Message        string
	Model          string
	GateDecisionID string
}

type ManagedDispatch struct {
	Ref                   ManagedTurnRef `json:"source"`
	ParticipationID       string         `json:"participation_id"`
	SessionID             string         `json:"session_id"`
	RequestID             string         `json:"request_id"`
	Iteration             int            `json:"iteration"`
	Message               string         `json:"message"`
	Model                 string         `json:"model,omitempty"`
	TurnID                string         `json:"turn_id,omitempty"`
	AttemptID             string         `json:"attempt_id,omitempty"`
	ExpectedWorkspaceHash string         `json:"expected_workspace_hash,omitempty"`
}

// ManagedRepository is an internal optional port. B2-a does not expose it as
// an HTTP/MCP capability: gates and restart reconciliation are not wired yet.
type ManagedRepository interface {
	PrepareRun(context.Context, Principal, string, PrepareRunInput) (Receipt, error)
	Run(context.Context, Principal, string) (ManagedRun, error)
	PendingCommands(context.Context, Principal, int) ([]ManagedCommand, error)
	AcknowledgeSubmission(context.Context, Principal, SubmissionReceipt) error
	ReserveDispatch(context.Context, Principal, string, ReserveDispatchInput) (Receipt, error)
	Dispatch(context.Context, Principal, ManagedTurnRef) (ManagedDispatch, error)
}

// ExecutionGuard holds a workspace admission claim for the entire turn. It is
// optional so independent Repository implementations remain source compatible.
type ExecutionGuard interface {
	AcquireTaskExecution(context.Context, string, string, string, *ManagedTurnRef) (func(), error)
	RequireTaskRecovery(context.Context, string, string) error
}
