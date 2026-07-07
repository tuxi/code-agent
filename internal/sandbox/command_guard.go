package sandbox

import (
	"fmt"
	"strings"
)

// shellOperators are metacharacters that only a shell interprets. Phase A opened
// && and ; via sh -c; Phase B adds |, ||, and >, >>, 2>&1.
// The remaining operators ($(), backticks, \n, single &) are still rejected.
// < (input redirect) is now supported — target path validated in checkRedirectTargets.
//
// NOTE: single "&" (backgrounding) remains rejected. "|" is only rejected when
// it's NOT part of "||" or "|&".
var shellOperators = []string{"`", "\n", "&"}

// ContainsShellOperators reports whether the command *structure* uses shell
// control operators that are NOT supported by Phases A/B. Supported operators
// (&&, ;, |, ||, >, >>, 2>&1) are excluded from this check.
func ContainsShellOperators(command string) bool {
	structure := unquotedStructure(command)
	for _, op := range shellOperators {
		idx := strings.Index(structure, op)
		if idx < 0 {
			continue
		}
		// "&" (single) should not match "&&". Remove "&&" occurrences first.
		if op == "&" {
			cleaned := strings.ReplaceAll(structure, "&&", "")
			if !strings.Contains(cleaned, "&") {
				continue
			}
		}
		return true
	}
	return false
}

// chainOperators are the subset of shell operators that sh -c execution supports
// with per-subcommand safety classification:
//
//	&&   conditional and (Phase A)
//	;    sequential separator (Phase A)
//	|    pipe — stdout of left feeds stdin of right (Phase B)
//	||   conditional or (Phase B)
//
// Note: "|" must appear after "||" in this list so splitByOperators tries the
// longer match first.
var chainOperators = []string{"&&", "||", "|", ";"}

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

// ContainsChainOperators reports whether the command structure uses &&, ;, |, or
// || (outside quotes). These trigger sh -c execution with per-subcommand safety
// classification.
func ContainsChainOperators(command string) bool {
	structure := unquotedStructure(command)
	for _, op := range chainOperators {
		if strings.Contains(structure, op) {
			return true
		}
	}
	return false
}

