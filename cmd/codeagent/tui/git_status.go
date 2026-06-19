package tui

import (
	"fmt"
	"os/exec"
	"strings"
)

// gitStatus returns a compact workspace summary: branch + changed files, like
// "main · M loop.go · ?? new.go". Empty string if we aren't in a git repo.
func gitStatus() string {
	var parts []string

	// Branch.
	if out, err := runGit("branch", "--show-current"); err == nil && out != "" {
		parts = append(parts, out)
	}

	// Changed files.
	out, err := runGit("status", "--short")
	if err != nil || out == "" {
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
		return ""
	}

	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(line) >= 3 {
			parts = append(parts, strings.TrimSpace(line))
		}
	}

	// Cap the total — a huge workspace fill creates noise, not signal.
	const maxFiles = 10
	if len(parts)-1 > maxFiles {
		extra := len(parts) - 1 - maxFiles
		parts = append(parts[:1+maxFiles], fmt.Sprintf("… %d more", extra))
	}

	return strings.Join(parts, " · ")
}

// gitSummaryLine returns a styled one-liner for the transcript, or "".
func gitSummaryLine() string {
	s := gitStatus()
	if s == "" {
		return ""
	}
	return styleMeta.Render("── " + s + " ──")
}

func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	// Don't let git open a pager; we want the output inline.
	cmd.Env = append(cmd.Environ(), "GIT_PAGER=cat")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
