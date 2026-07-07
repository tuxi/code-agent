// Package sandbox is the policy & permission layer: it decides, for a given
// shell command, whether the runtime may run it freely, must ask the user
// first, or must refuse it outright.
//
// The agent loop already gates side-effecting tools behind an Approver. This
// package makes that gate *command-aware* for run_command: a read-only
// "git status" should run without a prompt, while "rm -rf build" must be
// confirmed and "rm -rf /" must never run. The loop and the tool stay thin;
// the policy lives here so it can be tested and tightened in one place.
package sandbox

import (
	"strings"

	"code-agent/internal/sandbox/ast"
)

// Decision is the outcome of classifying a command. It is a string-valued enum
// so a Classification marshals directly to readable JSON ("allow" / "confirm" /
// "block") without a conversion step.
type Decision string

const (
	// Allow runs the command without asking the user.
	Allow Decision = "allow"
	// Confirm runs the command only after explicit user confirmation.
	Confirm Decision = "confirm"
	// Block refuses the command; it is never executed.
	Block Decision = "block"
)

// Level is the coarse capability tier a command falls into. It mirrors the
// permission model in the README: higher tiers are progressively more powerful
// and more dangerous. Levels are informational — the runtime gates on Decision —
// but they make policy reasons and telemetry legible.
type Level int

const (
	LevelReadOnly  Level = iota // 0: ls, cat, git status/diff/log — observe only
	LevelSafeDev                // 1: go vet, go test, go env — non-mutating dev
	LevelGitBuild               // 2: go build, git add, builds — mutate artifacts/index
	LevelFullShell              // 3: rm, mv, curl, bash — full shell, always gated
	LevelUnknown   Level = -1   // command matched no rule
)

func (l Level) String() string {
	switch l {
	case LevelReadOnly:
		return "0 (read-only)"
	case LevelSafeDev:
		return "1 (safe dev)"
	case LevelGitBuild:
		return "2 (git/build)"
	case LevelFullShell:
		return "3 (full shell)"
	default:
		return "unknown"
	}
}

// Classification is the full result of classifying a command: what to do, why,
// and the capability tier it landed in.
type Classification struct {
	Command  string
	Decision Decision
	Level    Level
	Reason   string
}

// CommandPolicy classifies a shell command into a Decision. It is the concrete
// permission model: three prefix lists plus a small set of always-blocked
// catastrophic patterns.
//
// Matching is by command prefix on word boundaries: the rule "git status"
// matches "git status" and "git status --short" but not "git stat" or
// "git status-ish". The longest matching prefix wins, so a specific
// "git push --force" rule can override a general "git push" one. Blocked
// patterns are matched as case-insensitive substrings, since a catastrophic
// fragment like "rm -rf /" can appear anywhere on the line.
type CommandPolicy struct {
	AllowedCommands []string // run without confirmation (Level 0–2 safe ops)
	RequiresConfirm []string // run only after the user confirms (mutating / Level 3)
	BlockedCommands []string // refused outright; matched as substrings (catastrophic)

	// levels maps an allow/confirm prefix to its Level, for reporting only.
	levels map[string]Level
}

