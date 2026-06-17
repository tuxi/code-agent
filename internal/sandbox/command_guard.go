package sandbox

import (
	"fmt"
	"strings"
)

// shellOperators are metacharacters that only a shell interprets. run_command
// executes a single program directly (no shell is spawned), so a command
// containing these would not behave as written — the operator would be passed
// as a literal argument. The guard lets the caller surface that explicitly
// instead of silently doing the wrong thing.
var shellOperators = []string{"|", "&", ";", ">", "<", "$(", "`", "\n"}

// ContainsShellOperators reports whether the command *structure* uses shell
// control operators (pipes, redirection, command substitution, chaining).
//
// It examines the command with quoted argument contents removed, so an operator
// or newline that lives inside a quoted string — e.g. a multi-line
// `git commit -m "line1\nline2"` or a message that contains a literal "|" — is
// treated as data, not syntax. That is correct for our execution model: we exec
// one program directly, so quoted bytes are always passed through verbatim.
func ContainsShellOperators(command string) bool {
	structure := unquotedStructure(command)
	for _, op := range shellOperators {
		if strings.Contains(structure, op) {
			return true
		}
	}
	return false
}

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
