// Package executionevidence checks recorded development-task evidence. It never
// schedules work, changes the agent loop, executes commands or reviews quality.
package executionevidence

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"code-agent/internal/reflection"
	"code-agent/internal/tools"
)

// Expectations come from the delegating caller, not from model-generated prose.
// Commands are compared exactly after trimming outer whitespace. They should be
// foreground, non-mutating checks; formatters belong before these checks.
type Expectations struct {
	ManualChecks       []string `json:"manual_checks,omitempty"`
	ValidationCommands []string `json:"validation_commands"`
}

type Event struct {
	Kind, CallID, ToolName string
	Input, Output          json.RawMessage
	Observation, Err       string
}

type Report struct {
	Status                    string   `json:"status"`
	Reasons                   []string `json:"reasons"`
	MissingValidationCommands []string `json:"missing_validation_commands"`
	UnfinishedCallIDs         []string `json:"unfinished_call_ids"`
	UnresolvedArgumentCallIDs []string `json:"unresolved_argument_call_ids"`
	LastMutationCallID        string   `json:"last_mutation_call_id,omitempty"`
}

type commandResult struct {
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
	Status   string `json:"status"`
	Decision string `json:"decision"`
	JobID    string `json:"job_id"`
}

type argumentFailure struct {
	call  Event
	field string
}

// Check consumes a complete, ordered exact-turn event stream. Missing results,
// missing expectations and ambiguous results stay incomplete. evidence_complete
// means only that execution evidence exists; independent task acceptance remains
// a separate decision. Only recorded tool mutations are observable here.
func Check(events []Event, expected Expectations, turnStatus string, pendingHuman bool) Report {
	r := Report{Status: "needs_validation", Reasons: []string{}, MissingValidationCommands: []string{}, UnfinishedCallIDs: []string{}, UnresolvedArgumentCallIDs: []string{}}
	checks := map[string]int{}
	for _, command := range expected.ValidationCommands {
		if command = strings.TrimSpace(command); command != "" {
			checks[command] = -1
		}
	}
	started := map[string]Event{}
	startIndex := map[string]int{}
	invalid := map[string]argumentFailure{}
	lastMutation := -1
	for i, event := range events {
		switch event.Kind {
		case "tool_started":
			started[event.CallID] = event
			startIndex[event.CallID] = i
		case "tool_finished":
			call, known := started[event.CallID]
			at := startIndex[event.CallID]
			delete(started, event.CallID)
			delete(startIndex, event.CallID)
			if !known || event.ToolName != call.ToolName {
				// A result without its call cannot prove a validation or mutation.
				r.Reasons = append(r.Reasons, "uncorrelated_tool_result")
				continue
			}
			var failure struct {
				Error *tools.ArgumentError `json:"error"`
			}
			_ = json.Unmarshal(event.Output, &failure)
			if failure.Error != nil && failure.Error.Code == "invalid_tool_arguments" {
				invalid[event.CallID] = argumentFailure{call: call, field: failure.Error.Field}
				continue
			}
			success := event.Err == "" && !strings.Contains(event.Observation, "The tool call was not approved.") &&
				!strings.Contains(event.Observation, "The tool call was blocked.") &&
				!strings.HasPrefix(event.Observation, "Tool error:") &&
				!strings.HasPrefix(event.Observation, "[observation] failure=")
			if success {
				for id, failed := range invalid {
					if corrects(failed, call) {
						delete(invalid, id)
					}
				}
			}
			if reflection.IsMutatingTool(event.ToolName) {
				// Even a failed mutation may have made a partial change.
				lastMutation, r.LastMutationCallID = i, event.CallID
			}
			if event.ToolName != "run_command" {
				continue
			}
			var input struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(call.Input, &input)
			command := strings.TrimSpace(input.Command)
			if _, wanted := checks[command]; !wanted {
				// Unknown shell commands may change source (e.g. gofmt). Conservatively
				// invalidate earlier checks instead of trying to parse shell programs.
				lastMutation, r.LastMutationCallID = i, event.CallID
				continue
			}
			checks[command] = -1 // a later failed/unknown rerun invalidates earlier success
			var result commandResult
			if json.Unmarshal(resultOutput(event), &result) == nil && success && result.ExitCode != nil && *result.ExitCode == 0 &&
				strings.TrimSpace(result.Command) == command && result.JobID == "" && completedCommand(result) {
				checks[command] = at
			}
		}
	}
	for command, at := range checks {
		if at <= lastMutation {
			r.MissingValidationCommands = append(r.MissingValidationCommands, command)
		}
	}
	for id := range started {
		r.UnfinishedCallIDs = append(r.UnfinishedCallIDs, id)
	}
	for id := range invalid {
		r.UnresolvedArgumentCallIDs = append(r.UnresolvedArgumentCallIDs, id)
	}
	sort.Strings(r.MissingValidationCommands)
	sort.Strings(r.UnfinishedCallIDs)
	sort.Strings(r.UnresolvedArgumentCallIDs)
	if len(checks) == 0 {
		r.Reasons = append(r.Reasons, "validation_not_declared")
	}
	if len(r.MissingValidationCommands) > 0 {
		r.Reasons = append(r.Reasons, "missing_successful_validation_after_last_mutation")
	}
	if len(r.UnfinishedCallIDs) > 0 {
		r.Reasons = append(r.Reasons, "unfinished_tool_calls")
	}
	if len(r.UnresolvedArgumentCallIDs) > 0 {
		r.Reasons = append(r.Reasons, "unresolved_tool_argument_errors")
	}
	// No trusted human-result input exists in this contract version. Machine
	// command success or assistant prose cannot satisfy a declared manual check.
	if len(expected.ManualChecks) > 0 {
		r.Reasons = append(r.Reasons, "manual_checks_unverified")
	}
	if pendingHuman {
		r.Status = "attention_required"
		r.Reasons = append(r.Reasons, "pending_human_control")
	} else if turnStatus == "running" || turnStatus == "queued" || turnStatus == "waiting" || turnStatus == "paused" {
		r.Status = "collecting"
	} else if turnStatus != "completed" {
		r.Reasons = append(r.Reasons, "turn_not_completed")
	} else if len(r.Reasons) == 0 {
		r.Status = "evidence_complete"
	}
	return r
}

