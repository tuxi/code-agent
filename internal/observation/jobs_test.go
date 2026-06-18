package observation

import (
	"strings"
	"testing"
)

func TestObserveJobStatusFailed(t *testing.T) {
	raw := `{"job_id":"job_1","command":"go test ./...","status":"failed","exit_code":1,"duration_ms":4200}`
	obs := Observe("job_status", raw)
	if obs.OK {
		t.Error("a failed background job must not be OK")
	}
	if obs.FailureType != FailureTest {
		t.Errorf("failure_type = %q, want test (from the go test command)", obs.FailureType)
	}
	if !strings.Contains(obs.Summary, "job_1") {
		t.Errorf("summary = %q, want it to name the job", obs.Summary)
	}
}

func TestObserveJobStatusRunningAndExited(t *testing.T) {
	running := Observe("job_status", `{"job_id":"job_2","command":"go test ./...","status":"running","exit_code":0}`)
	if !running.OK || running.FailureType != FailureNone {
		t.Errorf("running job: got OK=%v type=%q, want OK=true none", running.OK, running.FailureType)
	}

	exited := Observe("job_status", `{"job_id":"job_3","command":"go build ./...","status":"exited","exit_code":0}`)
	if !exited.OK || exited.FailureType != FailureNone {
		t.Errorf("exited job: got OK=%v type=%q, want OK=true none", exited.OK, exited.FailureType)
	}
}

func TestObserveJobStatusListFallsThrough(t *testing.T) {
	// A list of jobs is an overview, not a single result — generic OK, no crash.
	obs := Observe("job_status", `[{"job_id":"job_1","status":"failed"}]`)
	if !obs.OK {
		t.Errorf("a job list should be a generic OK observation, got %+v", obs)
	}
}

func TestObserveJobLogsFailedClassifiesLikeForeground(t *testing.T) {
	raw := "job job_1 [failed]\n--- FAIL: TestParse (0.00s)\n    parse_test.go:12: want 3 got 4\nFAIL\tcode-agent/internal/foo\t0.2s"
	obs := Observe("job_logs", raw)
	if obs.OK {
		t.Error("failed job logs must not be OK")
	}
	if obs.FailureType != FailureTest {
		t.Errorf("failure_type = %q, want test", obs.FailureType)
	}
	if len(obs.Salient) == 0 || !strings.Contains(strings.Join(obs.Salient, "\n"), "want 3 got 4") {
		t.Errorf("salient = %#v, want the assertion line", obs.Salient)
	}
}

func TestObserveJobLogsCompile(t *testing.T) {
	raw := "job job_2 [failed]\n# code-agent/internal/foo\ninternal/foo/x.go:42:13: undefined: Bar"
	obs := Observe("job_logs", raw)
	if obs.FailureType != FailureCompile {
		t.Errorf("failure_type = %q, want compile", obs.FailureType)
	}
}

func TestObserveJobLogsExitedIsOK(t *testing.T) {
	obs := Observe("job_logs", "job job_3 [exited]\nok  code-agent/internal/foo 0.2s")
	if !obs.OK || obs.FailureType != FailureNone {
		t.Errorf("exited logs: got OK=%v type=%q, want OK=true none", obs.OK, obs.FailureType)
	}
}
