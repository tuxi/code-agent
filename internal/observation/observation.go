// Package observation is the P4.1 data layer. It turns a raw tool result into a
// structured, machine-actionable Observation that the model reads to decide what
// to do next.
//
// It is pure and read-only: it classifies and distills, it never retries,
// re-runs, or sequences anything. "What happened" lives here; "what to do next"
// is the model's job. Keeping that line sharp is the whole point — see
// docs/p4.1-observation.md ("Guardrail"). This package therefore depends on
// nothing in internal/agent and contains no control flow.
package observation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Caps on the distilled output. Generous on purpose: a typical Go compile
// failure emits a dozen-plus diagnostics, and the goal is enough signal in one
// shot, not maximal compression. Tune down from telemetry later.
const (
	MaxSalientLines = 20
	MaxLineLength   = 300
)

// FailureType is a coarse, model-friendly classification of why a tool result is
// not OK. It is intentionally small; the model reads Salient lines for detail.
type FailureType string

const (
	FailureNone    FailureType = "none"    // exit 0 / no failure
	FailureCompile FailureType = "compile" // build / type / syntax error
	FailureTest    FailureType = "test"    // test assertions failed / panic
	FailureLint    FailureType = "lint"    // vet / linter findings
	FailureRuntime FailureType = "runtime" // nonzero exit, not one of the above
	FailureTimeout FailureType = "timeout" // killed by the deadline
	FailureBlocked FailureType = "blocked" // refused by CommandPolicy
)

// Observation is the enriched, machine-actionable view of a single tool result.
// Note the absence of any "suggestion" / "next step" field: that is deliberate
// (P4.1 describes; the model decides).
type Observation struct {
	Tool        string      `json:"tool"`
	OK          bool        `json:"ok"`
	FailureType FailureType `json:"failure_type"`
	Summary     string      `json:"summary"`           // one line, model- and human-readable
	Salient     []string    `json:"salient,omitempty"` // the lines that matter, capped
}

// Observe turns a raw tool observation into a structured Observation. It is the
// package entry point and is pure: same input → same output, no side effects.
//
// run_command and the background job_status / job_logs results are classified
// and distilled (so a failed background job is a real failure, not a generic
// OK). Any other tool gets a minimal Observation — OK unless the runtime marked
// the result a tool error.
func Observe(tool, raw string) Observation {
	switch tool {
	case "run_command":
		if res, ok := parseCommandResult(raw); ok {
			return observeCommand(res)
		}
	case "job_status":
		if obs, ok := observeJobStatus(raw); ok {
			return obs
		}
	case "job_logs":
		if obs, ok := observeJobLogs(raw); ok {
			return obs
		}
	}
	return observeGeneric(tool, raw)
}

// Render prepends a compact observation block before the raw tool result, so the
// distilled signal sits ahead of the (possibly truncated) raw output the model
// already receives. This is Context Engineering, not control flow — the model
// still decides what to do. Pure; the loop integration (P4.1.b) will call it.
func (o Observation) Render(raw string) string {
	var b strings.Builder
	b.WriteString("[observation] ")
	if o.OK {
		b.WriteString("ok")
	} else {
		fmt.Fprintf(&b, "failure=%s", o.FailureType)
	}
	if o.Summary != "" {
		fmt.Fprintf(&b, " summary=%q", o.Summary)
	}
	b.WriteByte('\n')
	for _, line := range o.Salient {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("---\n")
	b.WriteString(raw)
	return b.String()
}

// commandResult mirrors the JSON that run_command emits. It is a local copy on
// purpose: this package must not import internal/tools/shell (the dependency
// would point the wrong way, and the shell type is unexported anyway). Unknown
// fields such as duration_ms are ignored by the decoder.
type commandResult struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

// parseCommandResult recognizes and decodes a run_command JSON result. It gates
// on the *presence of at least two signature fields* (exit_code / command /
// decision) rather than a substring match, so it will not mistake some other
// tool's JSON — which might merely mention "exit_code" in a message — for a
// command result.
func parseCommandResult(raw string) (commandResult, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return commandResult{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return commandResult{}, false
	}
	signature := 0
	for _, k := range []string{"exit_code", "command", "decision"} {
		if _, ok := fields[k]; ok {
			signature++
		}
	}
	if signature < 2 {
		return commandResult{}, false
	}
	var res commandResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return commandResult{}, false
	}
	return res, true
}

// DefaultObserver delegates to Observe. It exists so callers (the agent runtime)
// can satisfy their Observer interface with a value, while the actual logic
// stays as a pure package function.
type DefaultObserver struct{}

// Observe implements the agent's Observer contract.
func (DefaultObserver) Observe(tool, rawObservation string) Observation {
	return Observe(tool, rawObservation)
}

func observeCommand(res commandResult) Observation {
	ft := classifyRunCommand(res)
	obs := Observation{
		Tool:        "run_command",
		OK:          ft == FailureNone,
		FailureType: ft,
	}
	if obs.OK {
		return obs // a success needs no distillation; the bare "ok" line is enough
	}
	obs.Salient = extractSalient(res.Stdout, res.Stderr)
	obs.Summary = summarize(ft, res, obs.Salient)
	return obs
}

// filesystemFailureMarkers are substrings that, when present in an edit_file /
// create_file / apply_patch observation, mark it as a recoverable failure — the
// tool returned a nil go error (so the model can adjust and retry), but from the
// human's perspective the action did not succeed. Matched case-sensitively: these
// are our own messages, not model output.
var filesystemFailureMarkers = []string{
	"Could not find the 'old' text",          // edit_file: text not in file
	"The 'old' text appears ",                // edit_file: ambiguous match (note trailing space: avoids matching the success message)
	"The 'old' text is empty",                // edit_file: no old string
	"The 'old' and 'new' text are identical", // edit_file: nothing to change
	"Could not open ",                        // edit_file: file not found
	"is a directory, not a file.",            // edit_file / create_file
	"is not a UTF-8 text file.",              // edit_file
	"File too large to edit",                 // edit_file
	"already exists. Use edit_file",          // create_file
	"Content too large:",                     // create_file
	"Patch failed",                           // apply_patch
	"Patch does not apply",                   // apply_patch
}

func observeGeneric(tool, raw string) Observation {
	raw = strings.TrimSpace(raw)
	// The agent loop prefixes a failed tool call's observation with "Tool error:".
	if strings.HasPrefix(raw, "Tool error:") {
		return Observation{
			Tool:        tool,
			OK:          false,
			FailureType: FailureRuntime,
			Summary:     firstLine(raw),
		}
	}
	// Filesystem tools return recoverable failures as observations (nil go error)
	// so the model can adjust and retry. Classify them as failures for the user:
	// a ✗ in the transcript, and the body shown without requiring expansion.
	if isFilesystemFailure(raw) {
		return Observation{
			Tool:        tool,
			OK:          false,
			FailureType: FailureRuntime,
			Summary:     firstLine(raw),
		}
	}
	return Observation{Tool: tool, OK: true, FailureType: FailureNone}
}

func isFilesystemFailure(raw string) bool {
	for _, m := range filesystemFailureMarkers {
		if strings.Contains(raw, m) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
