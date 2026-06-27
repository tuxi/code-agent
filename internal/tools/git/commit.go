package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitCommitTool creates git commits without going through a shell, so the
// commit message is passed verbatim via stdin (git commit -F -). This avoids
// the quoting and escaping problems that arise when embedding a multi-line
// message with special characters into a shell command string.
type GitCommitTool struct {
	Timeout time.Duration
}

func NewGitCommitTool() *GitCommitTool {
	return &GitCommitTool{
		Timeout: 30 * time.Second,
	}
}

type gitCommitInput struct {
	Message string `json:"message"`
	All     bool   `json:"all"` // stage all changes (including untracked files) before committing (git add -A)
}

type gitCommitResult struct {
	Hash      string `json:"hash,omitempty"`
	ShortHash string `json:"short_hash,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Staged    string `json:"staged,omitempty"` // files added to staging area (git status --short) when all=true
	ExitCode  int    `json:"exit_code"`
}

func (t *GitCommitTool) Name() string { return "git_commit" }

func (t *GitCommitTool) Description() string {
	return "Create a git commit. The commit message is passed via stdin (not a shell argument), " +
		"so multi-line messages and special characters (quotes, backticks, etc.) are handled " +
		"correctly without escaping issues. When all=true, stages all changes (including new " +
		"untracked files) before committing and reports what was staged. Requires user " +
		"confirmation before executing."
}

func (t *GitCommitTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"message": {
			Type:        "string",
			Description: `The commit message. Can be multi-line; the first line becomes the subject. Passed verbatim via stdin — no shell quoting needed.`,
		},
		"all": {
			Type:        "boolean",
			Description: `If true, stage all changes (including new untracked files) via git add -A before committing, and report what was staged. Respects .gitignore. Equivalent to git add -A followed by git commit. Default false; use git add separately for finer control.`,
		},
	}, "message").JSON()
}

// SideEffects marks git_commit as a mutating tool, so the runtime gates it
// behind user confirmation before it runs.
func (t *GitCommitTool) SideEffects() bool { return true }

func (t *GitCommitTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in gitCommitInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid git_commit input: %w", err)
		}
	}

	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		return tools.ToolResult{}, fmt.Errorf("message is required")
	}
	// Ensure trailing newline — git expects it and some editors complain without it.
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	res := gitCommitResult{}

	// When all=true, stage everything first (git add -A), then capture what was
	// staged so the model can inspect it. git add -A respects .gitignore natively,
	// so we don't need to parse it ourselves.
	if in.All {
		addCmd := exec.CommandContext(cmdCtx, "git", "-C", rootAbs, "add", "-A")
		addOut, addErr := addCmd.CombinedOutput()
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			res.ExitCode = -1
			return t.result(res, "git add -A timed out")
		}
		if addErr != nil {
			res.ExitCode = -1
			res.Stderr = strings.TrimSpace(string(addOut))
			return t.result(res, "git add -A failed: "+addErr.Error())
		}

		statusCmd := exec.CommandContext(cmdCtx, "git", "-C", rootAbs, "status", "--short")
		statusOut, _ := statusCmd.CombinedOutput()
		res.Staged = strings.TrimSpace(string(statusOut))
	}

	// Build args: git -C <root> commit -F -
	args := []string{"-C", rootAbs, "commit", "-F", "-"}

	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Dir = rootAbs
	cmd.Stdin = strings.NewReader(msg)

	output, err := cmd.CombinedOutput()

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		res.ExitCode = -1
		return t.result(res, "git commit timed out")
	}

	content := string(output)
	res.Stderr = strings.TrimSpace(content)

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
			return t.result(res, "git commit failed to start: "+err.Error())
		}
		// Exit code 1 from git commit usually means "nothing to commit" —
		// return the output so the model can read it and decide what to do.
		return t.result(res, "")
	}

	// Success: extract the commit hash from the output.
	res.ExitCode = 0
	res.Hash = extractHash(content)
	res.ShortHash = shortHash(res.Hash)
	res.Subject = firstLine(msg)

	return t.result(res, "")
}

func (t *GitCommitTool) result(res gitCommitResult, note string) (tools.ToolResult, error) {
	type enriched struct {
		gitCommitResult
		Note string `json:"note,omitempty"`
	}
	enc := enriched{gitCommitResult: res, Note: note}
	data, err := json.MarshalIndent(enc, "", "  ")
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: string(data)}, nil
}

// extractHash pulls the full commit SHA from git's default output, which
// contains a line like "[main abc1234] subject" or
// "[main (root-commit) abc1234] subject".
func extractHash(output string) string {
	// Look for the bracket pattern: [branch <hash>]
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		// Find the last hex-looking token before the closing bracket.
		end := strings.Index(line, "]")
		if end < 0 {
			continue
		}
		inner := line[1:end]
		fields := strings.Fields(inner)
		// The hash is the last field (or second-to-last if "root-commit" is present).
		for i := len(fields) - 1; i >= 0; i-- {
			if isHex(fields[i]) && len(fields[i]) >= 7 {
				return fields[i]
			}
		}
	}
	return ""
}

func shortHash(hash string) string {
	if len(hash) >= 7 {
		return hash[:7]
	}
	return hash
}

func firstLine(msg string) string {
	line, _, _ := strings.Cut(msg, "\n")
	return strings.TrimSpace(line)
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

var (
	_ tools.Tool          = (*GitCommitTool)(nil)
	_ tools.SideEffecting = (*GitCommitTool)(nil)
)
