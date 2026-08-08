// Package chat implements the chat transcript and composer components of the
// TUI, ported from opencode (internal/tui/components/chat) and adapted to
// code-agent's local infrastructure.
//
// This file defines the empty-state header (logo + cwd) shown by the chat page
// before any message exists. opencode's LSP section is dropped: code-agent has
// no LSP integration (see plan §5.7), so the header is just logo + cwd.
package chat

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	chAnsi "github.com/charmbracelet/x/ansi"
	"os"

	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
)

// header renders the empty-state header: logo + cwd.
func header(width int) string {
	return lipgloss.JoinVertical(
		lipgloss.Top,
		logo(width),
		"",
		cwd(width),
	)
}

// logo renders the app logo. opencode's version tag is dropped; code-agent has
// no version package (see plan §5.5).
func logo(width int) string {
	logo := fmt.Sprintf("%s %s", styles.OpenCodeIcon, "CodeAgent")
	baseStyle := styles.BaseStyle()
	return baseStyle.
		Bold(true).
		Width(width).
		Render(
			lipgloss.JoinHorizontal(lipgloss.Left, logo),
		)
}

// cwd renders the current working directory.
func cwd(width int) string {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	text := fmt.Sprintf("cwd: %s", chAnsi.Truncate(wd, width, "…"))
	t := theme.CurrentTheme()
	return styles.BaseStyle().
		Foreground(t.TextMuted()).
		Width(width).
		Render(text)
}
