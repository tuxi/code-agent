package observation

import "strings"

// Failure markers, matched case-insensitively against the combined output. These
// are Go-toolchain-first by design (~95% of today's commands); cargo / swift /
// pytest markers get added here when those toolchains actually land, not before.
var (
	compileMarkers = []string{
		"undefined:", "cannot use", "syntax error", "expected ",
		"not enough arguments", "too many arguments", "undeclared name",
		"imported and not used", "declared and not used", "missing return",
		"is not a type", "cannot find package", "cannot convert",
		"undefined reference", "redeclared", "[build failed]",
	}
	testMarkers = []string{
		"--- fail", "panic:", "fail\t", "test failed",
	}
)

// classifyRunCommand maps a run_command result to a FailureType. Precedence is
// deliberate: policy/timeout first, then success, then the most specific
// failure. Compile is checked before test so that a `go test` which fails to
// *compile* is reported as a compile failure — that is the fix the model needs.
func classifyRunCommand(res commandResult) FailureType {
	if res.Decision == "block" {
		return FailureBlocked
	}
	if isTimeout(res.Note) {
		return FailureTimeout
	}
	if res.ExitCode == 0 {
		return FailureNone
	}

	combined := res.Stderr + "\n" + res.Stdout
	low := strings.ToLower(combined)

	// Precedence matters. A hard compile marker (undefined:, [build failed], …)
	// wins outright, even under `go test` or `go vet`, because the fix is a
	// compile fix. Only then does the *command* disambiguate the "# package"
	// header that both `go build` and `go vet` print: a lint command without a
	// hard compile marker is a lint finding, not a build error.
	switch {
	case containsAny(low, compileMarkers):
		return FailureCompile
	case isLintCommand(res.Command):
		return FailureLint
	case containsAny(low, testMarkers):
		return FailureTest
	case hasBuildHeader(combined):
		return FailureCompile
	default:
		return FailureRuntime
	}
}

func isTimeout(note string) bool {
	n := strings.ToLower(note)
	return strings.Contains(n, "timed out") || strings.Contains(n, "timeout")
}

// hasBuildHeader detects the "# package/path" line `go build` prints above a
// failing package's diagnostics.
func hasBuildHeader(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			return true
		}
	}
	return false
}

func isLintCommand(command string) bool {
	c := strings.TrimSpace(command)
	return strings.HasPrefix(c, "go vet") ||
		strings.Contains(c, "golangci-lint") ||
		strings.Contains(c, "staticcheck")
}

// containsAny reports whether low (already lower-cased) contains any marker.
// Markers are authored lower-case, so no per-call allocation is needed.
func containsAny(low string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
