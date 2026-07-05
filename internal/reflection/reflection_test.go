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

// P3.9.e: a test failure surfaced from a BACKGROUND job (a job_logs step
// enriched with failure=test) must arm TestEditedAfterFailure just like a
// foreground run_command failure does.
func TestReflectBackgroundTestFailure(t *testing.T) {
	jobLogsFail := StepView{
		Tool:        "job_logs",
		Input:       `{"job_id":"job_1"}`,
		Observation: "[observation] failure=test summary=\"background tests failed\"\n  --- FAIL: TestX\n---\njob job_1 [failed]\n--- FAIL: TestX",
	}
	steps := []StepView{
		runCmd("go test ./...", okObs()), // background launch returns "ok" (job started)
		jobLogsFail,                      // reading the logs reveals the failure
		editFile("internal/app/config_test.go"),
	}
	rc := Reflect(steps)
	if !rc.TestEditedAfterFailure {
		t.Errorf("TestEditedAfterFailure = false, want true (test edited after a background test failure): %+v", rc)
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

	// P4.3-R Move 2: the unverified signal no longer produces a nudge — the
	// runtime does not GUESS "unverified"; the loop's deterministic verify (or
	// silence) handles it. So an unverified-only context yields an empty nudge.
	n = ReflectionContext{
		UnverifiedMutation: true,
		MutatedFiles:       []string{"internal/app/config.go"},
		CodeFilesMutated:   []string{"internal/app/config.go"},
	}.Nudge()
	if n != "" {
		t.Errorf("unverified-only nudge = %q, want empty (Move 2 retired the guess)", n)
	}
}

// P4.3-R Move 1: a turn that writes ONLY doc/data files (no verifiable code) must
// not fire UnverifiedMutation — there is no build/test to run on a .md.
func TestReflectDocOnlyTurnHasNoConcern(t *testing.T) {
	createDoc := func(path string) StepView {
		return StepView{Tool: "create_file", Input: `{"path":"` + path + `"}`, Observation: okObs()}
	}
	steps := []StepView{
		createDoc("btc-reports/01-price-market.md"),
		createDoc("btc-reports/02-onchain.md"),
		createDoc("data/summary.json"),
	}
	rc := Reflect(steps)
	if rc.UnverifiedMutation {
		t.Errorf("UnverifiedMutation = true, want false for a doc/data-only turn: %+v", rc)
	}
	if len(rc.CodeFilesMutated) != 0 {
		t.Errorf("CodeFilesMutated = %v, want empty (no verifiable code)", rc.CodeFilesMutated)
	}
	if len(rc.MutatedFiles) != 3 {
		t.Errorf("MutatedFiles = %v, want the 3 doc/data files still recorded", rc.MutatedFiles)
	}
	if rc.Concerns() {
		t.Errorf("Concerns() = true, want false — nothing verifiable was written: %+v", rc)
	}
	if n := rc.Nudge(); n != "" {
		t.Errorf("Nudge = %q, want empty for a doc-only turn", n)
	}
}

// P4.3-R Move 1: a mixed turn (code + doc, no verify after) is still an
// unverified CODE change — UnverifiedMutation fires and CodeFilesMutated holds
// only the code file, never the doc. (Move 2 consumes CodeFilesMutated in the
// loop's verify re-prompt, so the doc exclusion stays load-bearing.)
func TestReflectMixedCodeAndDocIsCodeOnly(t *testing.T) {
	createDoc := StepView{Tool: "create_file", Input: `{"path":"docs/notes.md"}`, Observation: okObs()}
	steps := []StepView{
		editFile("internal/app/config.go"),
		createDoc,
	}
	rc := Reflect(steps)
	if !rc.UnverifiedMutation {
		t.Errorf("UnverifiedMutation = false, want true (code changed, no verify): %+v", rc)
	}
	if len(rc.CodeFilesMutated) != 1 || rc.CodeFilesMutated[0] != "internal/app/config.go" {
		t.Errorf("CodeFilesMutated = %v, want [internal/app/config.go] (doc excluded)", rc.CodeFilesMutated)
	}
	if len(rc.MutatedFiles) != 2 {
		t.Errorf("MutatedFiles = %v, want both files recorded", rc.MutatedFiles)
	}
}

// jsonString quotes s as a JSON string literal for embedding in test input.
func jsonString(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
	return "\"" + r.Replace(s) + "\""
}
