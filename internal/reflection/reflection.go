// Package reflection is the P4.3 data layer. It looks at a turn's completed
// steps and extracts factual signals about the work's quality — did a fix only
// edit a test after that test failed? did code change without any verification
// afterward? — into a ReflectionContext. From that it builds an ephemeral
// self-check nudge for the model.
//
// Like observation (P4.1), it is pure and read-only: it states facts and asks
// questions; it never decides what the agent does next. The model owns control
// flow. The nudge it produces is the mirror image of the loop's existing
// convergence nudge — fired at the finish line instead of the over-work line.
// See docs/p4.3-reflection.md.
package reflection

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// StepView is the neutral, minimal view of one completed tool step that
// reflection needs. The agent adapter (P4.3.b) converts its richer Step into
// this so the reflection package never imports agent — keeping the dependency
// one-way and this logic testable in isolation.
type StepView struct {
	Tool        string // tool name, e.g. "run_command", "edit_file", "apply_patch"
	Input       string // raw tool input JSON (the call's arguments)
	Observation string // the (P4.1-enriched) observation string the model saw
}

// ReflectionContext is the factual, machine-extracted state of a turn's work.
// Pure data — the raw material for the self-check nudge, no judgments. P4.3.a
// ships the two highest-value signals; more can follow from telemetry.
type ReflectionContext struct {
	MutatedFiles           []string // files changed via edit_file / apply_patch this turn
	TestFilesMutated       []string // subset of MutatedFiles ending in _test.go
	LastVerifyPassed       *bool    // result of the turn's last build/test (nil = unknown/none)
	TestEditedAfterFailure bool     // a test file was edited after a test failed
	UnverifiedMutation     bool     // code changed, but no build/test ran afterward
}

// Concerns reports whether anything is worth asking the model about. When false,
// the loop should accept the final answer immediately — no nudge, no extra call.
func (c ReflectionContext) Concerns() bool {
	return c.TestEditedAfterFailure || c.UnverifiedMutation
}

// Reflect extracts a ReflectionContext from a turn's steps, in order. It reads
// P4.1's "[observation]" markers to tell a passing verify from a failing one;
// verify *commands* are recognized from the call arguments alone, so
// "did any verification run" holds even if enrichment is absent.
func Reflect(steps []StepView) ReflectionContext {
	var rc ReflectionContext
	lastMutationIdx, lastVerifyIdx, lastTestFailIdx := -1, -1, -1

	for i, s := range steps {
		// Mutations.
		if paths := mutatedPaths(s.Tool, s.Input); len(paths) > 0 {
			lastMutationIdx = i
			for _, p := range paths {
				rc.MutatedFiles = appendUnique(rc.MutatedFiles, p)
				if isTestFile(p) {
					rc.TestFilesMutated = appendUnique(rc.TestFilesMutated, p)
					if lastTestFailIdx >= 0 && lastTestFailIdx < i {
						rc.TestEditedAfterFailure = true
					}
				}
			}
		}

		// A test failure can surface from a foreground run_command OR from reading
		// a background job's logs/status (P3.9.e) — detect it from the observation
		// marker regardless of which tool produced it.
		if known, _, failure := parseObsMarker(s.Observation); known && failure == "test" {
			lastTestFailIdx = i
		}

		// A verify *command* is a run_command launch (foreground or background).
		if s.Tool == "run_command" && isVerifyCommand(parseCommand(s.Input)) {
			lastVerifyIdx = i
			if known, ok, _ := parseObsMarker(s.Observation); known {
				rc.LastVerifyPassed = &ok
			} else {
				rc.LastVerifyPassed = nil
			}
		}
	}

	// A mutation with no verify after it (regardless of pass/fail) is unverified.
	if lastMutationIdx >= 0 && lastVerifyIdx < lastMutationIdx {
		rc.UnverifiedMutation = true
	}
	return rc
}

// mutatedPaths returns the workspace files a mutating tool call touches.
func mutatedPaths(tool, input string) []string {
	switch tool {
	case "edit_file", "create_file", "write_file":
		var in struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal([]byte(input), &in)
		if p := strings.TrimSpace(in.Path); p != "" {
			return []string{filepath.ToSlash(p)}
		}
	case "apply_patch":
		var in struct {
			Patch string `json:"patch"`
		}
		_ = json.Unmarshal([]byte(input), &in)
		return patchFiles(in.Patch)
	}
	return nil
}

// patchFiles pulls the target paths out of a unified diff's "+++ b/<path>"
// headers, ignoring /dev/null (a deletion's target).
func patchFiles(patch string) []string {
	var files []string
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+++ ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
		p = strings.TrimPrefix(p, "b/")
		if p != "" && p != "/dev/null" {
			files = appendUnique(files, filepath.ToSlash(p))
		}
	}
	return files
}

func parseCommand(input string) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal([]byte(input), &in)
	return strings.TrimSpace(in.Command)
}

// isVerifyCommand reports whether a command is a build/test/vet that confirms a
// change. Go-first (matching the toolchain support that actually exists), with a
// few common cousins.
func isVerifyCommand(cmd string) bool {
	for _, p := range []string{
		"go build", "go test", "go vet", "go run",
		"cargo build", "cargo test", "cargo check", "swift build", "xcodebuild",
	} {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}

func isTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

// parseObsMarker reads P4.1's prepended "[observation] ok|failure=<type>" line.
// known is false when no marker is present (e.g. enrichment disabled), so callers
// can stay conservative rather than guess.
func parseObsMarker(obs string) (known, ok bool, failure string) {
	const prefix = "[observation] "
	s := strings.TrimSpace(obs)
	if !strings.HasPrefix(s, prefix) {
		return false, false, ""
	}
	rest := s[len(prefix):]
	switch {
	case strings.HasPrefix(rest, "ok"):
		return true, true, ""
	case strings.HasPrefix(rest, "failure="):
		f := rest[len("failure="):]
		if i := strings.IndexAny(f, " \n"); i >= 0 {
			f = f[:i]
		}
		return true, false, f
	default:
		return true, false, ""
	}
}

func appendUnique(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}
