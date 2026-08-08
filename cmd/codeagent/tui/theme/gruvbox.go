package theme

import (
	"charm.land/lipgloss/v2"
)

// Gruvbox color palette constants
const (
	// Dark theme colors
	gruvboxDarkBg0          = "#282828"
	gruvboxDarkBg0Soft      = "#32302f"
	gruvboxDarkBg1          = "#3c3836"
	gruvboxDarkBg2          = "#504945"
	gruvboxDarkBg3          = "#665c54"
	gruvboxDarkBg4          = "#7c6f64"
	gruvboxDarkFg0          = "#fbf1c7"
	gruvboxDarkFg1          = "#ebdbb2"
	gruvboxDarkFg2          = "#d5c4a1"
	gruvboxDarkFg3          = "#bdae93"
	gruvboxDarkFg4          = "#a89984"
	gruvboxDarkGray         = "#928374"
	gruvboxDarkRed          = "#cc241d"
	gruvboxDarkRedBright    = "#fb4934"
	gruvboxDarkGreen        = "#98971a"
	gruvboxDarkGreenBright  = "#b8bb26"
	gruvboxDarkYellow       = "#d79921"
	gruvboxDarkYellowBright = "#fabd2f"
	gruvboxDarkBlue         = "#458588"
	gruvboxDarkBlueBright   = "#83a598"
	gruvboxDarkPurple       = "#b16286"
	gruvboxDarkPurpleBright = "#d3869b"
	gruvboxDarkAqua         = "#689d6a"
	gruvboxDarkAquaBright   = "#8ec07c"
	gruvboxDarkOrange       = "#d65d0e"
	gruvboxDarkOrangeBright = "#fe8019"

	// Light theme colors
	gruvboxLightBg0          = "#fbf1c7"
	gruvboxLightBg0Soft      = "#f2e5bc"
	gruvboxLightBg1          = "#ebdbb2"
	gruvboxLightBg2          = "#d5c4a1"
	gruvboxLightBg3          = "#bdae93"
	gruvboxLightBg4          = "#a89984"
	gruvboxLightFg0          = "#282828"
	gruvboxLightFg1          = "#3c3836"
	gruvboxLightFg2          = "#504945"
	gruvboxLightFg3          = "#665c54"
	gruvboxLightFg4          = "#7c6f64"
	gruvboxLightGray         = "#928374"
	gruvboxLightRed          = "#9d0006"
	gruvboxLightRedBright    = "#cc241d"
	gruvboxLightGreen        = "#79740e"
	gruvboxLightGreenBright  = "#98971a"
	gruvboxLightYellow       = "#b57614"
	gruvboxLightYellowBright = "#d79921"
	gruvboxLightBlue         = "#076678"
	gruvboxLightBlueBright   = "#458588"
	gruvboxLightPurple       = "#8f3f71"
	gruvboxLightPurpleBright = "#b16286"
	gruvboxLightAqua         = "#427b58"
	gruvboxLightAquaBright   = "#689d6a"
	gruvboxLightOrange       = "#af3a03"
	gruvboxLightOrangeBright = "#d65d0e"
)

// GruvboxTheme implements the Theme interface with Gruvbox colors.
// It provides both dark and light variants.
type GruvboxTheme struct {
	BaseTheme
}