// DefaultPolicy is the built-in policy used by run_command. It is permissive for
// observation and builds, conservative for anything that mutates the tree or
// reaches the network, and hard-blocks a small set of irrecoverable commands.
//
// Note on git: read and staging operations (status/diff/log/add/fetch) auto-run,
// but commands that can discard uncommitted work (checkout/restore/reset/clean/
// stash) require confirmation even though the PRD groups some under "Level 2".
// Losing a user's working changes silently is exactly the误伤 (collateral
// damage) the upgrade is meant to avoid, so safety wins over auto-running them.
func DefaultPolicy() CommandPolicy {
	allow := map[Level][]string{
		LevelReadOnly: {
			"ls", "pwd", "cat", "echo", "head", "tail", "wc", "find", "grep",
			"rg", "tree", "file", "stat", "which", "env", "date",
			"git status", "git diff", "git log", "git show", "git branch",
			"git remote", "git rev-parse", "git blame", "git describe",
			"git ls-files", "git shortlog", "git tag -l", "git tag --list",
		},
		LevelSafeDev: {
			"go vet", "go test", "go env", "go list", "go version", "go doc",
			"go fmt", "gofmt", "cargo check", "cargo test", "cargo clippy",
			"pyright", "ruff", "golangci-lint",
		},
		LevelGitBuild: {
			"go build", "go run", "go generate", "git add", "git fetch",
			"cargo build", "swift build", "xcodebuild build",
		},
	}
	confirm := map[Level][]string{
		LevelGitBuild: {
			"git commit", "git checkout", "git switch", "git restore",
			"git reset", "git clean", "git stash", "git merge", "git rebase",
			"git pull", "git push", "git tag", "git cherry-pick", "git rm",
			"git mv", "git apply", "git revert",
		},
		LevelFullShell: {
			"rm", "rmdir", "mv", "cp", "chmod", "chown", "ln", "mkdir", "touch",
			"curl", "wget", "ssh", "scp", "kill", "pkill", "killall",
			"bash", "sh", "zsh", "make", "npm", "yarn", "pnpm", "pip", "pip3",
			"docker", "sudo", "xargs", "eval",
		},
	}
	blocked := []string{
		"rm -rf /", "rm -fr /", "rm -rf ~", "rm -fr ~", "rm -rf --no-preserve-root",
		":(){", "mkfs", "dd if=", "of=/dev/sd", "of=/dev/disk", "> /dev/sd",
		"shutdown", "reboot", "halt", "init 0", "init 6",
		"git push --force origin main", "git push -f origin main",
		"git push --force origin master", "git push -f origin master",
		"chmod -r 000", "chmod 000 /",
		// Interpreter -c forms run an arbitrary nested script, which would slip
		// past per-command classification entirely. The MVP runs one program per
		// call and spawns no shell, so these are refused outright.
		"bash -c", "sh -c", "zsh -c", "bash -lc", "/bin/sh -c",
	}

	p := CommandPolicy{levels: map[string]Level{}}
	for lvl, cmds := range allow {
		for _, c := range cmds {
			p.AllowedCommands = append(p.AllowedCommands, c)
			p.levels[c] = lvl
		}
	}
	for lvl, cmds := range confirm {
		for _, c := range cmds {
			p.RequiresConfirm = append(p.RequiresConfirm, c)
			p.levels[c] = lvl
		}
	}
	p.BlockedCommands = blocked
	return p
}

// defaultPolicy backs the package-level Classify. Built once: DefaultPolicy is
// pure and immutable, so a single shared instance is safe to read concurrently.
var defaultPolicy = DefaultPolicy()

// Classify classifies a command against the built-in DefaultPolicy. It is the
// package-level convenience for callers that do not hold their own policy;
// run_command keeps a configurable CommandPolicy and calls the method form.
func Classify(command string) Classification {
	return defaultPolicy.Classify(command)
}

// Classify decides what to do with a command. Precedence: a blocked pattern
// wins over everything; otherwise the longest matching allow/confirm prefix
// wins, with a tie broken toward Confirm (the safer choice); an unrecognized
// command defaults to Confirm so the agent can still run it with the user's
// explicit say-so rather than being hard-blocked.
func (p CommandPolicy) Classify(command string) Classification {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return Classification{Command: cmd, Decision: Block, Level: LevelUnknown, Reason: "empty command"}
	}

	// 0. Wrapper peeling (Phase C): strip known safe wrappers (timeout, env,
	//    sudo, etc.) and classify the inner command. The original command is
	//    preserved for display.
	if peeled := peelWrappers(cmd); peeled != cmd {
		inner := p.classifyOne(peeled)
		inner.Command = cmd
		return inner
	}

	// 0b. $() command substitution extraction: classify each inner command
	//     independently. If any inner command is not Allow, the whole command
	//     inherits the stricter verdict.
	if subs := extractCommandSubstitutions(cmd); len(subs) > 0 {
		return p.classifyWithSubstitutions(cmd, subs)
	}

	// 1. AST-based classification (Phase D): use recursive-descent parser to
	//    extract subcommands with proper argv. Falls back to token-based
	//    classification when AST parsing is unavailable.
	if prog := ast.Parse(cmd); len(prog.Statements) > 1 || hasComplexStructure(prog) {
		return p.classifyAST(prog, cmd)
	}

	// 2. Compound commands (Phase A/B fallback): split on && ; | || and classify
	//    each subcommand independently. Used when AST parsing produces only one
	//    statement (i.e. the AST didn't add value over token-based parsing).
	if ContainsChainOperators(cmd) {
		return p.classifyChain(cmd)
	}

	return p.classifyOne(cmd)
}

