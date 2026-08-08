// Package page defines the top-level TUI pages and the message used to switch
// between them. Ported from opencode (internal/tui/page) and adapted to
// code-agent's local infrastructure.
package page

// PageID identifies a page in the TUI.
type PageID string

// PageChangeMsg is sent to request a switch to another page.
type PageChangeMsg struct {
	ID PageID
}