// splitByOperators splits a command on &&, ;, |, and || outside of quoted spans.
// It returns the trimmed subcommands in order and the operators between them.
//
// Examples:
//
//	"go build && go test"       → ["go build", "go test"], ["&&"]
//	"go test | grep FAIL"       → ["go test", "grep FAIL"], ["|"]
//	"go build || echo failed"   → ["go build", "echo failed"], ["||"]
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
		case strings.HasPrefix(cmd[i:], "||"):
			points = append(points, splitPoint{pos: i, op: "||"})
			i += 2
		case r == ';':
			points = append(points, splitPoint{pos: i, op: ";"})
			i++
		case r == '|':
			points = append(points, splitPoint{pos: i, op: "|"})
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
	// Pre-check: run P2 dangerous pattern detection on the FULL command before
	// splitting. Patterns like "curl ... | bash" or "cat .env |" span across
	// pipe operators and would be missed by per-subcommand classification.
	cmd := strings.TrimSpace(command)
	if args, err := SplitArgs(cmd); err == nil {
		if dp, ok := matchDangerousTokens(args); ok {
			return Classification{Command: cmd, Decision: Block, Level: LevelFullShell, Reason: "dangerous pattern: " + dp.desc}
		}
	}

	subs, ops := splitByOperators(cmd)
	if len(subs) <= 1 {
		// No operators found or only one subcommand — classify normally.
		return p.Classify(cmd)
	}

	var worst Classification
	reasons := make([]string, 0, len(subs))
	for i, sub := range subs {
		c := p.classifyOne(peelWrappers(sub))
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

// safeWrappers are commands that modify HOW another command runs without changing
// WHAT it does. They are stripped before classification so the inner command is
// what gets classified. Wrapper flags (durations, env vars, signal masks) are
// irrelevant to safety.
var safeWrappers = []string{
	"timeout", // timeout 30s cmd → classify cmd
	"time",    // time cmd → classify cmd
	"env",     // env VAR=val cmd → classify cmd
	"nohup",   // nohup cmd → classify cmd
	"nice",    // nice -n 10 cmd → classify cmd
	"sudo",    // sudo cmd → classify cmd
	"stdbuf",  // stdbuf -oL cmd → classify cmd
}

// peelWrappers recursively strips known safe wrappers from a command, returning
// the inner command that should be classified. Non-wrapped commands are returned
// unchanged.
func peelWrappers(command string) string {
	cmd := strings.TrimSpace(command)
	for {
		found := false
		for _, w := range safeWrappers {
			if cmd == w || strings.HasPrefix(cmd, w+" ") {
				rest := strings.TrimSpace(cmd[len(w):])
				if rest == "" {
					return cmd
				}
				switch w {
				case "timeout":
					rest = skipFirstArg(rest) // skip duration
				case "env":
					rest = skipEnvArgs(rest) // skip VAR=val assignments
				case "nice":
					if strings.HasPrefix(rest, "-n ") {
						rest = skipFirstArg(skipFirstArg(rest)) // skip -n and value
					} else if strings.HasPrefix(rest, "-") {
						rest = skipFirstArg(rest)
					}
				case "stdbuf":
					for strings.HasPrefix(rest, "-") {
						rest = skipFirstArg(rest)
					}
				case "sudo":
					for strings.HasPrefix(rest, "-") {
						// flag-with-value: skip flag and its argument
						rest = skipFirstArg(skipFirstArg(rest))
					}
					if strings.HasPrefix(rest, "-- ") {
						rest = rest[3:]
					}
				}
				cmd = strings.TrimSpace(rest)
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return cmd
}

func skipFirstArg(s string) string {
	s = strings.TrimSpace(s)
	idx := strings.IndexAny(s, " \t")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(s[idx:])
}

func skipEnvArgs(s string) string {
	for {
		s = strings.TrimSpace(s)
		idx := strings.IndexAny(s, " \t")
		if idx < 0 {
			if strings.Contains(s, "=") {
				return ""
			}
			return s
		}
		word := s[:idx]
		if !strings.Contains(word, "=") {
			return s
		}
		s = s[idx+1:]
	}
}

// extractCommandSubstitutions finds all $(...) command substitutions in a shell
// command (outside quotes) and returns their inner command strings. Nested $()
// is handled via bracket counting.
func extractCommandSubstitutions(command string) []string {
	var results []string
	inSingle, inDouble := false, false
	i := 0
	for i < len(command) {
		r := rune(command[i])
		switch {
		case inSingle:
			if r == '\'' { inSingle = false }
			i++
		case inDouble:
			// $() IS expanded inside double quotes in a real shell.
			if strings.HasPrefix(command[i:], "$(") {
				inner, end := extractBracketContent(command[i+2:])
				if inner != "" {
					results = append(results, strings.TrimSpace(inner))
				}
				i += 2 + end + 1
			} else {
				if r == '"' { inDouble = false }
				i++
			}
		case r == '\'':
			inSingle = true; i++
		case r == '"':
			inDouble = true; i++
		case strings.HasPrefix(command[i:], "$("):
			inner, end := extractBracketContent(command[i+2:])
			if inner != "" {
				results = append(results, strings.TrimSpace(inner))
			}
			i += 2 + end + 1
		default:
			i++
		}
	}
	return results
}

func extractBracketContent(s string) (string, int) {
	depth := 1
	inSingle, inDouble := false, false
	for i, r := range s {
		switch {
		case inSingle:
			if r == '\'' { inSingle = false }
		case inDouble:
			if r == '"' { inDouble = false }
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth == 0 { return s[:i], i }
		}
	}
	return "", 0
}

// dangerousEnvVars are environment variables that can alter program behavior
// in dangerous ways when set before a command. Setting any of these escalates
// the classification: Allow → Confirm, so the user sees the assignment before
// it executes. Mirrors Claude Code's env-var intercept list.
var dangerousEnvVars = []string{
	"PATH",
	"LD_PRELOAD", "LD_LIBRARY_PATH",
	"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
	"PYTHONPATH", "RUBYOPT", "RUBYLIB",
	"PERL5LIB", "PERLLIB",
	"BASH_ENV", "ENV", "SHELLOPTS",
	"GIT_EXEC_PATH", "GIT_TEMPLATE_DIR",
	"NPM_CONFIG_REGISTRY", "YARN_REGISTRY",
	"PIP_INDEX_URL", "PIP_TRUSTED_HOST",
}

// hasDangerousAssignment checks whether a command starts with dangerous VAR=val
// assignments. It returns true and the variable name if found.
func hasDangerousAssignment(command string) (string, bool) {
	cmd := strings.TrimSpace(command)
	for {
		idx := strings.IndexAny(cmd, " \t")
		word := cmd
		if idx >= 0 {
			word = cmd[:idx]
		}
		// Check if this word looks like VAR=val (contains '=' and not a flag).
		if strings.Contains(word, "=") && !strings.HasPrefix(word, "-") {
			varName := word[:strings.Index(word, "=")]
			for _, d := range dangerousEnvVars {
				if strings.EqualFold(varName, d) {
					return d, true
				}
			}
			// Not dangerous — skip to next assignment.
			if idx < 0 {
				return "", false
			}
			cmd = strings.TrimSpace(cmd[idx:])
			continue
		}
		// Not an assignment — stop checking.
		return "", false
	}
}

// stripSafeAssignments removes VAR=val assignments from the beginning of a
// command when VAR is NOT a dangerous env var. This lets "GOOS=linux go build"
// classify as "go build" rather than being unrecognized.
func stripSafeAssignments(command string) string {
	cmd := strings.TrimSpace(command)
	for {
		idx := strings.IndexAny(cmd, " \t")
		word := cmd
		if idx >= 0 {
			word = cmd[:idx]
		}
		if strings.Contains(word, "=") && !strings.HasPrefix(word, "-") {
			if _, dangerous := hasDangerousAssignment(word); dangerous {
				return command // don't strip — let the escalation handle it
			}
			// Safe assignment — strip it.
			if idx < 0 {
				return strings.TrimSpace(cmd[len(word):])
			}
			cmd = strings.TrimSpace(cmd[idx:])
			continue
		}
		return cmd // not an assignment, stop
	}
}

// ContainsCommandSubstitution reports whether the command structure contains
// $(...) command substitution (outside single quotes). It is used to route
// commands that need shell expansion to the sh -c execution path.
func ContainsCommandSubstitution(command string) bool {
	structure := unquotedStructure(command)
	return strings.Contains(structure, "$(")
}

// ContainsAssignment reports whether the command structure contains VAR=val
// assignments (outside quotes). Commands with assignments need sh -c for the
// shell to set the environment variables before executing the command.
func ContainsAssignment(command string) bool {
	structure := unquotedStructure(command)
	// Look for WORD=value patterns at the start or after spaces.
	return strings.Contains(structure, "=") && !strings.Contains(structure, "==")
}

// ContainsRedirect reports whether the command structure contains redirect
// operators (>, >>, <, 2>, &>) outside quotes. Commands with redirects need
// sh -c for the shell to interpret them.
func ContainsRedirect(command string) bool {
	structure := unquotedStructure(command)
	return strings.Contains(structure, ">") || strings.Contains(structure, "<")
}
