package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// ConfirmApprover is the terminal implementation of the agent's Approver
// interface. Before a side-effecting tool runs, it shows the tool name and its
// arguments and asks the user to confirm.
type ConfirmApprover struct {
	// Reader, when set, is shared with the REPL's input loop so both read from
	// the same buffered stdin. When nil, a fresh reader is used (one-shot mode).
	Reader *bufio.Reader
}

func (a ConfirmApprover) Approve(toolName string, input json.RawMessage) bool {
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

	r := a.Reader
	if r == nil {
		r = bufio.NewReader(os.Stdin)
	}
	return confirm(r, "Proceed?")
}