// NewGruvboxTheme creates a new instance of the Gruvbox theme.
func NewGruvboxTheme() *GruvboxTheme {
	theme := &GruvboxTheme{}

	// Base colors
	theme.PrimaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}
	theme.SecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkPurpleBright),
		Light: lipgloss.Color(gruvboxLightPurpleBright),
	}
	theme.AccentColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkOrangeBright),
		Light: lipgloss.Color(gruvboxLightOrangeBright),
	}

	// Status colors
	theme.ErrorColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkRedBright),
		Light: lipgloss.Color(gruvboxLightRedBright),
	}
	theme.WarningColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkYellowBright),
		Light: lipgloss.Color(gruvboxLightYellowBright),
	}
	theme.SuccessColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkGreenBright),
		Light: lipgloss.Color(gruvboxLightGreenBright),
	}
	theme.InfoColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}

	// Text colors
	theme.TextColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg1),
		Light: lipgloss.Color(gruvboxLightFg1),
	}
	theme.TextMutedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg4),
		Light: lipgloss.Color(gruvboxLightFg4),
	}
	theme.TextEmphasizedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkYellowBright),
		Light: lipgloss.Color(gruvboxLightYellowBright),
	}

	// Background colors
	theme.BackgroundColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg0),
		Light: lipgloss.Color(gruvboxLightBg0),
	}
	theme.BackgroundSecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg1),
		Light: lipgloss.Color(gruvboxLightBg1),
	}
	theme.BackgroundDarkerColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg0Soft),
		Light: lipgloss.Color(gruvboxLightBg0Soft),
	}

	// Border colors
	theme.BorderNormalColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg2),
		Light: lipgloss.Color(gruvboxLightBg2),
	}
	theme.BorderFocusedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}
	theme.BorderDimColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg1),
		Light: lipgloss.Color(gruvboxLightBg1),
	}

	// Diff view colors
	theme.DiffAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkGreenBright),
		Light: lipgloss.Color(gruvboxLightGreenBright),
	}
	theme.DiffRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkRedBright),
		Light: lipgloss.Color(gruvboxLightRedBright),
	}
	theme.DiffContextColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg4),
		Light: lipgloss.Color(gruvboxLightFg4),
	}
	theme.DiffHunkHeaderColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg3),
		Light: lipgloss.Color(gruvboxLightFg3),
	}
	theme.DiffHighlightAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkGreenBright),
		Light: lipgloss.Color(gruvboxLightGreenBright),
	}
	theme.DiffHighlightRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkRedBright),
		Light: lipgloss.Color(gruvboxLightRedBright),
	}
	theme.DiffAddedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#3C4C3C"), // Darker green background
		Light: lipgloss.Color("#E8F5E9"), // Light green background
	}
	theme.DiffRemovedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#4C3C3C"), // Darker red background
		Light: lipgloss.Color("#FFEBEE"), // Light red background
	}
	theme.DiffContextBgColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg0),
		Light: lipgloss.Color(gruvboxLightBg0),
	}
	theme.DiffLineNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg4),
		Light: lipgloss.Color(gruvboxLightFg4),
	}
	theme.DiffAddedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#32432F"), // Slightly darker green
		Light: lipgloss.Color("#C8E6C9"), // Light green
	}
	theme.DiffRemovedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#43322F"), // Slightly darker red
		Light: lipgloss.Color("#FFCDD2"), // Light red
	}

	// Markdown colors
	theme.MarkdownTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg1),
		Light: lipgloss.Color(gruvboxLightFg1),
	}
	theme.MarkdownHeadingColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkYellowBright),
		Light: lipgloss.Color(gruvboxLightYellowBright),
	}
	theme.MarkdownLinkColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}
	theme.MarkdownLinkTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkAquaBright),
		Light: lipgloss.Color(gruvboxLightAquaBright),
	}
	theme.MarkdownCodeColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkGreenBright),
		Light: lipgloss.Color(gruvboxLightGreenBright),
	}
	theme.MarkdownBlockQuoteColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkAquaBright),
		Light: lipgloss.Color(gruvboxLightAquaBright),
	}
	theme.MarkdownEmphColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkYellowBright),
		Light: lipgloss.Color(gruvboxLightYellowBright),
	}
	theme.MarkdownStrongColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkOrangeBright),
		Light: lipgloss.Color(gruvboxLightOrangeBright),
	}
	theme.MarkdownHorizontalRuleColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBg3),
		Light: lipgloss.Color(gruvboxLightBg3),
	}
	theme.MarkdownListItemColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}
	theme.MarkdownListEnumerationColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}
	theme.MarkdownImageColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkPurpleBright),
		Light: lipgloss.Color(gruvboxLightPurpleBright),
	}
	theme.MarkdownImageTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkAquaBright),
		Light: lipgloss.Color(gruvboxLightAquaBright),
	}
	theme.MarkdownCodeBlockColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg1),
		Light: lipgloss.Color(gruvboxLightFg1),
	}

	// Syntax highlighting colors
	theme.SyntaxCommentColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkGray),
		Light: lipgloss.Color(gruvboxLightGray),
	}
	theme.SyntaxKeywordColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkRedBright),
		Light: lipgloss.Color(gruvboxLightRedBright),
	}
	theme.SyntaxFunctionColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkGreenBright),
		Light: lipgloss.Color(gruvboxLightGreenBright),
	}
	theme.SyntaxVariableColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkBlueBright),
		Light: lipgloss.Color(gruvboxLightBlueBright),
	}
	theme.SyntaxStringColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkYellowBright),
		Light: lipgloss.Color(gruvboxLightYellowBright),
	}
	theme.SyntaxNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkPurpleBright),
		Light: lipgloss.Color(gruvboxLightPurpleBright),
	}
	theme.SyntaxTypeColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkYellow),
		Light: lipgloss.Color(gruvboxLightYellow),
	}
	theme.SyntaxOperatorColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkAquaBright),
		Light: lipgloss.Color(gruvboxLightAquaBright),
	}
	theme.SyntaxPunctuationColor = AdaptiveColor{
		Dark:  lipgloss.Color(gruvboxDarkFg1),
		Light: lipgloss.Color(gruvboxLightFg1),
	}

	return theme
}

func init() {
	// Register the Gruvbox theme with the theme manager
	RegisterTheme("gruvbox", NewGruvboxTheme())
}
