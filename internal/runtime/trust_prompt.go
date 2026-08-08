package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"code-agent/internal/agent"
	"golang.org/x/term"
)

// TerminalTrustPolicy prompts the user on stdin to decide whether to trust a
// project directory. It is the default interactive trust resolver for the CLI
// (TUI and REPL modes).
//
// The prompt mimics Claude Code's "Quick safety check" and persists the
// decision to the trust store so the user is not asked again.
type TerminalTrustPolicy struct {
	Store *TrustStore
	Out   io.Writer // where to print the prompt (default: os.Stderr)
	In    io.Reader // where to read the answer (default: os.Stdin)
}

// isTerminal reports whether f is likely an interactive terminal.
func isTerminal(f interface{}) bool {
	type fd interface{ Fd() uintptr }
	if fder, ok := f.(fd); ok {
		return term.IsTerminal(int(fder.Fd()))
	}
	return false
}

// ResolveTrust implements agent.TrustPolicy.
// It only prompts when stdin is a terminal; non-interactive sessions are
// denied (fail-closed) unless overridden by a CLI flag.
func (p *TerminalTrustPolicy) ResolveTrust(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
	var out io.Writer = os.Stderr
	if p.Out != nil {
		out = p.Out
	}
	var in io.Reader = os.Stdin
	if p.In != nil {
		in = p.In
	}

	// Non-interactive: undecided, so the chain falls through to
	// fail-closed with a clear message. Use --trust to bypass.
	if !isTerminal(in) {
		return agent.TrustUndecided, nil
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Accessing workspace:")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s\n", cwd)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Quick safety check: Is this a project you created or one you trust?")
	fmt.Fprintln(out, "(Like your own code, a well-known open source project, or work from your team).")
	fmt.Fprintln(out, "If not, take a moment to review what's in this folder first.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Code Agent will be able to read, edit, and execute files here.")
	fmt.Fprintln(out)

	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Trust this folder? [y/N]: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return agent.TrustDenied, nil
		}
		line = strings.TrimSpace(strings.ToLower(line))
		switch line {
		case "y", "yes":
			if p.Store != nil {
				_ = p.Store.Store(cwd, true)
			}
			return agent.TrustAllowed, nil
		case "n", "no", "":
			if p.Store != nil {
				_ = p.Store.Store(cwd, false)
			}
			return agent.TrustDenied, nil
		default:
			fmt.Fprintln(out, "Please answer 'y' or 'n'.")
		}
	}
}

// TrustPolicyFunc adapts a function to the agent.TrustPolicy interface.
type TrustPolicyFunc func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error)

// ResolveTrust implements agent.TrustPolicy.
func (f TrustPolicyFunc) ResolveTrust(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
	return f(ctx, cwd, hasResources)
}

// TrustAlways is a TrustPolicy that always grants trust. It is appropriate for
// headless/daemon/server modes where the operator explicitly launched the process
// from their own project directory.
var TrustAlways agent.TrustPolicy = TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
	return agent.TrustAllowed, nil
})
