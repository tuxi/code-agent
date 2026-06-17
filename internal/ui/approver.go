package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ConfirmApprover is the terminal implementation of the agent's Approver
// interface. Before a side-effecting tool runs, it shows the tool name and its
// arguments and asks the user to confirm.
type ConfirmApprover struct {
	// Prompt, when set, reads one line for the given prompt string. The REPL wires
	// this to its readline instance so the approver and the input loop share a
	// single owner of stdin — otherwise two readers would steal each other's
	// bytes. When nil, a fresh bufio reader on stdin is used (one-shot mode).
	Prompt func(prompt string) (string, error)
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

	line, err := a.readLine("Proceed? [y/N]: ")
	if err != nil {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func (a ConfirmApprover) readLine(prompt string) (string, error) {
	if a.Prompt != nil {
		return a.Prompt(prompt)
	}
	fmt.Print(prompt)
	return bufio.NewReader(os.Stdin).ReadString('\n')
}
