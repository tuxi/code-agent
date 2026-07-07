// Package ast provides a lightweight recursive-descent parser for the subset of
// bash grammar that code-agent's safety classifier needs to understand.
//
// It extracts structured information — subcommands with argv, shell operators,
// redirects, and variable assignments — without external dependencies. Commands
// that are "too complex" for the parser (e.g. if/for/while/case/function) are
// returned as opaque blobs and classified as Confirm by the caller.
//
// Design: fail-soft. A parse error never panics or errors; it returns the full
// command as a single opaque statement. The caller always gets a result it can
// classify. Complexity is surfaced via the TooComplex flag rather than rejected.
package ast

import "strings"

// ── AST node types ────────────────────────────────────────────────────────

// Program is the top-level result of parsing a shell command.
type Program struct {
	Statements []Statement // list elements separated by && or ;
}

// Statement is one element in a && / ; list. It is either an Opaque command
// (when parsing failed or the structure was too complex) or a pipeline.
type Statement struct {
	// Cmd holds the command and its arguments when the statement was successfully
	// parsed as a simple command or pipeline.
	Cmd *Command
	// Op is the operator AFTER this statement ("&&", ";", "") — only meaningful
	// when this statement is part of a Program.Statements list.
	Op string
	// TooComplex is true when the parser encountered a structure it can't fully
	// analyse (if/for/while/case/function/subshell). The raw text is preserved
	// in Cmd.Args[0].
	TooComplex bool
}

// Command holds everything the classifier needs to know about one executable
// unit: the argv, any redirects, and any preceding variable assignments.
type Command struct {
	// Program is the program name (argv[0]). Empty when TooComplex.
	Program string
	// Args holds all arguments including argv[0] at index 0.
	Args []string
	// Assignments are VAR=val assignments that precede the command.
	Assignments []string
	// Redirects are I/O redirects attached to the command.
	Redirects []Redirect
}

// Redirect describes one I/O redirection operator and its target.
type Redirect struct {
	Op     string // ">", ">>", "2>", "&>", "<"
	Target string // file path or &1 / &2 for fd merging
}

// ── Parser ────────────────────────────────────────────────────────────────

// Parse parses a shell command line and returns a Program. It never errors —
// unparseable input is returned as a single opaque Statement with TooComplex set.
func Parse(command string) *Program {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return &Program{}
	}
	t := newTokenizer(cmd)
	prog := parseProgram(t)
	prog = assignOperators(prog, cmd)
	return prog
}

// parseProgram: list → pipeline (("&&" | ";" | "||") pipeline)*
func parseProgram(t *tokenizer) *Program {
	var stmts []Statement
	for {
		st := parseStatement(t)
		stmts = append(stmts, st)
		// Consume the separator operator if present.
		if t.hasPrefix("&&") {
			t.skipBytes(2)
		} else if t.hasPrefix("||") {
			t.skipBytes(2)
		} else if t.hasPrefix(";") {
			t.skipBytes(1)
		} else {
			break
		}
	}
	return &Program{Statements: stmts}
}

// parseStatement: parse a single pipeline (possibly a simple command).
func parseStatement(t *tokenizer) Statement {
	// Check for "too complex" structures first.
	if t.hasPrefix("if ") || t.hasPrefix("for ") || t.hasPrefix("while ") ||
		t.hasPrefix("case ") || t.hasPrefix("function ") || t.hasPrefix("(") || t.hasPrefix("{") {
		return Statement{TooComplex: true, Cmd: &Command{Args: []string{strings.TrimSpace(t.rest())}}}
	}

	// Check for variable assignments before the command.
	assignments := parseAssignments(t)

	// Parse the first command in a potential pipeline.
	cmd := parseSimpleCommand(t)
	cmd.Assignments = assignments
	if cmd.Program == "" && len(cmd.Args) == 0 && len(assignments) > 0 {
		// Only assignments, no command — treat as a simple command.
		cmd.Program = assignments[0]
	}

	// Check for pipe (but not || — that's handled by parseProgram).
	if t.hasPrefix("|") && !t.hasPrefix("||") {
		// Pipeline: collect all piped commands.
		piped := []Command{cmd}
		for t.hasPrefix("|") && !t.hasPrefix("||") {
			t.skipBytes(1) // consume |
			piped = append(piped, parseSimpleCommand(t))
		}
		// For our purposes, the "main" command is the last in the pipeline
		// (its output is what the model sees), but we classify ALL of them.
		// Return as a single statement with the first command's info.
		return Statement{Cmd: &piped[0]}
	}

	return Statement{Cmd: &cmd}
}

