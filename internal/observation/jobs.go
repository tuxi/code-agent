package observation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file extends Observe to the asynchronous world (P3.9.e). Without it, a
// failed background job is invisible: job_status returns {status:"failed"} and
// the loop would classify it as a generic OK, so Reflection and Verify-Fix never
// engage. Background tool results flow through the SAME Observe entry point and
// normalize into the SAME Observation schema as foreground run_command — there
// is no ObserveJobStatus/ObserveRunCommand fork at the call site.

// jobSnapshot mirrors the JSON job_status returns for a single job.
type jobSnapshot struct {
	JobID    string `json:"job_id"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
}

// observeJobStatus classifies a single job_status result. A failed job becomes a
// failure Observation, so a background failure is treated like a foreground one.
// The snapshot carries no output, so the failure_type is a best-effort guess
// from the command family; reading job_logs yields the precise classification.
func observeJobStatus(raw string) (Observation, bool) {
	raw = strings.TrimSpace(raw)
	// Only a single snapshot object; a list ("[...]") falls through to generic.
	if !strings.HasPrefix(raw, "{") || !strings.Contains(raw, `"status"`) || !strings.Contains(raw, `"job_id"`) {
		return Observation{}, false
	}
	var snap jobSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return Observation{}, false
	}

	obs := Observation{Tool: "job_status", FailureType: FailureNone}
	switch snap.Status {
	case "failed":
		obs.OK = false
		obs.FailureType = classifyByCommand(snap.Command)
		obs.Summary = fmt.Sprintf("background job %s failed (exit %d) — read job_logs for detail", snap.JobID, snap.ExitCode)
	case "running":
		obs.OK = true
		obs.Summary = "background job " + snap.JobID + " still running"
	case "canceled":
		obs.OK = true
		obs.Summary = "background job " + snap.JobID + " canceled"
	default: // exited (exit 0)
		obs.OK = true
	}
	return obs, true
}

// observeJobLogs classifies a job_logs result. The body is the command's real
// stdout/stderr, so a failed job's logs classify exactly like a foreground
// command — full failure_type + salient lines. This is what lets Reflection and
// Verify-Fix engage with background work.
func observeJobLogs(raw string) (Observation, bool) {
	if !strings.HasPrefix(raw, "job ") {
		return Observation{}, false
	}
	header, body := raw, ""
	if nl := strings.IndexByte(raw, '\n'); nl >= 0 {
		header, body = raw[:nl], raw[nl+1:]
	}

	obs := Observation{Tool: "job_logs", FailureType: FailureNone}
	if statusFromLogsHeader(header) != "failed" {
		obs.OK = true
		return obs, true
	}
	ft := classifyOutput(body)
	obs.OK = false
	obs.FailureType = ft
	obs.Salient = extractSalient(body, "")
	obs.Summary = summarizeJobFailure(ft)
	return obs, true
}

// statusFromLogsHeader pulls "failed" out of a "job job_1 [failed]" header.
func statusFromLogsHeader(header string) string {
	l := strings.LastIndexByte(header, '[')
	r := strings.LastIndexByte(header, ']')
	if l >= 0 && r > l {
		return header[l+1 : r]
	}
	return ""
}

// classifyByCommand guesses a failure type from the command alone, used for
// job_status where the snapshot carries no logs. It is a best-effort *hint*, not
// an authoritative classification: a "failed" status can also mean the process
// was killed (OOM), ran out of disk, or hit a permission error — none of which
// is really a test/compile failure. The precise classification comes from
// job_logs; a future FailureUnknown may fit the truly-ambiguous case better than
// FailureRuntime. (status != failure cause.)
func classifyByCommand(command string) FailureType {
	c := strings.ToLower(command)
	switch {
	case strings.Contains(c, "vet"), strings.Contains(c, "lint"):
		return FailureLint
	case strings.Contains(c, "test"):
		return FailureTest
	case strings.Contains(c, "build"), strings.Contains(c, "compile"):
		return FailureCompile
	default:
		return FailureRuntime
	}
}

// classifyOutput classifies failed output by marker alone (no command/exit
// context), for background job logs.
func classifyOutput(combined string) FailureType {
	low := strings.ToLower(combined)
	switch {
	case containsAny(low, compileMarkers):
		return FailureCompile
	case containsAny(low, testMarkers):
		return FailureTest
	case hasBuildHeader(combined):
		return FailureCompile
	default:
		return FailureRuntime
	}
}

func summarizeJobFailure(ft FailureType) string {
	switch ft {
	case FailureCompile:
		return "background build failed"
	case FailureTest:
		return "background tests failed"
	case FailureLint:
		return "background lint failed"
	default:
		return "background job failed"
	}
}
