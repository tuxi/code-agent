// Package theme centralizes every lipgloss style the TUI uses, so colors and
// borders live in one place instead of scattered as package-level vars across
// view.go, model.go, steps.go, and sessions.go. lipgloss.AdaptiveColor already
// picks light/dark per the terminal background, and lipgloss honors NO_COLOR
// automatically — so a single Theme with adaptive colors covers both profiles
// without a runtime switch.
package theme

import "github.com/charmbracelet/lipgloss"

// Theme bundles the semantic colors, text styles, and card borders the TUI
// renders with. Every field is a value (not a pointer), so passing Theme by
// value is cheap and safe.
type Theme struct {
	// Semantic colors (AdaptiveColor picks light/dark per terminal background).
	// Named with a Color suffix so they never collide with the style fields
	// below (e.g. OKColor the color vs OK the style).
	AccentColor     lipgloss.AdaptiveColor
	OKColor         lipgloss.AdaptiveColor
	FailColor       lipgloss.AdaptiveColor
	SkillColor      lipgloss.AdaptiveColor
	ReflectionColor lipgloss.AdaptiveColor

	// Text styles.
	User       lipgloss.Style // bold, accent — the user's prompt
	Thinking   lipgloss.Style // faint + italic — model reasoning / step headers
	OK         lipgloss.Style // ✓ success marker
	Fail       lipgloss.Style // bold, fail — ✗ failure marker and error text
	Skill      lipgloss.Style // skill cards and the PLAN badge
	Reflection lipgloss.Style // reflection cards
	Meta       lipgloss.Style // faint — timestamps, hints, secondary info
	Args       lipgloss.Style // faint — tool argument hints
	Assistant  lipgloss.Style // plain — the assistant reply body
	AsstLabel  lipgloss.Style // bold, accent — the "⏺ assistant" label
	Body       lipgloss.Style // faint — tool result bodies, indented detail
	Soon       lipgloss.Style // faint + italic — "(soon)" deferred-command hint

	// Interactive elements.
	PaletteSel lipgloss.Style // bold, accent — selected row in pickers/palette
	ApproveHl  lipgloss.Style // bold, accent — highlighted choice in a card
	ApproveDim lipgloss.Style // faint — non-highlighted choices in a card

	// Card border (alt-screen panels and overlays share this).
	Border   lipgloss.Border
	BorderFg lipgloss.AdaptiveColor
}

// Default is the single theme the TUI references. AdaptiveColor + lipgloss's
// built-in NO_COLOR handling means this one instance covers light terminals,
// dark terminals, and NO_COLOR environments without a runtime picker.
var Default = Theme{
	AccentColor:     lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"},
	OKColor:         lipgloss.AdaptiveColor{Light: "#207020", Dark: "#5fd75f"},
	FailColor:       lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff5f5f"},
	SkillColor:      lipgloss.AdaptiveColor{Light: "#8700af", Dark: "#d787ff"},
	ReflectionColor: lipgloss.AdaptiveColor{Light: "#af5f00", Dark: "#ffaf5f"},

	User:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"}),
	Thinking:   lipgloss.NewStyle().Faint(true).Italic(true),
	OK:         lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#207020", Dark: "#5fd75f"}),
	Fail:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff5f5f"}),
	Skill:      lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8700af", Dark: "#d787ff"}),
	Reflection: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#af5f00", Dark: "#ffaf5f"}),
	Meta:       lipgloss.NewStyle().Faint(true),
	Args:       lipgloss.NewStyle().Faint(true),
	Assistant:  lipgloss.NewStyle(),
	AsstLabel:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"}),
	Body:       lipgloss.NewStyle().Faint(true),
	Soon:       lipgloss.NewStyle().Faint(true).Italic(true),

	PaletteSel: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"}),
	ApproveHl:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"}),
	ApproveDim: lipgloss.NewStyle().Faint(true),

	Border:   lipgloss.RoundedBorder(),
	BorderFg: lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff5f5f"},
}

// ApproveBox returns a fresh style with the rounded border applied. Callers
// chain .Width(n) on the result; constructing per-call avoids the receiver
// mutation that a cached style would suffer.
func (t Theme) ApproveBox() lipgloss.Style {
	return lipgloss.NewStyle().Border(t.Border).BorderForeground(t.FailColor)
}
