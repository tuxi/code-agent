package session

import "time"

// Turn lifecycle status values, persisted in Session.Metadata under MetaTurnStatus
// and surfaced on Meta.TurnStatus. They drive the client's "已暂停 / 恢复中 /
// 思考中" labels (agent-wire v1.2 §1). An unset status ("") means the session has
// no interrupted turn — a normal, completed conversation.
const (
	TurnStatusRunning  = "running"
	TurnStatusPaused   = "paused"
	TurnStatusResuming = "resuming"
	TurnStatusDone     = "done"
	TurnStatusFailed   = "failed"
)

// Metadata keys for the turn lifecycle (v1.2). Values are JSON-round-trippable
// (string / float64) because Metadata is persisted as a JSON blob alongside the
// session — the same mechanism turn_seq already uses.
const (
	MetaTurnStatus      = "turn_status"
	MetaPausedAt        = "paused_at"        // unix seconds (float64 in the map)
	MetaResumeAttempts  = "resume_attempts"  // consecutive failed resumes (float64)
	MetaExecutionPolicy = "execution_policy" // shared_workspace | isolated_worktree | read_only
	MetaWorkspaceID     = "workspace_id"
	MetaBaseWorkspaceID = "base_workspace_id"
)

const (
	ExecutionPolicySharedWorkspace  = "shared_workspace"
	ExecutionPolicyIsolatedWorktree = "isolated_worktree"
	ExecutionPolicyReadOnly         = "read_only"
)

// ExecutionPolicy returns the Runtime-enforced workspace mode. Older sessions
// without metadata remain conservatively shared.
func (s *Session) ExecutionPolicy() string {
	if v, ok := s.Metadata[MetaExecutionPolicy].(string); ok {
		switch v {
		case ExecutionPolicySharedWorkspace, ExecutionPolicyIsolatedWorktree, ExecutionPolicyReadOnly:
			return v
		}
	}
	return ExecutionPolicySharedWorkspace
}

func (s *Session) SetExecutionPolicy(policy string) {
	s.ensureMetadata()
	s.Metadata[MetaExecutionPolicy] = policy
}

func (s *Session) ensureMetadata() {
	if s.Metadata == nil {
		s.Metadata = map[string]any{}
	}
}

// TurnStatus returns the persisted turn lifecycle status, or "" if unset.
func (s *Session) TurnStatus() string {
	if v, ok := s.Metadata[MetaTurnStatus].(string); ok {
		return v
	}
	return ""
}

// SetTurnStatus records the turn lifecycle status.
func (s *Session) SetTurnStatus(status string) {
	s.ensureMetadata()
	s.Metadata[MetaTurnStatus] = status
}

// PausedAt returns the unix-seconds timestamp the turn was paused, or 0 if the
// session is not paused.
func (s *Session) PausedAt() int64 {
	return metaInt64(s.Metadata, MetaPausedAt)
}

// MarkPaused sets status=paused and records when, so the client can show
// staleness ("interrupted N minutes ago") and decide auto- vs prompted-resume.
func (s *Session) MarkPaused(now time.Time) {
	s.ensureMetadata()
	s.Metadata[MetaTurnStatus] = TurnStatusPaused
	s.Metadata[MetaPausedAt] = float64(now.Unix())
}

// ResumeAttempts returns the count of consecutive failed resume attempts.
func (s *Session) ResumeAttempts() int {
	return int(metaInt64(s.Metadata, MetaResumeAttempts))
}

// IncResumeAttempts bumps the consecutive-failure counter and returns the new
// value. The lifecycle layer escalates to TurnStatusFailed once it exceeds the
// retry cap, so a permanently-failing history is not retried forever (v1.2 §3.2.1).
func (s *Session) IncResumeAttempts() int {
	s.ensureMetadata()
	n := s.ResumeAttempts() + 1
	s.Metadata[MetaResumeAttempts] = float64(n)
	return n
}

// ClearResumeAttempts resets the counter. It MUST be called on a successful
// resume so a later, unrelated suspend/resume does not inherit a stale count and
// escalate to failed prematurely (v1.2 §3.2.1).
func (s *Session) ClearResumeAttempts() {
	if s.Metadata != nil {
		delete(s.Metadata, MetaResumeAttempts)
	}
}

func metaInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