// hasComplexStructure reports whether the AST found structures that need
// per-statement classification (pipelines, multiple statements, redirects).
func hasComplexStructure(prog *ast.Program) bool {
	for _, st := range prog.Statements {
		if st.TooComplex {
			return true
		}
		if st.Cmd != nil && len(st.Cmd.Redirects) > 0 {
			return true
		}
	}
	return false
}

// classifyAST classifies each statement from an AST parse independently. Too
// complex statements are treated as Confirm (fail-safe: ask the user).
func (p CommandPolicy) classifyAST(prog *ast.Program, originalCommand string) Classification {
	var worst Classification
	reasons := make([]string, 0, len(prog.Statements))

	for i, st := range prog.Statements {
		var c Classification
		if st.TooComplex {
			c = Classification{
				Command:  originalCommand,
				Decision: Confirm,
				Level:    LevelUnknown,
				Reason:   "too complex structure; needs confirmation",
			}
		} else if st.Cmd != nil && st.Cmd.Program != "" {
			// Reconstruct the subcommand from AST argv.
			subCmd := strings.Join(st.Cmd.Args, " ")
			c = p.classifyOne(peelWrappers(subCmd))
			c.Command = subCmd
		} else {
			c = p.classifyOne(originalCommand)
		}

		reasons = append(reasons, c.Reason)

		if i == 0 {
			worst = c
			continue
		}
		switch {
		case c.Decision == Block || worst.Decision == Block:
			worst.Decision = Block
		case c.Decision == Confirm || worst.Decision == Confirm:
			worst.Decision = Confirm
		}
	}

	worst.Command = originalCommand
	worst.Reason = "AST compound (" + strings.Join(reasons, "; ") + ")"
	return worst
}

// classifyWithSubstitutions classifies a command that contains $(...) command
// substitutions. Each inner command is independently classified, and the outer
// command (with substitutions stripped) is also classified. The aggregate
// verdict applies: any Block → Block, any Confirm → Confirm, all Allow → Allow.
func (p CommandPolicy) classifyWithSubstitutions(originalCommand string, subs []string) Classification {
	reasons := make([]string, 0, len(subs)+1)

	// Classify each inner substitution command.
	var worst Classification
	for i, sub := range subs {
		c := p.classifyOne(peelWrappers(sub))
		reasons = append(reasons, "$("+sub+") → "+c.Reason)
		if i == 0 {
			worst = c
		} else if c.Decision == Block || worst.Decision == Block {
			worst.Decision = Block
		} else if c.Decision == Confirm || worst.Decision == Confirm {
			worst.Decision = Confirm
		}
	}

	// Classify the outer command with $() stripped.
	stripped := stripSubstitutions(originalCommand)
	if stripped != "" && stripped != originalCommand {
		outer := p.Classify(stripped)
		reasons = append(reasons, "outer: "+outer.Reason)
		if outer.Decision == Block || worst.Decision == Block {
			worst.Decision = Block
		} else if outer.Decision == Confirm || worst.Decision == Confirm {
			worst.Decision = Confirm
		}
	}

	worst.Command = originalCommand
	worst.Reason = "command substitution (" + strings.Join(reasons, "; ") + ")"
	return worst
}

