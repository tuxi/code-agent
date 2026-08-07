package tui

import tea "github.com/charmbracelet/bubbletea"

// Overlay is one modal view in the bottom live region: a key handler plus its
// render. The model holds at most one open overlay (m.overlay; nil = none).
// Approval cards, plan/ask_user cards, and the /resume and /use pickers are all
// overlays — previously each had its own field, handler, and render switch in
// model.go (the God Object this interface splits).
//
// State lives in the overlay value, which the model stores by pointer, so a Key
// mutation survives the model being copied on every Update. An overlay must
// therefore NOT capture a *model or a bound method value at construction: those
// would point at a stale model copy, and every subsequent Update returns a new
// one. Handlers that need the model (picker enter calls m.resume) receive the
// CURRENT model through Key's m argument instead — the same pattern the old
// handlePickerKey used.
type Overlay interface {
	// Key handles one keystroke while the overlay is open. handled=false means
	// the key is not the overlay's — it falls through to the model's global keys
	// (ctrl+c/z/o/p) and then the composer. next is the overlay to keep open
	// (nil closes it); the model applies it when handled is true.
	Key(msg tea.KeyMsg, m *model) (next Overlay, handled bool, cmd tea.Cmd)
	// View renders the overlay into the bottom live region. Most overlays are
	// self-contained and ignore m; the palette needs the live composer text.
	View(width int, m *model) []string
}
