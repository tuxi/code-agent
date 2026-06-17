package reflection

import (
	"strings"
	"testing"
)

// Convenience builders for enriched observation strings (what the model saw).
func okObs() string { return "[observation] ok\n---\n{\"exit_code\":0}" }
func testFailObs() string {
	return "[observation] failure=test summary=\"1 test failed\"\n  --- FAIL: TestX\n---\n{\"exit_code\":1}"
}

func runCmd(cmd, obs string) StepView {
	return StepView{Tool: "run_command", Input: `{"command":"` + cmd + `"}`, Observation: obs}
}
func editFile(path string) StepView {
	return StepView{Tool: "edit_file", Input: `{"path":"` + path + `"}`, Observation: okObs()}
}

func TestReflectPaperOver(t *testing.T) {
	// The real transcript: test fails → edit the TEST → test passes.
	steps := []StepView{
		runCmd("go test ./...", testFailObs()),
		editFile("internal/app/config_test.go"),
		runCmd("go test ./...", okObs()),
	}
	rc := Reflect(steps)

	if !rc.TestEditedAfterFailure {
		t.Error("TestEditedAfterFailure = false, want true (edited a test after it failed)")
	}
	if rc.UnverifiedMutation {
		t.Error("UnverifiedMutation = true, want false (a verify ran after the edit)")
	}
	if len(rc.TestFilesMutated) != 1 || rc.TestFilesMutated[0] != "internal/app/config_test.go" {
		t.Errorf("TestFilesMutated = %v, want [internal/app/config_test.go]", rc.TestFilesMutated)
	}
	if rc.LastVerifyPassed == nil || !*rc.LastVerifyPassed {
		t.Errorf("LastVerifyPassed = %v, want &true", rc.LastVerifyPassed)
	}
	if !rc.Concerns() {
		t.Error("Concerns() = false, want true")
	}
}

func TestReflectCleanFix(t *testing.T) {
	// Test fails → edit the SOURCE → test passes. No concern.
	steps := []StepView{
		runCmd("go test ./...", testFailObs()),
		editFile("internal/app/config.go"),
		runCmd("go test ./...", okObs()),
	}
	rc := Reflect(steps)

	if rc.TestEditedAfterFailure {
		t.Error("TestEditedAfterFailure = true, want false (edited source, not test)")
	}
	if rc.UnverifiedMutation {
		t.Error("UnverifiedMutation = true, want false (verified after)")
	}
	if rc.Concerns() {
		t.Error("Concerns() = true, want false for a clean verified fix")
	}
}

func TestReflectUnverifiedMutation(t *testing.T) {
	// Edited code, never ran a build/test afterward.
	steps := []StepView{
		runCmd("go test ./...", testFailObs()),
		editFile("internal/app/config.go"),
	}
	rc := Reflect(steps)

	if !rc.UnverifiedMutation {
		t.Error("UnverifiedMutation = false, want true (no verify after the edit)")
	}
	if !rc.Concerns() {
		t.Error("Concerns() = false, want true")
	}
}

func TestReflectVerifiedMutationIsClean(t *testing.T) {
	steps := []StepView{
		editFile("internal/app/config.go"),
		runCmd("go build ./...", okObs()),
	}
	rc := Reflect(steps)
	if rc.UnverifiedMutation {
		t.Error("UnverifiedMutation = true, want false (build ran after the edit)")
	}
	if rc.Concerns() {
		t.Error("Concerns() = true, want false")
	}
}

func TestReflectReadOnlyTurnHasNoConcern(t *testing.T) {
	steps := []StepView{
		{Tool: "list_files", Input: `{"path":"."}`, Observation: okObs()},
		{Tool: "read_file", Input: `{"path":"go.mod"}`, Observation: "module x"},
		runCmd("go test ./...", okObs()),
	}
	rc := Reflect(steps)
	if rc.Concerns() {
		t.Errorf("Concerns() = true, want false for a read-only/verify-only turn: %+v", rc)
	}
}

func TestReflectApplyPatchTestFileAfterFailure(t *testing.T) {
	patch := "--- a/internal/app/config_test.go\n+++ b/internal/app/config_test.go\n@@ -1 +1 @@\n-x\n+y\n"
	steps := []StepView{
		runCmd("go test ./...", testFailObs()),
		{Tool: "apply_patch", Input: `{"patch":` + jsonString(patch) + `}`, Observation: okObs()},
		runCmd("go test ./...", okObs()),
	}
	rc := Reflect(steps)
	if !rc.TestEditedAfterFailure {
		t.Errorf("TestEditedAfterFailure = false, want true (apply_patch touched a _test.go after a failure): %+v", rc)
	}
}

func TestNudgeContainsOnlyTriggeredSignals(t *testing.T) {
	// No concern → empty nudge.
	if n := (ReflectionContext{}).Nudge(); n != "" {
		t.Errorf("Nudge with no concerns = %q, want empty", n)
	}

	// Only the test-edit signal.
	n := ReflectionContext{
		TestEditedAfterFailure: true,
		TestFilesMutated:       []string{"internal/app/config_test.go"},
	}.Nudge()
	if !strings.Contains(n, "edited a test file") || !strings.Contains(n, "config_test.go") {
		t.Errorf("nudge missing the test-edit line: %q", n)
	}
	if strings.Contains(n, "no build or test has confirmed") {
		t.Errorf("nudge included the unverified line when it should not: %q", n)
	}

	// Only the unverified signal.
	n = ReflectionContext{
		UnverifiedMutation: true,
		MutatedFiles:       []string{"internal/app/config.go"},
	}.Nudge()
	if !strings.Contains(n, "no build or test has confirmed") {
		t.Errorf("nudge missing the unverified line: %q", n)
	}
	if strings.Contains(n, "edited a test file") {
		t.Errorf("nudge included the test-edit line when it should not: %q", n)
	}
}

// jsonString quotes s as a JSON string literal for embedding in test input.
func jsonString(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
	return "\"" + r.Replace(s) + "\""
}
