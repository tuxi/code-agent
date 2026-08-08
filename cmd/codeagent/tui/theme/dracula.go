package theme

import (
	"charm.land/lipgloss/v2"
)

// DraculaTheme implements the Theme interface with Dracula colors.
// It provides both dark and light variants, though Dracula is primarily a dark theme.
type DraculaTheme struct {
	BaseTheme
}

// NewDraculaTheme creates a new instance of the Dracula theme.
func NewDraculaTheme() *DraculaTheme {
	// Dracula color palette
	// Official colors from https://draculatheme.com/
	darkBackground := "#282a36"
	darkCurrentLine := "#44475a"
	darkSelection := "#44475a"
	darkForeground := "#f8f8f2"
	darkComment := "#6272a4"
	darkCyan := "#8be9fd"
	darkGreen := "#50fa7b"
	darkOrange := "#ffb86c"
	darkPink := "#ff79c6"
	darkPurple := "#bd93f9"
	darkRed := "#ff5555"
	darkYellow := "#f1fa8c"
	darkBorder := "#44475a"

	// Light mode approximation (Dracula is primarily a dark theme)
	lightBackground := "#f8f8f2"
	lightCurrentLine := "#e6e6e6"
	lightSelection := "#d8d8d8"
	lightForeground := "#282a36"
	lightComment := "#6272a4"
	lightCyan := "#0097a7"
	lightGreen := "#388e3c"
	lightOrange := "#f57c00"
	lightPink := "#d81b60"
	lightPurple := "#7e57c2"
	lightRed := "#e53935"
	lightYellow := "#fbc02d"
	lightBorder := "#d8d8d8"

	theme := &DraculaTheme{}

	// Base colors
	theme.PrimaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.SecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPink),
		Light: lipgloss.Color(lightPink),
	}
	theme.AccentColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
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
		Dark:  lipgloss.Color("#21222c"), // Slightly darker than background
		Light: lipgloss.Color("#ffffff"), // Slightly lighter than background
	}

	// Border colors
	theme.BorderNormalColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBorder),
		Light: lipgloss.Color(lightBorder),
	}
	theme.BorderFocusedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.BorderDimColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkSelection),
		Light: lipgloss.Color(lightSelection),
	}

	// Diff view colors
	theme.DiffAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkGreen),
		Light: lipgloss.Color(lightGreen),
	}
	theme.DiffRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkRed),
		Light: lipgloss.Color(lightRed),
	}
	theme.DiffContextColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkComment),
		Light: lipgloss.Color(lightComment),
	}
	theme.DiffHunkHeaderColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.DiffHighlightAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#50fa7b"),
		Light: lipgloss.Color("#a5d6a7"),
	}
	theme.DiffHighlightRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#ff5555"),
		Light: lipgloss.Color("#ef9a9a"),
	}
	theme.DiffAddedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#2c3b2c"),
		Light: lipgloss.Color("#e8f5e9"),
	}
	theme.DiffRemovedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#3b2c2c"),
		Light: lipgloss.Color("#ffebee"),
	}
	theme.DiffContextBgColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBackground),
		Light: lipgloss.Color(lightBackground),
	}
	theme.DiffLineNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkComment),
		Light: lipgloss.Color(lightComment),
	}
	theme.DiffAddedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#253025"),
		Light: lipgloss.Color("#c8e6c9"),
	}
	theme.DiffRemovedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#302525"),
		Light: lipgloss.Color("#ffcdd2"),
	}

	// Markdown colors
	theme.MarkdownTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkForeground),
		Light: lipgloss.Color(lightForeground),
	}
	theme.MarkdownHeadingColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPink),
		Light: lipgloss.Color(lightPink),
	}
	theme.MarkdownLinkColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
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
		Dark:  lipgloss.Color(darkOrange),
		Light: lipgloss.Color(lightOrange),
	}
	theme.MarkdownHorizontalRuleColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkComment),
		Light: lipgloss.Color(lightComment),
	}
	theme.MarkdownListItemColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.MarkdownListEnumerationColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.MarkdownImageColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
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
		Dark:  lipgloss.Color(darkPink),
		Light: lipgloss.Color(lightPink),
	}
	theme.SyntaxFunctionColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkGreen),
		Light: lipgloss.Color(lightGreen),
	}
	theme.SyntaxVariableColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkOrange),
		Light: lipgloss.Color(lightOrange),
	}
	theme.SyntaxStringColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkYellow),
		Light: lipgloss.Color(lightYellow),
	}
	theme.SyntaxNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.SyntaxTypeColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.SyntaxOperatorColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPink),
		Light: lipgloss.Color(lightPink),
	}
	theme.SyntaxPunctuationColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkForeground),
		Light: lipgloss.Color(lightForeground),
	}

	return theme
}

func init() {
	// Register the Dracula theme with the theme manager
	RegisterTheme("dracula", NewDraculaTheme())
}
