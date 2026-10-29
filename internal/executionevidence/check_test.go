package executionevidence

import (
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
)

var expected = Expectations{ValidationCommands: []string{"go test ./internal/example", "go vet ./internal/example"}}

func call(id, tool, input string) []Event {
	return []Event{{Kind: "tool_started", CallID: id, ToolName: tool, Input: json.RawMessage(input)},
		{Kind: "tool_finished", CallID: id, ToolName: tool, Observation: "ok"}}
}

func check(id, command string, code int) []Event {
	input, _ := json.Marshal(map[string]string{"command": command})
	output, _ := json.Marshal(map[string]any{"command": command, "exit_code": code, "status": "completed", "decision": "allow"})
	es := call(id, "run_command", string(input))
	es[1].Output = output
	return es
}

func validated() []Event {
	es := call("edit", "edit_file", `{"path":"a.go","old":"a","new":"b"}`)
	es = append(es, check("test", expected.ValidationCommands[0], 0)...)
	return append(es, check("vet", expected.ValidationCommands[1], 0)...)
}

func TestEvidenceRequiresValidationAfterLastMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []Event
		want   string
	}{
		{"complete", validated(), "evidence_complete"},
		{"no checks", call("edit", "edit_file", `{"path":"a.go"}`), "needs_validation"},
		{"later edit", append(validated(), call("late", "edit_file", `{"path":"a.go"}`)...), "needs_validation"},
		{"later formatter", append(validated(), check("format", "gofmt -w a.go", 0)...), "needs_validation"},
		{"failed rerun", append(validated(), check("retest", expected.ValidationCommands[0], 1)...), "needs_validation"},
		{"unfinished", append(validated(), Event{Kind: "tool_started", CallID: "unfinished", ToolName: "read_file"}), "needs_validation"},
		{"prose only", []Event{{Kind: "turn_finished", Observation: "All tests passed"}}, "needs_validation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(tc.events, expected, "completed", false)
			if r.Status != tc.want {
				t.Fatalf("report=%+v", r)
			}
		})
	}
	r := Check(validated(), Expectations{}, "completed", false)
	if r.Status != "needs_validation" {
		t.Fatalf("missing declaration accepted: %+v", r)
	}
}

func TestEvidenceRejectsUnknownBackgroundAndOverlappingResults(t *testing.T) {
	for _, output := range []string{
		`{"command":"go test ./internal/example","status":"running","job_id":"j1"}`,
		`{"command":"go test ./internal/example","exit_code":0,"status":"timed_out","decision":"allow"}`,
		`{"command":"different","exit_code":0,"status":"completed","decision":"allow"}`,
		`{"command":"go test ./internal/example","exit_code":0,"status":"completed","decision":"deny"}`,
		`{"command":"go test ./internal/example","exit_code":0,"status":"running","job_id":"j1","decision":"allow"}`,
		`{}`,
	} {
		es := validated()
		es[3].Output = json.RawMessage(output)
		if r := Check(es, expected, "completed", false); r.Status != "needs_validation" {
			t.Fatalf("accepted unknown result %s: %+v", output, r)
		}
	}
	// A test started before a concurrent mutation ended cannot verify it.
	test := check("test", expected.ValidationCommands[0], 0)
	edit := call("edit", "edit_file", `{"path":"a.go"}`)
	es := []Event{test[0], edit[0], edit[1], test[1]}
	es = append(es, check("vet", expected.ValidationCommands[1], 0)...)
	if r := Check(es, expected, "completed", false); r.Status != "needs_validation" {
		t.Fatalf("overlap accepted: %+v", r)
	}
}

func TestEvidenceArgumentRetryMustMatchIntent(t *testing.T) {
	invalid := call("bad", "edit_file", `{"old":"a","new":"b"}`)
	err := &tools.ArgumentError{Code: "invalid_tool_arguments", Tool: "edit_file", Field: "path", Message: "required field is missing", RetryableByModel: true}
	invalid[1].Output = err.JSON()
	invalid[1].Err = err.Error()
	for _, tc := range []struct {
		input string
		want  string
	}{
		{`{"path":"a.go","old":"a","new":"b"}`, "evidence_complete"},
		{`{"path":"a.go","old":"different","new":"b"}`, "needs_validation"},
	} {
		es := append(append([]Event{}, invalid...), call("retry", "edit_file", tc.input)...)
		es = append(es, check("test", expected.ValidationCommands[0], 0)...)
		es = append(es, check("vet", expected.ValidationCommands[1], 0)...)
		if r := Check(es, expected, "completed", false); r.Status != tc.want {
			t.Fatalf("retry input %s: %+v", tc.input, r)
		}
	}
}

func TestEvidenceNeverEquatesAttentionOrTerminalFailureWithAcceptance(t *testing.T) {
	for _, tc := range []struct {
		status  string
		pending bool
		want    string
	}{
		{"completed", true, "attention_required"}, {"waiting_for_human", true, "attention_required"},
		{"running", false, "collecting"}, {"paused", false, "collecting"},
		{"failed", false, "needs_validation"}, {"cancelled", false, "needs_validation"},
	} {
		if r := Check(validated(), expected, tc.status, tc.pending); r.Status != tc.want {
			t.Fatalf("%s %+v", tc.status, r)
		}
	}
}

func TestConfirmedPolicyRequiresActualSuccessfulExecution(t *testing.T) {
	for _, tc := range []struct {
		name, status, observation, err string
		code                           *int
		want                           string
	}{
		{"executed", "completed", "ok", "", intPtr(0), "evidence_complete"},
		{"classification only", "", "ok", "", intPtr(0), "needs_validation"},
		{"missing exit", "completed", "ok", "", nil, "needs_validation"},
		{"nonzero", "completed", "ok", "", intPtr(1), "needs_validation"},
		{"timeout", "timed_out", "ok", "", intPtr(0), "needs_validation"},
		{"cancelled", "cancelled", "ok", "", intPtr(0), "needs_validation"},
		{"denied", "completed", "The tool call was not approved.", "", intPtr(0), "needs_validation"},
		{"tool error", "completed", "ok", "execution failed", intPtr(0), "needs_validation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := validated()
			es[3].Output, _ = json.Marshal(map[string]any{"command": expected.ValidationCommands[0], "decision": "confirm", "status": tc.status, "exit_code": tc.code})
			es[3].Observation, es[3].Err = tc.observation, tc.err
			if r := Check(es, expected, "completed", false); r.Status != tc.want {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func intPtr(v int) *int { return &v }
