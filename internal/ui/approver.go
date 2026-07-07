package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"code-agent/internal/agent"
)

// AlwaysAllower persists an "always allow" decision so future matching calls are
// auto-approved without a prompt. Kept as a narrow local interface (rather than
// importing the approve package) so ui stays dependency-light; *approve.RuleStore
// satisfies it, persisting at the project-local scope and returning the rule it
// added (e.g. "mcp__github__*") for display.
type AlwaysAllower interface {
	AllowAlways(toolName string) (rule string, err error)
}

// ConfirmApprover is the terminal implementation of the agent's Approver
// interface. Before a side-effecting tool runs, it shows the tool name and its
// arguments and asks the user to confirm.
type ConfirmApprover struct {
	// Prompt, when set, reads one line for the given prompt string. The REPL wires
	// this to its readline instance so the approver and the input loop share a
	// single owner of stdin — otherwise two readers would steal each other's
	// bytes. When nil, a fresh bufio reader on stdin is used (one-shot mode).
	Prompt func(prompt string) (string, error)

	// Granter, when set, enables the "always allow" choice: picking it persists a
	// rule (via AllowAlways) so the tool — or, for MCP, its whole server — is
	// auto-approved for the rest of this session and future ones. Nil offers only
	// once/deny (e.g. non-interactive runs with no readline).
	Granter AlwaysAllower
}

func (a ConfirmApprover) Approve(toolName string, input json.RawMessage) agent.Verdict {
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

	prompt := "Proceed? [y]es once / [N]o: "
	if a.Granter != nil {
		prompt = "Proceed? [y]es once / [a]lways / [N]o: "
	}
	line, err := a.readLine(prompt)
	if err != nil {
		return agent.VerdictDeny
	}
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes", "o", "once":
		return agent.VerdictAllow
	case "a", "always":
		if a.Granter == nil {
			return agent.VerdictAllow // no persistence available; treat as a one-time yes
		}
		if rule, err := a.Granter.AllowAlways(toolName); err != nil {
			fmt.Printf("  (could not persist always-allow, allowing once: %v)\n", err)
		} else {
			fmt.Printf("  ✓ always allowing %q (project-local)\n", rule)
		}
		return agent.VerdictAllow
	default:
		return agent.VerdictDeny
	}
}

func (a ConfirmApprover) readLine(prompt string) (string, error) {
	if a.Prompt != nil {
		return a.Prompt(prompt)
	}
	fmt.Print(prompt)
	return bufio.NewReader(os.Stdin).ReadString('\n')
}
