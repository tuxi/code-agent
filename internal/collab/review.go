package collab

import (
	"context"
	"encoding/json"
)

// ReviewPolicy is supplied by trusted host assembly, never by model output.
// ProtectedPaths must include the workspace's configured additions.
type ReviewPolicy struct {
	Files              []string `json:"files"`
	ValidationCommands []string `json:"validation_commands"`
	ProtectedPaths     []string `json:"protected_paths,omitempty"`
}

type ArtifactBinding struct {
	ArtifactID string `json:"artifact_id"`
	Version    string `json:"version"`
	Hash       string `json:"hash"`
}

type FileImage struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Mode   uint32 `json:"mode"`
	Data   []byte `json:"data,omitempty"`
}

type WorkspaceImage struct {
	Head   string      `json:"head"`
	Status string      `json:"status"`
	Index  []FileImage `json:"index"`
	Files  []FileImage `json:"files"`
}

func (s WorkspaceImage) Hash() string { b, _ := json.Marshal(s); return PayloadHash(b) }

type ReviewBundle struct {
	Schema           string            `json:"schema"`
	Source           ManagedTurnRef    `json:"source"`
	AttemptID        string            `json:"attempt_id"`
	TurnID           string            `json:"turn_id"`
	Iteration        int               `json:"iteration"`
	EventCursor      string            `json:"event_cursor"`
	Plan             ArtifactBinding   `json:"plan"`
	Baseline         WorkspaceImage    `json:"baseline"`
	Candidate        WorkspaceImage    `json:"candidate"`
	Patch            string            `json:"patch"`
	IndexPatch       string            `json:"index_patch"`
	ValidationStatus string            `json:"validation_status"`
	Evidence         json.RawMessage   `json:"execution_evidence"`
	Events           []json.RawMessage `json:"events"`
}

// ReviewInput binds a reported review to exactly one immutable candidate.
type ReviewInput struct {
	RequestID               string          `json:"request_id"`
	SubmissionID            string          `json:"producer_submission_id"`
	RunID                   string          `json:"run_id"`
	Epoch                   string          `json:"control_epoch"`
	ReviewerParticipationID string          `json:"reviewer_participation_id"`
	AttemptID               string          `json:"reviewed_attempt_id"`
	Plan                    ArtifactBinding `json:"plan"`
	Artifact                ArtifactBinding `json:"artifact"`
	Verdict                 string          `json:"verdict"`
	Findings                []string        `json:"findings"`
}

type GateDecision struct {
	DecisionID string      `json:"decision_id"`
	Review     ReviewInput `json:"review"`
	Iteration  int         `json:"iteration"`
	Action     string      `json:"action"` // pending, pass, repair, attention
	Reason     string      `json:"reason,omitempty"`
}

type TurnObservation struct {
	Source    ManagedTurnRef `json:"source"`
	SessionID string         `json:"session_id"`
	TurnID    string         `json:"turn_id"`
	Status    string         `json:"status"`
	Terminal  bool           `json:"terminal"`
}

// ReviewRepository is separate from B1 reporting and the B2-a optional port.
// It is not exposed as a capability until startup reconciliation is integrated.
type ReviewRepository interface {
	CaptureBundle(context.Context, Principal, ManagedTurnRef) (TaskArtifactRef, error)
	ReadBundle(context.Context, Principal, string) (ReviewBundle, error)
	SubmitReview(context.Context, Principal, ReviewInput) (Receipt, error)
	Decision(context.Context, Principal, string, int) (GateDecision, error)
	ApplyGate(context.Context, Principal, string, string) (Receipt, error)
	FinalizeRun(context.Context, Principal, string, string) (Receipt, error)
	ObserveTurn(context.Context, Principal, ManagedTurnRef) (TurnObservation, error)
	DispatchAt(context.Context, Principal, string, int) (ManagedDispatch, error)
	WaitForHuman(context.Context, Principal, string, string) (Receipt, error)
}
