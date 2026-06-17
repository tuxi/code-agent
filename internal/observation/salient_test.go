package observation

import (
	"strings"
	"testing"
)

func TestExtractSalientCompile(t *testing.T) {
	stderr := `# code-agent/internal/foo
internal/foo/service.go:42:13: undefined: Bar
internal/foo/service.go:45:7: cannot use baz (variable of type int) as string value
internal/foo/repo.go:18:2: "fmt" imported and not used`

	salient := extractSalient("", stderr)
	if len(salient) != 3 {
		t.Fatalf("got %d salient lines, want 3: %#v", len(salient), salient)
	}
	if !strings.Contains(salient[0], "undefined: Bar") {
		t.Errorf("salient[0] = %q, want the undefined diagnostic", salient[0])
	}
	// The "# package" header is not itself a diagnostic and should be dropped.
	for _, l := range salient {
		if strings.HasPrefix(l, "#") {
			t.Errorf("salient kept a non-diagnostic header line: %q", l)
		}
	}
}

func TestExtractSalientTestFailure(t *testing.T) {
	stdout := `=== RUN   TestParse
--- FAIL: TestParse (0.00s)
    parse_test.go:12: want 3 got 4
=== RUN   TestLex
--- PASS: TestLex (0.00s)
FAIL
FAIL	code-agent/internal/foo	0.20s`

	salient := extractSalient(stdout, "")
	joined := strings.Join(salient, "\n")
	if !strings.Contains(joined, "--- FAIL: TestParse") {
		t.Errorf("expected the FAIL line in salient, got: %#v", salient)
	}
	if !strings.Contains(joined, "want 3 got 4") {
		t.Errorf("expected the assertion diagnostic in salient, got: %#v", salient)
	}
	// Noise like "=== RUN" / "--- PASS" must not be salient.
	if strings.Contains(joined, "=== RUN") || strings.Contains(joined, "--- PASS") {
		t.Errorf("salient kept passing/run noise: %#v", salient)
	}
}

func TestExtractSalientCaps(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("internal/foo.go:1:1: undefined: X\n")
	}
	salient := extractSalient("", b.String())
	if len(salient) > MaxSalientLines {
		t.Errorf("salient lines = %d, want <= %d", len(salient), MaxSalientLines)
	}

	long := "internal/foo.go:1:1: " + strings.Repeat("x", 5000)
	out := extractSalient("", long)
	if len(out) != 1 {
		t.Fatalf("got %d lines, want 1", len(out))
	}
	if len([]rune(out[0])) > MaxLineLength+1 { // +1 for the ellipsis rune
		t.Errorf("line length = %d runes, want <= %d", len([]rune(out[0])), MaxLineLength+1)
	}
}

func TestExtractSalientFallback(t *testing.T) {
	// No recognizable markers, but the command failed: keep the tail.
	stderr := "doing thing one\ndoing thing two\nsomething went sideways"
	salient := extractSalient("", stderr)
	if len(salient) == 0 {
		t.Fatal("expected a fallback to the last non-empty lines, got none")
	}
	if salient[len(salient)-1] != "something went sideways" {
		t.Errorf("fallback last line = %q, want the final stderr line", salient[len(salient)-1])
	}
}

func TestExtractSalientDropsAdjacentDuplicates(t *testing.T) {
	stderr := "foo.go:1:1: undefined: X\nfoo.go:1:1: undefined: X\nfoo.go:2:1: undefined: Y"
	salient := extractSalient("", stderr)
	if len(salient) != 2 {
		t.Errorf("got %d lines, want 2 (adjacent duplicate dropped): %#v", len(salient), salient)
	}
}
