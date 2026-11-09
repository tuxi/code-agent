package preview

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Event is the preview-specific subset of the Agent Wire envelope.
type Event struct {
	Kind      string    `json:"kind"`
	EventID   string    `json:"event_id,omitempty"`
	Seq       int64     `json:"seq,omitempty"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	CallID    string    `json:"call_id,omitempty"`
	Artifact  *Artifact `json:"artifact"`
}

type ReplayResult struct {
	Artifacts []Artifact
	ByID      map[string]Artifact
	Error     *Error
}

func DecodeEvent(data []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return Event{}, err
	}
	return event, nil
}

// ValidateEventEnvelope checks only the transport envelope and correlation
// binding. Artifact-version or digest failures stay in the preview block so a
// client can degrade it without losing the containing turn event.
func ValidateEventEnvelope(event Event) *Error {
	if event.Kind != "preview_artifact" {
		return NewError(CodeInvalidArtifact, "Preview event kind is invalid.", false)
	}
	if event.SessionID == "" || event.TurnID == "" || event.Artifact == nil {
		return NewError(CodeInvalidArtifact, "Preview event envelope is incomplete.", false)
	}
	if event.Artifact.SessionID != event.SessionID || event.Artifact.TurnID != event.TurnID {
		return NewError(CodeInvalidArtifact, "Preview event correlation does not match its artifact.", false)
	}
	if event.CallID != "" && event.Artifact.CallID != "" && event.CallID != event.Artifact.CallID {
		return NewError(CodeInvalidArtifact, "Preview event call correlation does not match its artifact.", false)
	}
	return nil
}

func ValidateEventCorrelation(event Event) *Error {
	if err := ValidateEventEnvelope(event); err != nil {
		return err
	}
	artifact := event.Artifact.Clone()
	artifact.RevisionSHA256 = event.Artifact.RevisionSHA256
	if err := artifact.Validate(); err != nil {
		return err
	}
	return nil
}

// ApplyReplay folds complete preview snapshots by artifact ID and revision.
// Higher revisions replace lower ones; duplicate revisions are idempotent only
// when their revision digests match.
func ApplyReplay(events []Event) ReplayResult {
	state := make(map[string]Artifact)
	for _, event := range events {
		if err := ValidateEventCorrelation(event); err != nil {
			return ReplayResult{ByID: state, Artifacts: sortedArtifacts(state), Error: err}
		}
		artifact := event.Artifact.Clone()
		current, ok := state[artifact.ArtifactID]
		if !ok {
			state[artifact.ArtifactID] = artifact
			continue
		}
		switch {
		case artifact.Revision > current.Revision:
			state[artifact.ArtifactID] = artifact
		case artifact.Revision == current.Revision:
			if artifact.RevisionSHA256 != current.RevisionSHA256 {
				return ReplayResult{ByID: state, Artifacts: sortedArtifacts(state), Error: NewError(CodeRevisionConflict, "Preview revision conflicts with the persisted snapshot.", false)}
			}
		}
	}
	return ReplayResult{ByID: state, Artifacts: sortedArtifacts(state)}
}

func sortedArtifacts(state map[string]Artifact) []Artifact {
	ids := make([]string, 0, len(state))
	for id := range state {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Artifact, 0, len(ids))
	for _, id := range ids {
		out = append(out, state[id])
	}
	return out
}

func EventTime(event Event) time.Time {
	if event.Artifact == nil {
		return time.Time{}
	}
	return event.Artifact.UpdatedAt
}

func (r ReplayResult) String() string {
	return fmt.Sprintf("preview replay: %d artifact(s)", len(r.Artifacts))
}
