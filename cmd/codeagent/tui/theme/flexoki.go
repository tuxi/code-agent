package theme

import (
	"charm.land/lipgloss/v2"
)

// Flexoki color palette constants
const (
	// Base colors
	flexokiPaper   = "#FFFCF0" // Paper (lightest)
	flexokiBase50  = "#F2F0E5" // bg-2 (light)
	flexokiBase100 = "#E6E4D9" // ui (light)
	flexokiBase150 = "#DAD8CE" // ui-2 (light)
	flexokiBase200 = "#CECDC3" // ui-3 (light)
	flexokiBase300 = "#B7B5AC" // tx-3 (light)
	flexokiBase500 = "#878580" // tx-2 (light)
	flexokiBase600 = "#6F6E69" // tx (light)
	flexokiBase700 = "#575653" // tx-3 (dark)
	flexokiBase800 = "#403E3C" // ui-3 (dark)
	flexokiBase850 = "#343331" // ui-2 (dark)
	flexokiBase900 = "#282726" // ui (dark)
	flexokiBase950 = "#1C1B1A" // bg-2 (dark)
	flexokiBlack   = "#100F0F" // bg (darkest)

	// Accent colors - Light theme (600)
	flexokiRed600     = "#AF3029"
	flexokiOrange600  = "#BC5215"
	flexokiYellow600  = "#AD8301"
	flexokiGreen600   = "#66800B"
	flexokiCyan600    = "#24837B"
	flexokiBlue600    = "#205EA6"
	flexokiPurple600  = "#5E409D"
	flexokiMagenta600 = "#A02F6F"

	// Accent colors - Dark theme (400)
	flexokiRed400     = "#D14D41"
	flexokiOrange400  = "#DA702C"
	flexokiYellow400  = "#D0A215"
	flexokiGreen400   = "#879A39"
	flexokiCyan400    = "#3AA99F"
	flexokiBlue400    = "#4385BE"
	flexokiPurple400  = "#8B7EC8"
	flexokiMagenta400 = "#CE5D97"
)

// FlexokiTheme implements the Theme interface with Flexoki colors.
// It provides both dark and light variants.
type FlexokiTheme struct {
	BaseTheme
}

