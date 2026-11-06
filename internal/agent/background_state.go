package agent

// BackgroundContinuationInfo is a public projection, never a runnable
// checkpoint. Credentials, model bindings and raw command output stay private.
type BackgroundContinuationInfo struct {
	TurnID   string   `json:"turn_id"`
	State    string   `json:"state"`
	Revision int64    `json:"revision"`
	JobIDs   []string `json:"job_ids,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}
