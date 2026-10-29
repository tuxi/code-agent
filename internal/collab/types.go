// Package collab defines the B1 association/read model. It never starts an Agent.
package collab

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid task request")
	ErrNotFound  = errors.New("task resource not found")
	ErrConflict  = errors.New("task request conflicts with persisted data")
	ErrForbidden = errors.New("task operation is not permitted")
)

// Principal is host-authenticated data, never decoded from a request body.
type Principal struct {
	ID            string
	LocalSessions bool
}
type Identity struct {
	ServerID string `json:"server_id"`
	StoreID  string `json:"store_id"`
}
type SessionRef struct {
	Kind            string  `json:"kind"`
	ServerID        string  `json:"server_id,omitempty"`
	StoreID         string  `json:"store_id,omitempty"`
	SessionID       string  `json:"session_id,omitempty"`
	AgentID         string  `json:"agent_id,omitempty"`
	RuntimeKind     string  `json:"runtime_kind,omitempty"`
	SessionHandle   string  `json:"session_handle,omitempty"`
	NativeSessionID *string `json:"native_session_id,omitempty"`
}
type SourceRef struct {
	Kind                 string `json:"kind,omitempty"`
	ServerID             string `json:"server_id,omitempty"`
	StoreID              string `json:"store_id,omitempty"`
	SessionID            string `json:"session_id,omitempty"`
	TurnID               string `json:"turn_id,omitempty"`
	RequestID            string `json:"request_id,omitempty"`
	Seq                  string `json:"seq,omitempty"`
	ProducerSubmissionID string `json:"producer_submission_id,omitempty"`
}
type CodingTask struct {
	Schema           string      `json:"schema"`
	TaskID           string      `json:"task_id"`
	WorkspaceRef     string      `json:"workspace_ref"`
	Goal             string      `json:"goal"`
	Acceptance       []string    `json:"acceptance"`
	Mode             string      `json:"mode"`
	State            string      `json:"state"`
	Phase            string      `json:"phase"`
	WaitReason       string      `json:"wait_reason,omitempty"`
	Revision         string      `json:"revision"`
	SnapshotTaskSeq  string      `json:"snapshot_task_seq"`
	CurrentAttemptID string      `json:"current_attempt_id,omitempty"`
	WorkflowRef      *string     `json:"workflow_ref"`
	TaskAcceptance   string      `json:"task_acceptance"`
	ManagedRun       *ManagedRun `json:"managed_run,omitempty"`
}
type TaskParticipation struct {
	ParticipationID  string     `json:"participation_id"`
	TaskID           string     `json:"task_id"`
	AgentID          string     `json:"agent_id"`
	Role             string     `json:"role"`
	SessionRef       SessionRef `json:"session_ref"`
	ObservationLevel string     `json:"observation_level"`
	Capabilities     []string   `json:"capabilities"`
}
type TaskAttempt struct {
	AttemptID         string          `json:"attempt_id"`
	TaskID            string          `json:"task_id"`
	ParticipationID   string          `json:"participation_id"`
	Stage             string          `json:"stage"`
	Iteration         int             `json:"iteration"`
	Status            string          `json:"status"`
	SourceRef         SourceRef       `json:"source_ref"`
	ExecutionEvidence json.RawMessage `json:"execution_evidence"`
}
type TaskArtifactRef struct {
	ArtifactID  string          `json:"artifact_id"`
	AttemptID   string          `json:"attempt_id"`
	Kind        string          `json:"kind"`
	Version     string          `json:"version"`
	ContentHash *string         `json:"content_hash"`
	CaptureMode string          `json:"capture_mode"`
	SourceRef   SourceRef       `json:"source_ref"`
	Truncated   bool            `json:"truncated"`
	Content     json.RawMessage `json:"content,omitempty"`
}
type TaskEvent struct {
	SchemaVersion   int             `json:"schema_version"`
	EventID         string          `json:"event_id"`
	TaskID          string          `json:"task_id"`
	TaskSeq         string          `json:"task_seq"`
	TaskRevision    string          `json:"task_revision"`
	Kind            string          `json:"kind"`
	RecordedAt      time.Time       `json:"recorded_at"`
	ParticipationID string          `json:"participation_id,omitempty"`
	AttemptID       string          `json:"attempt_id,omitempty"`
	Source          SourceRef       `json:"source"`
	Payload         json.RawMessage `json:"payload"`
}
type Snapshot struct {
	CodingTask
	Participations   []TaskParticipation `json:"participations"`
	Attempts         []TaskAttempt       `json:"attempts"`
	ArtifactRefs     []TaskArtifactRef   `json:"artifact_refs"`
	PendingAttention []json.RawMessage   `json:"pending_attention"`
	AvailableActions []string            `json:"available_actions"`
	Usage            *json.RawMessage    `json:"usage"`
	CostUSD          *float64            `json:"cost_usd"`
}
type EventPage struct {
	Schema         string      `json:"schema"`
	TaskID         string      `json:"task_id"`
	After          string      `json:"after"`
	Events         []TaskEvent `json:"events"`
	NextCursor     string      `json:"next_cursor"`
	HasMore        bool        `json:"has_more"`
	ResyncRequired bool        `json:"resync_required"`
}
type Receipt struct {
	ID       string `json:"id"`
	TaskID   string `json:"task_id,omitempty"`
	Revision string `json:"revision,omitempty"`
	TaskSeq  string `json:"task_seq,omitempty"`
	Status   string `json:"status"` // B1 always recorded; managed gate/finalize have explicit receipts
}
type CreateInput struct {
	RequestID          string   `json:"request_id"`
	WorkspaceSessionID string   `json:"workspace_session_id"`
	WorkspacePath      string   `json:"workspace_path,omitempty"`
	Goal               string   `json:"goal"`
	Acceptance         []string `json:"acceptance"`
}
type ExternalInput struct {
	RequestID       string  `json:"request_id"`
	RuntimeKind     string  `json:"runtime_kind"`
	NativeSessionID *string `json:"native_session_id,omitempty"`
}
type ParticipationInput struct {
	RequestID  string     `json:"request_id"`
	Role       string     `json:"role"`
	SessionRef SessionRef `json:"session_ref"`
}
type AttemptInput struct {
	RequestID       string `json:"request_id"`
	ParticipationID string `json:"participation_id"`
	TurnID          string `json:"turn_id"`
	TurnRequestID   string `json:"turn_request_id"`
	Iteration       int    `json:"iteration"`
}
type ArtifactInput struct {
	RequestID   string  `json:"request_id"`
	AttemptID   string  `json:"attempt_id"`
	Kind        string  `json:"kind"`
	SourceSeq   string  `json:"source_seq"`
	ContentHash *string `json:"content_hash,omitempty"`
	Truncated   bool    `json:"truncated"`
}
type ReportInput struct {
	RequestID          string `json:"request_id"`
	ParticipationID    string `json:"participation_id"`
	SubmissionID       string `json:"submission_id"`
	Iteration          int    `json:"iteration"`
	Content            string `json:"content"`
	Verdict            string `json:"verdict,omitempty"`
	ReviewedArtifactID string `json:"reviewed_artifact_id,omitempty"`
}

// Repository is optional; third-party Session stores need not implement it.
// Every mutation and read-through projection commits state/events atomically.
type Repository interface {
	Identity(context.Context) (Identity, error)
	RegisterExternal(context.Context, Principal, ExternalInput) (SessionRef, error)
	Create(context.Context, Principal, CreateInput) (Receipt, error)
	Participate(context.Context, Principal, string, ParticipationInput) (Receipt, error)
	LinkAttempt(context.Context, Principal, string, AttemptInput) (Receipt, error)
	LinkArtifact(context.Context, Principal, string, ArtifactInput) (Receipt, error)
	Report(context.Context, Principal, string, string, ReportInput) (Receipt, error)
	Snapshot(context.Context, Principal, string) (Snapshot, error)
	List(context.Context, Principal, string, int) ([]CodingTask, error)
	Events(context.Context, Principal, string, int64, int) (EventPage, error)
}
type RepositoryProvider interface {
	TaskRepository(serverID string) Repository
}
