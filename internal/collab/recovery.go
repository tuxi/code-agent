package collab

import "context"

// RecoveryRepository is a host-only startup port. It never resumes a Turn or
// treats expiry/PID disappearance as proof that side effects did not happen.
type RecoveryRepository interface {
	LockManagedHost(context.Context) (func(), error)
	ActiveRuns(context.Context, Principal) ([]ManagedRun, error)
	ReconcileRun(context.Context, Principal, string) (RecoveryCheck, error)
	FenceRun(context.Context, Principal, string, string) error
}

type RecoveryCheck struct {
	Run    ManagedRun `json:"run"`
	Safe   bool       `json:"safe_to_observe"`
	Reason string     `json:"reason,omitempty"`
}

// StartRunInput deliberately excludes workflow identity, protected-path policy,
// and generation parameters. An optional executor model uses the host catalog.
type StartRunInput struct {
	ExecutorModel           string   `json:"executor_model,omitempty"`
	RequestID               string   `json:"request_id"`
	PlanArtifactID          string   `json:"plan_artifact_id"`
	ExecutorParticipationID string   `json:"executor_participation_id"`
	ReviewerParticipationID string   `json:"reviewer_participation_id"`
	MaxIterations           int      `json:"max_iterations"`
	Files                   []string `json:"files"`
	ValidationCommands      []string `json:"validation_commands"`
}

type ManagedService interface {
	Ready() bool
	StartRun(context.Context, Principal, string, StartRunInput) (Receipt, error)
	ReportReview(context.Context, Principal, string, string, ReviewInput) (Receipt, error)
	Artifact(context.Context, Principal, string, string) (ReviewBundle, error)
}
