package sandbox

import (
	"fmt"
	"strings"
)

// shellOperators are metacharacters that only a shell interprets. && and ; are
// now supported via sh -c execution with per-subcommand classification (Phase A);
// the remaining operators (pipe, single &, redirect, command substitution) are
// still rejected — they require further safety work (Phases B/C).
//
// NOTE: "&" (single ampersand, backgrounding) remains rejected. It is listed
// before "&&" so the longer match in ContainsChainOperators takes priority.
var shellOperators = []string{"|", "&", ">", "<", "$(", "`", "\n"}

// ContainsShellOperators reports whether the command *structure* uses shell
// control operators that are NOT supported by Phase A. Chain operators (&& and ;)
// are excluded from this check — they are handled by classifyChain and sh -c.
func ContainsShellOperators(command string) bool {
	structure := unquotedStructure(command)
	for _, op := range shellOperators {
		if strings.Contains(structure, op) {
			// "&" matches both "&&" and "&". Skip the match when it's part of "&&".
			if op == "&" && strings.Contains(structure, "&&") {
				// Only flag "&" when it's NOT part of "&&".
				// Remove all "&&" occurrences and check for remaining "&".
				cleaned := strings.ReplaceAll(structure, "&&", "")
				if !strings.Contains(cleaned, "&") {
					continue
				}
			}
			return true
		}
	}
	return false
}

// chainOperators are the subset of shell operators that Phase A supports:
// && (conditional and) and ; (sequential separator). These trigger sh -c
// execution with per-subcommand safety classification.
var chainOperators = []string{"&&", ";"}

// ContainsShellOperators reports whether the command *structure* uses shell
// control operators (pipes, redirection, command substitution, chaining).
//
// unquotedStructure returns the command with the contents of every single- or
// double-quoted span removed, leaving only the command's structural skeleton
// (program name, flags, unquoted arguments, and any real shell operators).
//
// This is what makes policy checks ignore data the user put inside quotes: a
// commit message that merely mentions "rm -rf /" or embeds a newline is text,
// not a command to run. The full, unmasked command is still shown to the user
// at the confirmation prompt, so nothing is hidden — only the false positives
// are removed. Unterminated quotes mask to end-of-string, which is the safe
// (more-masked) direction.
func unquotedStructure(command string) string {
	var b strings.Builder
	inSingle, inDouble := false, false
	for _, r := range command {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			}
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SplitArgs splits a command line into argv. It understands single and double
// quotes so that arguments with spaces survive intact (commit messages, paths),
// but it deliberately does not implement full shell parsing — no escapes, no
// variable expansion, no globbing. Those are a non-goal: run_command does not
// invoke a shell, and the policy classifies the leading tokens.
func SplitArgs(command string) ([]string, error) {
	var (
		args     []string
		cur      strings.Builder
		inSingle bool
		inDouble bool
		hasToken bool
	)
	for _, r := range command {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			} else {
				cur.WriteRune(r)
			}
		case r == '\'':
			inSingle = true
			hasToken = true
		case r == '"':
			inDouble = true
			hasToken = true
		case r == ' ' || r == '\t':
			if hasToken {
				args = append(args, cur.String())
				cur.Reset()
				hasToken = false
			}
		default:
			cur.WriteRune(r)
			hasToken = true
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote in command")
	}
	if hasToken {
		args = append(args, cur.String())
	}
	return args, nil
}

// longestPrefixMatch returns the longest prefix from prefixes that matches the
// command on a word boundary, or "" if none does. "git status" matches
// "git status" and "git status --short" but not "git stat" or "git status-x".
func longestPrefixMatch(command string, prefixes []string) string {
	best := ""
	for _, p := range prefixes {
		if matchesPrefix(command, p) && len(p) > len(best) {
			best = p
		}
	}
	return best
}

func matchesPrefix(command, prefix string) bool {
	if command == prefix {
		return true
	}
	return strings.HasPrefix(command, prefix+" ")
}

// ContainsChainOperators reports whether the command structure uses && or ;
// (outside quotes). These are safe to execute via sh -c with per-subcommand
// safety classification — unlike pipes and redirects which are still rejected.
func ContainsChainOperators(command string) bool {
	structure := unquotedStructure(command)
	for _, op := range chainOperators {
		if strings.Contains(structure, op) {
			return true
		}
	}
	return false
}

// splitByOperators splits a command on && and ; outside of quoted spans. It
// returns the trimmed subcommands in order and the operators between them.
// For example:
//
//	"go build && go test"       → ["go build", "go test"], ["&&"]
//	"git add .; git commit -m wip" → ["git add .", "git commit -m wip"], [";"]
//
// Operators inside quotes are treated as literal text and do NOT split.
// Trailing operators are ignored.
func splitByOperators(command string) (subcommands []string, operators []string) {
	cmd := strings.TrimSpace(command)

	// Find all split points by walking the original command, tracking quote
	// state. Operators inside quotes are NOT split points.
	type splitPoint struct {
		pos int // byte position in cmd
		op  string
	}
	var points []splitPoint

	// Walk the original command, tracking quote state and looking for operators.
	inSingle, inDouble := false, false
	i := 0
	for i < len(cmd) {
		r := rune(cmd[i])
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			}
			i++
		case inDouble:
			if r == '"' {
				inDouble = false
			}
			i++
		case r == '\'':
			inSingle = true
			i++
		case r == '"':
			inDouble = true
			i++
		case strings.HasPrefix(cmd[i:], "&&"):
			points = append(points, splitPoint{pos: i, op: "&&"})
			i += 2
		case r == ';':
			points = append(points, splitPoint{pos: i, op: ";"})
			i++
		default:
			i++
		}
	}

	if len(points) == 0 {
		return []string{cmd}, nil
	}

	// Split the command at each point.
	start := 0
	for _, p := range points {
		sub := strings.TrimSpace(cmd[start:p.pos])
		if sub != "" {
			subcommands = append(subcommands, sub)
			operators = append(operators, p.op)
		}
		start = p.pos + len(p.op)
	}
	// Last subcommand after the final operator.
	last := strings.TrimSpace(cmd[start:])
	if last != "" {
		subcommands = append(subcommands, last)
	}

	return subcommands, operators
}

// classifyChain classifies a compound command by splitting on && / ; and
// independently classifying each subcommand. The verdict is the strictest
// across all subcommands: any Block → Block, any Confirm → Confirm,
// all Allow → Allow. If splitting fails (unterminated quote etc.), it falls
// back to classifying the whole command as a single unit.
func (p CommandPolicy) classifyChain(command string) Classification {
	subs, ops := splitByOperators(command)
	if len(subs) <= 1 {
		// No operators found or only one subcommand — classify normally.
		return p.Classify(command)
	}

	var worst Classification
	reasons := make([]string, 0, len(subs))
	for i, sub := range subs {
		c := p.Classify(sub)
		reasons = append(reasons, c.Reason)
		_ = ops // operators don't affect the verdict; only subcommands matter

		if i == 0 {
			worst = c
			continue
		}
		// Aggregate: higher severity wins. Block (worst) > Confirm > Allow (best).
		switch {
		case c.Decision == Block || worst.Decision == Block:
			worst.Decision = Block
		case c.Decision == Confirm || worst.Decision == Confirm:
			worst.Decision = Confirm
		default:
			// both Allow
		}
	}

	worst.Command = command
	worst.Reason = "compound command (" + strings.Join(reasons, "; ") + ")"
	return worst
}
