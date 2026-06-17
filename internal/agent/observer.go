package agent

import "code-agent/internal/observation"

// Observer enriches a raw tool result into a structured Observation (P4.1). The
// loop consults it after each tool call to make failures legible to the model —
// it is a *data* hook, never control: it classifies and summarizes, it does not
// decide what the agent does next. The model still owns control flow.
//
// Defined here (like Approver and Emitter) so the loop depends only on the
// interface; the implementation lives in the observation package. Nil-safe: when
// unset, the loop appends raw tool results exactly as before.
type Observer interface {
	Observe(tool, rawObservation string) observation.Observation
}
