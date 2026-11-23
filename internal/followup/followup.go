// Package followup defines the structured next-step prompt attached to a
// completed turn. It is shared by the model-facing tool, the agent event, and
// the wire layer so all three describe the same card without importing each
// other's transport types.
package followup

const Capability = "turn_follow_up_v1"

// Offer is one end-of-turn decision card. Options may be empty, in which case
// the card is an open-ended prompt and the user answers through the composer.
type Offer struct {
	ID          string   `json:"id"`
	TurnID      string   `json:"turn_id"`
	Detail      string   `json:"detail"`
	Question    string   `json:"question"`
	Options     []Option `json:"options,omitempty"`
	Dismissible bool     `json:"dismissible"`
}

// Option is one selectable next step. Label is the short button/selected-state
// text; Message is the actual next user message sent to the model.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Message     string `json:"message"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}