// Decision records the policy classification, not the operator's answer. A
// correlated tool_finished with completed/exit 0 proves execution after the
// dispatcher's authorization gate. Merely seeing confirm never proves success.
func completedCommand(result commandResult) bool {
	if result.Decision == "confirm" {
		return result.Status == "completed"
	}
	return result.Decision == "allow" && (result.Status == "completed" || result.Status == "")
}

// Older local tools put their typed result JSON in Content rather than Output.
// Parse only the actual tool-result body, never a turn's assistant prose.
func resultOutput(event Event) json.RawMessage {
	if len(event.Output) > 0 && string(event.Output) != "null" {
		return event.Output
	}
	body := event.Observation
	if strings.HasPrefix(body, "[observation] ") {
		if _, content, ok := strings.Cut(body, "\n---\n"); ok {
			body = content
		}
	}
	return json.RawMessage(body)
}

// A later success only resolves an argument error when it is the same tool
// intent with the offending top-level field corrected. Unrelated calls cannot
// erase the error. Ambiguous JSON/nested errors remain unresolved.
func corrects(failed argumentFailure, call Event) bool {
	if failed.call.ToolName != call.ToolName || failed.field == "" || strings.Contains(failed.field, ".") {
		return false
	}
	var before, after map[string]any
	if json.Unmarshal(failed.call.Input, &before) != nil || json.Unmarshal(call.Input, &after) != nil || before == nil || after == nil {
		return false
	}
	for key, value := range before {
		if key != failed.field && !reflect.DeepEqual(value, after[key]) {
			return false
		}
	}
	_, corrected := after[failed.field]
	return corrected
}
