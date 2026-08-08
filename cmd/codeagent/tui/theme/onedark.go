package theme

import (
	"charm.land/lipgloss/v2"
)

// OneDarkTheme implements the Theme interface with Atom's One Dark colors.
// It provides both dark and light variants.
type OneDarkTheme struct {
	BaseTheme
}

// NewOneDarkTheme creates a new instance of the One Dark theme.
func NewOneDarkTheme() *OneDarkTheme {
	// One Dark color palette
	// Dark mode colors from Atom One Dark
	darkBackground := "#282c34"
	darkCurrentLine := "#2c313c"
	darkSelection := "#3e4451"
	darkForeground := "#abb2bf"
	darkComment := "#5c6370"
	darkRed := "#e06c75"
	darkOrange := "#d19a66"
	darkYellow := "#e5c07b"
	darkGreen := "#98c379"
	darkCyan := "#56b6c2"
	darkBlue := "#61afef"
	darkPurple := "#c678dd"
	darkBorder := "#3b4048"

	// Light mode colors from Atom One Light
	lightBackground := "#fafafa"
	lightCurrentLine := "#f0f0f0"
	lightSelection := "#e5e5e6"
	lightForeground := "#383a42"
	lightComment := "#a0a1a7"
	lightRed := "#e45649"
	lightOrange := "#da8548"
	lightYellow := "#c18401"
	lightGreen := "#50a14f"
	lightCyan := "#0184bc"
	lightBlue := "#4078f2"
	lightPurple := "#a626a4"
	lightBorder := "#d3d3d3"

	theme := &OneDarkTheme{}

	// Base colors
	theme.PrimaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
	}
	theme.SecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.AccentColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkOrange),
		Light: lipgloss.Color(lightOrange),
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
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
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
		Dark:  lipgloss.Color("#21252b"), // Slightly darker than background
		Light: lipgloss.Color("#ffffff"), // Slightly lighter than background
	}

	// Border colors
	theme.BorderNormalColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBorder),
		Light: lipgloss.Color(lightBorder),
	}
	theme.BorderFocusedColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
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
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.MarkdownLinkColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
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
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
	}
	theme.MarkdownListEnumerationColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkCyan),
		Light: lipgloss.Color(lightCyan),
	}
	theme.MarkdownImageColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
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
		Dark:  lipgloss.Color(darkPurple),
		Light: lipgloss.Color(lightPurple),
	}
	theme.SyntaxFunctionColor = AdaptiveColor{
		Dark:  lipgloss.Color(darkBlue),
		Light: lipgloss.Color(lightBlue),
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
		Dark:  lipgloss.Color(darkOrange),
		Light: lipgloss.Color(lightOrange),
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
	// Register the One Dark theme with the theme manager
	RegisterTheme("onedark", NewOneDarkTheme())
}
