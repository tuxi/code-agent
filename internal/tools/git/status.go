package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
)

// GitStatusTool reports the working-tree status (current branch + changed files)
// in pure Go via go-git. It exists for the sandboxed (iOS) profile, where the
// shell tool that would otherwise run `git status` is unavailable. Read-only.
type GitStatusTool struct{}

func NewGitStatusTool() *GitStatusTool { return &GitStatusTool{} }

func (t *GitStatusTool) Name() string { return "git_status" }

func (t *GitStatusTool) Description() string {
	return "Show the git working-tree status: current branch and changed files in short format " +
		"(XY path, e.g. ' M file', '?? new'). This is read-only."
}

func (t *GitStatusTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{}).JSON()
}

func (t *GitStatusTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	repo, err := gogit.PlainOpen(rootAbs)
	if err != nil {
		if err == gogit.ErrRepositoryNotExists {
			return tools.ToolResult{Content: "Not a git repository."}, nil
		}
		return tools.ToolResult{}, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return tools.ToolResult{}, err
	}
	st, err := wt.Status()
	if err != nil {
		return tools.ToolResult{}, err
	}

	var b strings.Builder
	if ref, err := repo.Head(); err != nil {
		b.WriteString("No commits yet\n")
	} else if ref.Name().IsBranch() {
		fmt.Fprintf(&b, "On branch %s\n", ref.Name().Short())
	} else {
		fmt.Fprintf(&b, "HEAD detached at %s\n", shortHash(ref.Hash().String()))
	}

	if st.IsClean() {
		b.WriteString("nothing to commit, working tree clean")
	} else {
		b.WriteString(strings.TrimRight(st.String(), "\n"))
	}
	return tools.ToolResult{Content: b.String()}, nil
}

var _ tools.Tool = (*GitStatusTool)(nil)