// NewFlexokiTheme creates a new instance of the Flexoki theme.
func NewFlexokiTheme() *FlexokiTheme {
	theme := &FlexokiTheme{}

	// Base colors
	theme.PrimaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlue400),
		Light: lipgloss.Color(flexokiBlue600),
	}
	theme.SecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiPurple400),
		Light: lipgloss.Color(flexokiPurple600),
	}
	theme.AccentColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiOrange400),
		Light: lipgloss.Color(flexokiOrange600),
	}

	// Status colors
	theme.ErrorColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiRed400),
		Light: lipgloss.Color(flexokiRed600),
	}
	theme.WarningColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiYellow400),
		Light: lipgloss.Color(flexokiYellow600),
	}
	theme.SuccessColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiGreen400),
		Light: lipgloss.Color(flexokiGreen600),
	}
	theme.InfoColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiCyan400),
		Light: lipgloss.Color(flexokiCyan600),
	}

	// Text colors
	theme.TextColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase300),
		Light: lipgloss.Color(flexokiBase600),
	}
	theme.TextMutedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase700),
		Light: lipgloss.Color(flexokiBase500),
	}
	theme.TextEmphasizedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiYellow400),
		Light: lipgloss.Color(flexokiYellow600),
	}

	// Background colors
	theme.BackgroundColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlack),
		Light: lipgloss.Color(flexokiPaper),
	}
	theme.BackgroundSecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase950),
		Light: lipgloss.Color(flexokiBase50),
	}
	theme.BackgroundDarkerColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase900),
		Light: lipgloss.Color(flexokiBase100),
	}

	// Border colors
	theme.BorderNormalColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase900),
		Light: lipgloss.Color(flexokiBase100),
	}
	theme.BorderFocusedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlue400),
		Light: lipgloss.Color(flexokiBlue600),
	}
	theme.BorderDimColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase850),
		Light: lipgloss.Color(flexokiBase150),
	}

	// Diff view colors
	theme.DiffAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiGreen400),
		Light: lipgloss.Color(flexokiGreen600),
	}
	theme.DiffRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiRed400),
		Light: lipgloss.Color(flexokiRed600),
	}
	theme.DiffContextColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase700),
		Light: lipgloss.Color(flexokiBase500),
	}
	theme.DiffHunkHeaderColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase700),
		Light: lipgloss.Color(flexokiBase500),
	}
	theme.DiffHighlightAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiGreen400),
		Light: lipgloss.Color(flexokiGreen600),
	}
	theme.DiffHighlightRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiRed400),
		Light: lipgloss.Color(flexokiRed600),
	}
	theme.DiffAddedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#1D2419"), // Darker green background
		Light: lipgloss.Color("#EFF2E2"), // Light green background
	}
	theme.DiffRemovedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#241919"), // Darker red background
		Light: lipgloss.Color("#F2E2E2"), // Light red background
	}
	theme.DiffContextBgColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlack),
		Light: lipgloss.Color(flexokiPaper),
	}
	theme.DiffLineNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase700),
		Light: lipgloss.Color(flexokiBase500),
	}
	theme.DiffAddedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#1A2017"), // Slightly darker green
		Light: lipgloss.Color("#E5EBD9"), // Light green
	}
	theme.DiffRemovedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#201717"), // Slightly darker red
		Light: lipgloss.Color("#EBD9D9"), // Light red
	}

	// Markdown colors
	theme.MarkdownTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase300),
		Light: lipgloss.Color(flexokiBase600),
	}
	theme.MarkdownHeadingColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiYellow400),
		Light: lipgloss.Color(flexokiYellow600),
	}
	theme.MarkdownLinkColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiCyan400),
		Light: lipgloss.Color(flexokiCyan600),
	}
	theme.MarkdownLinkTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiMagenta400),
		Light: lipgloss.Color(flexokiMagenta600),
	}
	theme.MarkdownCodeColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiGreen400),
		Light: lipgloss.Color(flexokiGreen600),
	}
	theme.MarkdownBlockQuoteColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiCyan400),
		Light: lipgloss.Color(flexokiCyan600),
	}
	theme.MarkdownEmphColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiYellow400),
		Light: lipgloss.Color(flexokiYellow600),
	}
	theme.MarkdownStrongColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiOrange400),
		Light: lipgloss.Color(flexokiOrange600),
	}
	theme.MarkdownHorizontalRuleColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase800),
		Light: lipgloss.Color(flexokiBase200),
	}
	theme.MarkdownListItemColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlue400),
		Light: lipgloss.Color(flexokiBlue600),
	}
	theme.MarkdownListEnumerationColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlue400),
		Light: lipgloss.Color(flexokiBlue600),
	}
	theme.MarkdownImageColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiPurple400),
		Light: lipgloss.Color(flexokiPurple600),
	}
	theme.MarkdownImageTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiMagenta400),
		Light: lipgloss.Color(flexokiMagenta600),
	}
	theme.MarkdownCodeBlockColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase300),
		Light: lipgloss.Color(flexokiBase600),
	}

	// Syntax highlighting colors (based on Flexoki's mappings)
	theme.SyntaxCommentColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase700), // tx-3
		Light: lipgloss.Color(flexokiBase300), // tx-3
	}
	theme.SyntaxKeywordColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiGreen400), // gr
		Light: lipgloss.Color(flexokiGreen600), // gr
	}
	theme.SyntaxFunctionColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiOrange400), // or
		Light: lipgloss.Color(flexokiOrange600), // or
	}
	theme.SyntaxVariableColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBlue400), // bl
		Light: lipgloss.Color(flexokiBlue600), // bl
	}
	theme.SyntaxStringColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiCyan400), // cy
		Light: lipgloss.Color(flexokiCyan600), // cy
	}
	theme.SyntaxNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiPurple400), // pu
		Light: lipgloss.Color(flexokiPurple600), // pu
	}
	theme.SyntaxTypeColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiYellow400), // ye
		Light: lipgloss.Color(flexokiYellow600), // ye
	}
	theme.SyntaxOperatorColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase500), // tx-2
		Light: lipgloss.Color(flexokiBase500), // tx-2
	}
	theme.SyntaxPunctuationColor = AdaptiveColor{
		Dark:  lipgloss.Color(flexokiBase500), // tx-2
		Light: lipgloss.Color(flexokiBase500), // tx-2
	}

	return theme
}

func init() {
	// Register the Flexoki theme with the theme manager
	RegisterTheme("flexoki", NewFlexokiTheme())
}
