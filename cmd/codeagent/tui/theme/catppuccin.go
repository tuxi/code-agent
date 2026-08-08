package theme

import (
	"charm.land/lipgloss/v2"
	catppuccin "github.com/catppuccin/go"
)

// CatppuccinTheme implements the Theme interface with Catppuccin colors.
// It provides both dark (Mocha) and light (Latte) variants.
type CatppuccinTheme struct {
	BaseTheme
}

// NewCatppuccinTheme creates a new instance of the Catppuccin theme.
func NewCatppuccinTheme() *CatppuccinTheme {
	// Get the Catppuccin palettes
	mocha := catppuccin.Mocha
	latte := catppuccin.Latte

	theme := &CatppuccinTheme{}

	// Base colors
	theme.PrimaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Blue().Hex),
		Light: lipgloss.Color(latte.Blue().Hex),
	}
	theme.SecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Mauve().Hex),
		Light: lipgloss.Color(latte.Mauve().Hex),
	}
	theme.AccentColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Peach().Hex),
		Light: lipgloss.Color(latte.Peach().Hex),
	}

	// Status colors
	theme.ErrorColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Red().Hex),
		Light: lipgloss.Color(latte.Red().Hex),
	}
	theme.WarningColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Peach().Hex),
		Light: lipgloss.Color(latte.Peach().Hex),
	}
	theme.SuccessColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Green().Hex),
		Light: lipgloss.Color(latte.Green().Hex),
	}
	theme.InfoColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Blue().Hex),
		Light: lipgloss.Color(latte.Blue().Hex),
	}

	// Text colors
	theme.TextColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Text().Hex),
		Light: lipgloss.Color(latte.Text().Hex),
	}
	theme.TextMutedColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Subtext0().Hex),
		Light: lipgloss.Color(latte.Subtext0().Hex),
	}
	theme.TextEmphasizedColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Lavender().Hex),
		Light: lipgloss.Color(latte.Lavender().Hex),
	}

	// Background colors
	theme.BackgroundColor = AdaptiveColor{
		Dark:  lipgloss.Color("#212121"), // From existing styles
		Light: lipgloss.Color("#EEEEEE"), // Light equivalent
	}
	theme.BackgroundSecondaryColor = AdaptiveColor{
		Dark:  lipgloss.Color("#2c2c2c"), // From existing styles
		Light: lipgloss.Color("#E0E0E0"), // Light equivalent
	}
	theme.BackgroundDarkerColor = AdaptiveColor{
		Dark:  lipgloss.Color("#181818"), // From existing styles
		Light: lipgloss.Color("#F5F5F5"), // Light equivalent
	}

	// Border colors
	theme.BorderNormalColor = AdaptiveColor{
		Dark:  lipgloss.Color("#4b4c5c"), // From existing styles
		Light: lipgloss.Color("#BDBDBD"), // Light equivalent
	}
	theme.BorderFocusedColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Blue().Hex),
		Light: lipgloss.Color(latte.Blue().Hex),
	}
	theme.BorderDimColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Surface0().Hex),
		Light: lipgloss.Color(latte.Surface0().Hex),
	}

	// Diff view colors
	theme.DiffAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#478247"), // From existing diff.go
		Light: lipgloss.Color("#2E7D32"), // Light equivalent
	}
	theme.DiffRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#7C4444"), // From existing diff.go
		Light: lipgloss.Color("#C62828"), // Light equivalent
	}
	theme.DiffContextColor = AdaptiveColor{
		Dark:  lipgloss.Color("#a0a0a0"), // From existing diff.go
		Light: lipgloss.Color("#757575"), // Light equivalent
	}
	theme.DiffHunkHeaderColor = AdaptiveColor{
		Dark:  lipgloss.Color("#a0a0a0"), // From existing diff.go
		Light: lipgloss.Color("#757575"), // Light equivalent
	}
	theme.DiffHighlightAddedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#DAFADA"), // From existing diff.go
		Light: lipgloss.Color("#A5D6A7"), // Light equivalent
	}
	theme.DiffHighlightRemovedColor = AdaptiveColor{
		Dark:  lipgloss.Color("#FADADD"), // From existing diff.go
		Light: lipgloss.Color("#EF9A9A"), // Light equivalent
	}
	theme.DiffAddedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#303A30"), // From existing diff.go
		Light: lipgloss.Color("#E8F5E9"), // Light equivalent
	}
	theme.DiffRemovedBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#3A3030"), // From existing diff.go
		Light: lipgloss.Color("#FFEBEE"), // Light equivalent
	}
	theme.DiffContextBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#212121"), // From existing diff.go
		Light: lipgloss.Color("#F5F5F5"), // Light equivalent
	}
	theme.DiffLineNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color("#888888"), // From existing diff.go
		Light: lipgloss.Color("#9E9E9E"), // Light equivalent
	}
	theme.DiffAddedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#293229"), // From existing diff.go
		Light: lipgloss.Color("#C8E6C9"), // Light equivalent
	}
	theme.DiffRemovedLineNumberBgColor = AdaptiveColor{
		Dark:  lipgloss.Color("#332929"), // From existing diff.go
		Light: lipgloss.Color("#FFCDD2"), // Light equivalent
	}

	// Markdown colors
	theme.MarkdownTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Text().Hex),
		Light: lipgloss.Color(latte.Text().Hex),
	}
	theme.MarkdownHeadingColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Mauve().Hex),
		Light: lipgloss.Color(latte.Mauve().Hex),
	}
	theme.MarkdownLinkColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Sky().Hex),
		Light: lipgloss.Color(latte.Sky().Hex),
	}
	theme.MarkdownLinkTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Pink().Hex),
		Light: lipgloss.Color(latte.Pink().Hex),
	}
	theme.MarkdownCodeColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Green().Hex),
		Light: lipgloss.Color(latte.Green().Hex),
	}
	theme.MarkdownBlockQuoteColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Yellow().Hex),
		Light: lipgloss.Color(latte.Yellow().Hex),
	}
	theme.MarkdownEmphColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Yellow().Hex),
		Light: lipgloss.Color(latte.Yellow().Hex),
	}
	theme.MarkdownStrongColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Peach().Hex),
		Light: lipgloss.Color(latte.Peach().Hex),
	}
	theme.MarkdownHorizontalRuleColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Overlay0().Hex),
		Light: lipgloss.Color(latte.Overlay0().Hex),
	}
	theme.MarkdownListItemColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Blue().Hex),
		Light: lipgloss.Color(latte.Blue().Hex),
	}
	theme.MarkdownListEnumerationColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Sky().Hex),
		Light: lipgloss.Color(latte.Sky().Hex),
	}
	theme.MarkdownImageColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Sapphire().Hex),
		Light: lipgloss.Color(latte.Sapphire().Hex),
	}
	theme.MarkdownImageTextColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Pink().Hex),
		Light: lipgloss.Color(latte.Pink().Hex),
	}
	theme.MarkdownCodeBlockColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Text().Hex),
		Light: lipgloss.Color(latte.Text().Hex),
	}

	// Syntax highlighting colors
	theme.SyntaxCommentColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Overlay1().Hex),
		Light: lipgloss.Color(latte.Overlay1().Hex),
	}
	theme.SyntaxKeywordColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Pink().Hex),
		Light: lipgloss.Color(latte.Pink().Hex),
	}
	theme.SyntaxFunctionColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Green().Hex),
		Light: lipgloss.Color(latte.Green().Hex),
	}
	theme.SyntaxVariableColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Sky().Hex),
		Light: lipgloss.Color(latte.Sky().Hex),
	}
	theme.SyntaxStringColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Yellow().Hex),
		Light: lipgloss.Color(latte.Yellow().Hex),
	}
	theme.SyntaxNumberColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Teal().Hex),
		Light: lipgloss.Color(latte.Teal().Hex),
	}
	theme.SyntaxTypeColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Sky().Hex),
		Light: lipgloss.Color(latte.Sky().Hex),
	}
	theme.SyntaxOperatorColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Pink().Hex),
		Light: lipgloss.Color(latte.Pink().Hex),
	}
	theme.SyntaxPunctuationColor = AdaptiveColor{
		Dark:  lipgloss.Color(mocha.Text().Hex),
		Light: lipgloss.Color(latte.Text().Hex),
	}

	return theme
}

func init() {
	// Register the Catppuccin theme with the theme manager
	RegisterTheme("catppuccin", NewCatppuccinTheme())
}
