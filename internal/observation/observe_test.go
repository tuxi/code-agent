package observation

import (
	"strings"
	"testing"
)

func TestObserveRunCommandSuccess(t *testing.T) {
	raw := `{"command":"go test ./...","stdout":"ok  code-agent/internal/foo 0.2s","exit_code":0,"duration_ms":210,"decision":"allow"}`
	obs := Observe("run_command", raw)
	if !obs.OK || obs.FailureType != FailureNone {
		t.Errorf("ok result: got OK=%v type=%q, want OK=true none", obs.OK, obs.FailureType)
	}
	if len(obs.Salient) != 0 {
		t.Errorf("success should have no salient lines, got %#v", obs.Salient)
	}
}

func TestObserveRunCommandCompile(t *testing.T) {
	raw := `{"command":"go build ./...","stdout":"","stderr":"# code-agent/internal/foo\ninternal/foo/service.go:42:13: undefined: Bar","exit_code":1,"duration_ms":120,"decision":"allow"}`
	obs := Observe("run_command", raw)

	if obs.OK {
		t.Error("compile failure should not be OK")
	}
	if obs.FailureType != FailureCompile {
		t.Errorf("failure_type = %q, want compile", obs.FailureType)
	}
	if !strings.Contains(obs.Summary, "build failed") {
		t.Errorf("summary = %q, want it to mention build failed", obs.Summary)
	}
	if len(obs.Salient) == 0 || !strings.Contains(obs.Salient[0], "undefined: Bar") {
		t.Errorf("salient = %#v, want the undefined diagnostic", obs.Salient)
	}
}

func TestObserveNonCommandTool(t *testing.T) {
	// A read-only tool's plain output: minimal, OK observation.
	obs := Observe("grep", "internal/foo.go:3: something")
	if !obs.OK || obs.FailureType != FailureNone {
		t.Errorf("grep output: got OK=%v type=%q, want OK=true none", obs.OK, obs.FailureType)
	}

	// The loop's "Tool error:" prefix is surfaced as a runtime failure.
	bad := Observe("apply_patch", "Tool error: patch did not apply\nextra detail")
	if bad.OK || bad.FailureType != FailureRuntime {
		t.Errorf("tool error: got OK=%v type=%q, want OK=false runtime", bad.OK, bad.FailureType)
	}
	if bad.Summary != "Tool error: patch did not apply" {
		t.Errorf("summary = %q, want the first line of the error", bad.Summary)
	}
}

func TestObserveMalformedJSONFallsBackToGeneric(t *testing.T) {
	// Not valid run_command JSON → treated as a generic (OK) observation, not a crash.
	obs := Observe("run_command", "not json at all")
	if !obs.OK {
		t.Errorf("malformed result should fall back to OK generic, got %#v", obs)
	}
}

func TestRenderPrependsSummaryBlock(t *testing.T) {
	raw := `{"command":"go build ./...","stderr":"internal/foo.go:42:13: undefined: Bar","exit_code":1,"decision":"allow"}`
	obs := Observe("run_command", raw)
	rendered := obs.Render(raw)

	// The observation block must come first, before the raw result.
	if !strings.HasPrefix(rendered, "[observation] failure=compile") {
		t.Errorf("render did not start with the observation header:\n%s", rendered)
	}
	idxBlock := strings.Index(rendered, "[observation]")
	idxSep := strings.Index(rendered, "\n---\n")
	idxRaw := strings.Index(rendered, "{\"command\"")
	if !(idxBlock < idxSep && idxSep < idxRaw) {
		t.Errorf("expected order: header < separator < raw; got %d, %d, %d", idxBlock, idxSep, idxRaw)
	}
	if !strings.Contains(rendered, "undefined: Bar") {
		t.Errorf("rendered output should surface the salient diagnostic:\n%s", rendered)
	}
}

func TestRenderOK(t *testing.T) {
	obs := Observation{Tool: "run_command", OK: true, FailureType: FailureNone}
	rendered := obs.Render("raw output here")
	if !strings.HasPrefix(rendered, "[observation] ok") {
		t.Errorf("ok render should start with '[observation] ok', got:\n%s", rendered)
	}
}
