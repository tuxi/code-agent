package tui

import (
	"strings"
	"testing"

	chAnsi "github.com/charmbracelet/x/ansi"
)

// TestFirstFrameRendersBeforeWindowSize guards the pre-size fix. Bubble Tea
// renders before it delivers the initial WindowSizeMsg, so that first view must
// already contain the UI.
func TestFirstFrameRendersBeforeWindowSize(t *testing.T) {
	m := newTestModel()
	v := chAnsi.Strip(m.View().Content)
	if len(v) == 0 {
		t.Fatal("first frame before WindowSizeMsg must not be empty")
	}
	// The full layout chrome should be present: composer prompt and help.
	for _, want := range []string{"> ", "help"} {
		if !strings.Contains(v, want) {
			t.Fatalf("first frame missing %q:\n%q", want, v)
		}
	}
}