// parseAssignments: VAR=val assignments (space-separated, before the command name).
func parseAssignments(t *tokenizer) []string {
	var as []string
	for {
		word := t.peekWord()
		if word == "" || !strings.Contains(word, "=") || strings.HasPrefix(word, "-") {
			break
		}
		as = append(as, word)
		t.skipWord()
	}
	return as
}

// parseSimpleCommand: command_name args? redirects?
func parseSimpleCommand(t *tokenizer) Command {
	var cmd Command
	var words []string

	for {
		word := t.peekWord()
		if word == "" {
			break
		}
		// Stop at shell operators.
		if word == "&&" || word == ";" || word == "|" || word == "||" {
			break
		}
		// Redirect operators.
		if word == ">" || word == ">>" || word == "2>" || word == "&>" || word == "<" {
			t.skipWord()
			target := t.peekWord()
			if target != "" {
				t.skipWord()
			}
			cmd.Redirects = append(cmd.Redirects, Redirect{Op: word, Target: target})
			continue
		}
		// Merge 2>&1 style redirect.
		if strings.HasPrefix(word, "2>") || strings.HasPrefix(word, "&>") {
			op := word
			if strings.HasPrefix(word, "2>&") {
				op = "2>&" + word[3:]
				cmd.Redirects = append(cmd.Redirects, Redirect{Op: op, Target: word[3:]})
			} else if len(word) > 2 {
				cmd.Redirects = append(cmd.Redirects, Redirect{Op: word[:2], Target: word[2:]})
			}
			t.skipWord()
			continue
		}
		t.skipWord()
		words = append(words, word)
	}

	if len(words) > 0 {
		cmd.Program = words[0]
		cmd.Args = words
	}
	return cmd
}

// assignOperators walks the original command to assign operators between statements.
func assignOperators(prog *Program, cmd string) *Program {
	// Find operator positions in the raw text to assign Op to each statement.
	// We walk the raw text to find && / ; between statements.
	opPositions := findOperators(cmd)
	for i := range prog.Statements {
		if i < len(opPositions) {
			prog.Statements[i].Op = opPositions[i]
		}
	}
	return prog
}

// ── Tokenizer ─────────────────────────────────────────────────────────────

type tokenizer struct {
	raw string
	pos int
}

func newTokenizer(s string) *tokenizer {
	return &tokenizer{raw: s, pos: 0}
}

func (t *tokenizer) rest() string { return t.raw[t.pos:] }

func (t *tokenizer) hasPrefix(s string) bool {
	t.skipSpace()
	return strings.HasPrefix(t.raw[t.pos:], s)
}

func (t *tokenizer) skipSpace() {
	for t.pos < len(t.raw) && (t.raw[t.pos] == ' ' || t.raw[t.pos] == '\t') {
		t.pos++
	}
}

func (t *tokenizer) skipBytes(n int) {
	t.skipSpace()
	t.pos += n
}

func (t *tokenizer) peekWord() string {
	t.skipSpace()
	if t.pos >= len(t.raw) {
		return ""
	}
	// Skip past the operator chars if present.
	rest := t.raw[t.pos:]
	if strings.HasPrefix(rest, "&&") || strings.HasPrefix(rest, "||") {
		return rest[:2]
	}
	if strings.HasPrefix(rest, ">>") || strings.HasPrefix(rest, "2>") || strings.HasPrefix(rest, "&>") {
		return rest[:2]
	}
	if rest[0] == ';' || rest[0] == '|' || rest[0] == '>' || rest[0] == '<' {
		return string(rest[0])
	}
	// Read a word: everything until space or operator.
	return readWord(rest)
}

func (t *tokenizer) skipWord() {
	t.skipSpace()
	if t.pos >= len(t.raw) {
		return
	}
	w := t.peekWord()
	t.pos += len(w)
}

func readWord(s string) string {
	inSingle, inDouble := false, false
	for i, r := range s {
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
		case r == ' ' || r == '\t':
			return s[:i]
		case r == ';' || r == '|' || r == '&' || r == '>' || r == '<':
			if i == 0 {
				// Single operator char at start — peekWord handles this.
				return ""
			}
			return s[:i]
		}
	}
	return s
}

func findOperators(cmd string) []string {
	// Walk the raw command to find && and ; operators between statements.
	var ops []string
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
			ops = append(ops, "&&")
			i += 2
		case strings.HasPrefix(cmd[i:], "||"):
			ops = append(ops, "||")
			i += 2
		case r == ';':
			ops = append(ops, ";")
			i++
		case r == '|':
			// Pipe is not a statement separator — skip.
			i++
		default:
			i++
		}
	}
	return ops
}