// stripSubstitutions removes all $(...) command substitutions from a command,
// replacing each with a placeholder so the outer structure remains intact.
func stripSubstitutions(command string) string {
	var b strings.Builder
	inSingle, inDouble := false, false
	i := 0
	for i < len(command) {
		r := rune(command[i])
		switch {
		case inSingle:
			if r == '\'' { inSingle = false }
			b.WriteRune(r); i++
		case inDouble:
			if strings.HasPrefix(command[i:], "$(") {
				_, end := extractBracketContent(command[i+2:])
				b.WriteString("''")
				i += 2 + end + 1
			} else {
				if r == '"' { inDouble = false }
				b.WriteRune(r); i++
			}
		case r == '\'':
			inSingle = true; b.WriteRune(r); i++
		case r == '"':
			inDouble = true; b.WriteRune(r); i++
		case strings.HasPrefix(command[i:], "$("):
			_, end := extractBracketContent(command[i+2:])
			b.WriteString("''") // placeholder so adjacent tokens don't merge
			i += 2 + end + 1
		default:
			b.WriteRune(r); i++
		}
	}
	return strings.TrimSpace(b.String())
}

// classifyOne classifies a single (non-compound, non-wrapper) command through
// the full pipeline: blocked patterns → dangerous tokens → prefix match.
func (p CommandPolicy) classifyOne(cmd string) Classification {
	// 0. Strip safe variable assignments (e.g. GOOS=linux) from the beginning
	//    of the command so the actual command is what gets classified. Dangerous
	//    assignments stay — they are escalated to Confirm below.
	trimmed := stripSafeAssignments(cmd)
	if trimmed != cmd {
		// Reclassify the trimmed command. If the assignment was dangerous,
		// the escalation check at step 4 below handles it.
		result := p.classifyOne(trimmed)
		// Check the ORIGINAL command for dangerous assignments.
		if v, ok := hasDangerousAssignment(cmd); ok && result.Decision == Allow {
			result.Decision = Confirm
			result.Reason = "dangerous env var " + v + " — needs confirmation"
		}
		result.Command = cmd
		return result
	}

	// 1. Catastrophic patterns are refused no matter what else matches. The match
	//    runs against the command's structure (quoted argument contents removed),
	//    so a commit message that merely *mentions* "rm -rf /" is not blocked —
	//    only an actual "rm -rf /" invocation is. The user still sees the full,
	//    unmasked command at the confirmation prompt.
	structure := strings.ToLower(unquotedStructure(cmd))
	for _, b := range p.BlockedCommands {
		if strings.Contains(structure, strings.ToLower(b)) {
			return Classification{Command: cmd, Decision: Block, Level: LevelFullShell, Reason: "matches blocked pattern: " + b}
		}
	}

	// 2. Structure-aware dangerous token patterns (P2). These operate on argv so
	//    word order is irrelevant and quoted content is excluded. The check is
	//    best-effort: if SplitArgs fails, the command is almost certainly malformed
	//    and will be rejected downstream anyway.
	if args, err := SplitArgs(cmd); err == nil {
		if dp, ok := matchDangerousTokens(args); ok {
			return Classification{Command: cmd, Decision: Block, Level: LevelFullShell, Reason: "dangerous pattern: " + dp.desc}
		}
	}

	// 3. Longest-prefix match across the allow and confirm lists.
	allowPfx := longestPrefixMatch(cmd, p.AllowedCommands)
	confirmPfx := longestPrefixMatch(cmd, p.RequiresConfirm)

	var result Classification
	switch {
	case allowPfx != "" && len(allowPfx) >= len(confirmPfx):
		result = Classification{Command: cmd, Decision: Allow, Level: p.levelOf(allowPfx), Reason: "allowed: " + allowPfx}
	case confirmPfx != "":
		result = Classification{Command: cmd, Decision: Confirm, Level: p.levelOf(confirmPfx), Reason: "needs confirmation: " + confirmPfx}
	default:
		result = Classification{Command: cmd, Decision: Confirm, Level: LevelUnknown, Reason: "unrecognized command; needs confirmation"}
	}

	// 4. Variable scope tracking: dangerous env var assignments (PATH=,
	//    LD_PRELOAD=, etc.) escalate Allow → Confirm so the user sees them.
	if result.Decision == Allow {
		if v, ok := hasDangerousAssignment(cmd); ok {
			result.Decision = Confirm
			result.Reason = "dangerous env var " + v + " — needs confirmation"
		}
	}

	return result
}

func (p CommandPolicy) levelOf(prefix string) Level {
	if p.levels == nil {
		return LevelUnknown
	}
	if lvl, ok := p.levels[prefix]; ok {
		return lvl
	}
	return LevelUnknown
}
