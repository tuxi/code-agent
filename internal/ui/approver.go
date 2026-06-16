package ui

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ConfirmApprover is the terminal implementation of the agent's Approver
// interface. Before a side-effecting tool runs, it shows the tool name and its
// arguments (with real newlines, so a patch or command reads naturally) and
// asks the user to confirm.
//
// It satisfies agent.Approver structurally; it does not import the agent
// package, which keeps the dependency direction clean.
type ConfirmApprover struct{}

func (ConfirmApprover) Approve(toolName string, input json.RawMessage) bool {
	fmt.Printf("\nThe agent wants to run a side-effecting tool: %s\n", toolName)

	var fields map[string]any
	if err := json.Unmarshal(input, &fields); err == nil && len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\n  %s:\n%v\n", k, fields[k])
		}
	} else if len(input) > 0 {
		fmt.Printf("  input: %s\n", string(input))
	}

	return Confirm("Proceed?")
}
