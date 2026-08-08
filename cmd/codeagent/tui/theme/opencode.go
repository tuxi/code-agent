package theme

import (
	"charm.land/lipgloss/v2"
)

// OpenCodeTheme implements the Theme interface with OpenCode brand colors.
// It provides both dark and light variants.
type OpenCodeTheme struct {
	BaseTheme
}

// NewOpenCodeTheme creates a new instance of the OpenCode theme.
func NewOpenCodeTheme() *OpenCodeTheme {
	// OpenCode color palette
	// Dark mode colors
	darkBackground := "#212121"
	darkCurrentLine := "#252525"
	darkSelection := "#303030"
	darkForeground := "#e0e0e0"
	darkComment := "#6a6a6a"
	darkPrimary := "#fab283"   // Primary orange/gold
	darkSecondary := "#5c9cf5" // Secondary blue
	darkAccent := "#9d7cd8"    // Accent purple
	darkRed := "#e06c75"       // Error red
	darkOrange := "#f5a742"    // Warning orange
	darkGreen := "#7fd88f"     // Success green
	darkCyan := "#56b6c2"      // Info cyan
	darkYellow := "#e5c07b"    // Emphasized text
	darkBorder := "#4b4c5c"    // Border color

	// Light mode colors
	lightBackground := "#f8f8f8"
	lightCurrentLine := "#f0f0f0"
	lightSelection := "#e5e5e6"
	lightForeground := "#2a2a2a"
	lightComment := "#8a8a8a"
	lightPrimary := "#3b7dd8"   // Primary blue
	lightSecondary := "#7b5bb6" // Secondary purple
	lightAccent := "#d68c27"    // Accent orange/gold
	lightRed := "#d1383d"       // Error red
	lightOrange := "#d68c27"    // Warning orange
	lightGreen := "#3d9a57"     // Success green
	lightCyan := "#318795"      // Info cyan
	lightYellow := "#b0851f"    // Emphasized text
	lightBorder := "#d3d3d3"    // Border color

	theme := &OpenCodeTheme{}

	// Base colors
	theme.PrimaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPrimary),
		Light: lipgloss.Color(lightPrimary),
	}
	theme.SecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkSecondary),
		Light: lipgloss.Color(lightSecondary),
	}
	theme.AccentColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkAccent),
		Light: lipgloss.Color(lightAccent),
	}

	// Status colors
	theme.ErrorColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkRed),
		Light: lipgloss.Color(lightRed),
	}
	theme.WarningColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkOrange),
		Light: lipgloss.Color(lightOrange),
	}
	theme.SuccessColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkGreen),
		Light: lipgloss.Color(lightGreen),
	}
	theme.InfoColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}

	// Text colors
	theme.TextColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkForeground),
		Light: lipgloss.Color(lightForeground),
	}
	theme.TextMutedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkComment),
		Light: lipgloss.Color(lightComment),
	}
	theme.TextEmphasizedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkYellow),
		Light: lipgloss.Color(lightYellow),
	}

	// Background colors
	theme.BackgroundColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBackground),
		Light: lipgloss.Color(lightBackground),
	}
	theme.BackgroundSecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCurrentLine),
		Light: lipgloss.Color(lightCurrentLine),
	}
	theme.BackgroundDarkerColor = AdaptiveColor{
		Dark:  lipgloss.Color("#121212"), // Slightly darker than background
		Light: lipgloss.Color("#ffffff"), // Slightly lighter than background
	}

	// Border colors
	theme.BorderNormalColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBorder),
		Light: lipgloss.Color(lightBorder),
	}
	theme.BorderFocusedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPrimary),
		Light: lipgloss.Color(lightPrimary),
	}
	theme.BorderDimColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkSelection),
		Light: lipgloss.Color(lightSelection),
	}

	// Diff view colors
	theme.DiffAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#478247"),
		Light: lipgloss.Color("#2E7D32"),
	}
	theme.DiffRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#7C4444"),
		Light: lipgloss.Color("#C62828"),
	}
	theme.DiffContextColor = AdaptiveColor{
		Dark:  lipgloss.Color("#a0a0a0"),
		Light: lipgloss.Color("#757575"),
	}
	theme.DiffHunkHeaderColor = AdaptiveColor{
		Dark:  lipgloss.Color("#a0a0a0"),
		Light: lipgloss.Color("#757575"),
	}
	theme.DiffHighlightAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#DAFADA"),
		Light: lipgloss.Color("#A5D6A7"),
	}
	theme.DiffHighlightRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#FADADD"),
		Light: lipgloss.Color("#EF9A9A"),
	}
	theme.DiffAddedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#303A30"),
		Light: lipgloss.Color("#E8F5E9"),
	}
	theme.DiffRemovedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#3A3030"),
		Light: lipgloss.Color("#FFEBEE"),
	}
	theme.DiffContextBgColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBackground),
		Light: lipgloss.Color(lightBackground),
	}
	theme.DiffLineNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color("#888888"),
		Light: lipgloss.Color("#9E9E9E"),
	}
	theme.DiffAddedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#293229"),
		Light: lipgloss.Color("#C8E6C9"),
	}
	theme.DiffRemovedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#332929"),
		Light: lipgloss.Color("#FFCDD2"),
	}

	// Markdown colors
	theme.MarkdownTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkForeground),
		Light: lipgloss.Color(lightForeground),
	}
	theme.MarkdownHeadingColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkSecondary),
		Light: lipgloss.Color(lightSecondary),
	}
	theme.MarkdownLinkColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPrimary),
		Light: lipgloss.Color(lightPrimary),
	}
	theme.MarkdownLinkTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.MarkdownCodeColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkGreen),
		Light: lipgloss.Color(lightGreen),
	}
	theme.MarkdownBlockQuoteColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkYellow),
		Light: lipgloss.Color(lightYellow),
	}
	theme.MarkdownEmphColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkYellow),
		Light: lipgloss.Color(lightYellow),
	}
	theme.MarkdownStrongColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkAccent),
		Light: lipgloss.Color(lightAccent),
	}
	theme.MarkdownHorizontalRuleColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkComment),
		Light: lipgloss.Color(lightComment),
	}
	theme.MarkdownListItemColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPrimary),
		Light: lipgloss.Color(lightPrimary),
	}
	theme.MarkdownListEnumerationColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.MarkdownImageColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPrimary),
		Light: lipgloss.Color(lightPrimary),
	}
	theme.MarkdownImageTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.MarkdownCodeBlockColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkForeground),
		Light: lipgloss.Color(lightForeground),
	}

	// Syntax highlighting colors
	theme.SyntaxCommentColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkComment),
		Light: lipgloss.Color(lightComment),
	}
	theme.SyntaxKeywordColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkSecondary),
		Light: lipgloss.Color(lightSecondary),
	}
	theme.SyntaxFunctionColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPrimary),
		Light: lipgloss.Color(lightPrimary),
	}
	theme.SyntaxVariableColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkRed),
		Light: lipgloss.Color(lightRed),
	}
	theme.SyntaxStringColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkGreen),
		Light: lipgloss.Color(lightGreen),
	}
	theme.SyntaxNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkAccent),
		Light: lipgloss.Color(lightAccent),
	}
	theme.SyntaxTypeColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkYellow),
		Light: lipgloss.Color(lightYellow),
	}
	theme.SyntaxOperatorColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.SyntaxPunctuationColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkForeground),
		Light: lipgloss.Color(lightForeground),
	}

	return theme
}

func init() {
	// Register the OpenCode theme with the theme manager
	RegisterTheme("opencode", NewOpenCodeTheme())
}
