package runtime

import "strings"

// ExtractModelFlag pulls a --model NAME (or --model=NAME) out of args from any
// position, returning the chosen name and the remaining args.
func ExtractModelFlag(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--model" || args[i] == "-model":
			if i+1 < len(args) {
				rest := append(append([]string{}, args[:i]...), args[i+2:]...)
				return args[i+1], rest
			}
		case strings.HasPrefix(args[i], "--model="):
			name := strings.TrimPrefix(args[i], "--model=")
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return name, rest
		}
	}
	return "", args
}

// ExtractAutoFlag pulls a boolean `--auto` (or `-auto`) out of args, returning
// whether auto mode should start enabled and the remaining args. It is a CLI flag,
// not a config value, on purpose: the enable switch must come from a trusted source
// the agent cannot write, and config.yaml lives inside the writable workspace
// (p9.1 §12.4). Default off — auto mode is always explicit opt-in.
func ExtractAutoFlag(args []string) (bool, []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--auto" || args[i] == "-auto" {
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return true, rest
		}
	}
	return false, args
}

// ExtractNoContextFilesFlag pulls --no-context-files (or -nc) out of args,
// returning whether to skip AGENTS.md/CLAUDE.md discovery and the remaining args.
func ExtractNoContextFilesFlag(args []string) (bool, []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--no-context-files" || args[i] == "-nc" {
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return true, rest
		}
	}
	return false, args
}

// ExtractTrustFlag pulls --trust or --no-trust out of args from any position,
// returning a tri-state pointer (nil = not specified, true = --trust, false = --no-trust)
// and the remaining args. The trust flag is a security boundary — it must come from the
// CLI (a trusted source the agent cannot write), never from config files in the workspace.
func ExtractTrustFlag(args []string) (*bool, []string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--trust":
			t := true
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return &t, rest
		case "--no-trust":
			f := false
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return &f, rest
		}
	}
	return nil, args
}
