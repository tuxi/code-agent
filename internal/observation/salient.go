package observation

import (
	"fmt"
	"regexp"
	"strings"
)

// diagnosticPrefix matches a "path:line:" or "path:line:col:" compiler/tool
// diagnostic at the start of a line, e.g. "internal/foo/service.go:42:13:".
var diagnosticPrefix = regexp.MustCompile(`^\S+:\d+:(\d+:)?`)

// salientSubstrings are lower-cased fragments that mark a line as signal even
// when it is not a file:line diagnostic (test failures, panics, vet findings).
var salientSubstrings = []string{
	"error:", "--- fail", "panic:", "undefined:", "expected",
	"cannot", "fail", "warning:", "vet:",
}

// extractSalient distills the lines that carry signal from a command's output,
// capped at MaxSalientLines. stderr is scanned before stdout (build errors land
// on stderr; test failures on stdout), order is preserved, and adjacent repeats
// are dropped. When nothing matches but the command failed, it falls back to the
// last few non-empty lines — the shell convention that the error is at the end.
func extractSalient(stdout, stderr string) []string {
	var out []string
	prev := ""
	add := func(line string) {
		t := strings.TrimSpace(line)
		if t == "" || t == prev {
			return
		}
		out = append(out, truncateLine(t))
		prev = t
	}
	scan := func(text string) {
		for _, line := range strings.Split(text, "\n") {
			if len(out) >= MaxSalientLines {
				return
			}
			if isSalient(line) {
				add(line)
			}
		}
	}
	scan(stderr)
	scan(stdout)

	if len(out) == 0 {
		out = lastNonEmptyLines(firstNonEmpty(stderr, stdout), 5)
	}
	if len(out) > MaxSalientLines {
		out = out[:MaxSalientLines]
	}
	return out
}

func isSalient(line string) bool {
	l := strings.TrimSpace(line)
	if l == "" {
		return false
	}
	if diagnosticPrefix.MatchString(l) {
		return true
	}
	low := strings.ToLower(l)
	for _, s := range salientSubstrings {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// summarize builds the one-line, model- and human-readable summary for a failed
// result. Counts are best-effort and degrade to a plain phrase when zero.
func summarize(ft FailureType, res commandResult, salient []string) string {
	switch ft {
	case FailureCompile:
		if n := countDiagnostics(res.Stderr + "\n" + res.Stdout); n > 0 {
			return "build failed: " + plural(n, "error", "errors")
		}
		return "build failed"
	case FailureTest:
		if n := strings.Count(res.Stdout+res.Stderr, "--- FAIL"); n > 0 {
			return plural(n, "test", "tests") + " failed"
		}
		return "tests failed"
	case FailureLint:
		if len(salient) > 0 {
			return "lint: " + plural(len(salient), "finding", "findings")
		}
		return "lint findings"
	case FailureTimeout:
		return "command timed out"
	case FailureBlocked:
		return "refused by policy"
	case FailureRuntime:
		return fmt.Sprintf("command failed (exit %d)", res.ExitCode)
	default:
		return ""
	}
}

func countDiagnostics(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if diagnosticPrefix.MatchString(strings.TrimSpace(line)) {
			n++
		}
	}
	return n
}

func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

// truncateLine caps a line at MaxLineLength runes (rune-aware so a multibyte
// character is never split), appending an ellipsis when cut.
func truncateLine(s string) string {
	if len(s) <= MaxLineLength {
		return s
	}
	r := []rune(s)
	if len(r) <= MaxLineLength {
		return s
	}
	return string(r[:MaxLineLength]) + "…"
}

func lastNonEmptyLines(text string, n int) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			lines = append(lines, truncateLine(t))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
