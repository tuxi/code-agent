package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
)

// GitInitTool initializes a git repository in the workspace, in pure Go via go-git.
// It is the keystone for git on the sandboxed (iOS) profile: an app folder starts
// as a plain directory, so without init the other git tools have no repository to
// operate on. Once initialized, the agent's edits can be committed and reviewed
// (status/diff/log), giving the user a record of what changed each turn.
type GitInitTool struct{}

func NewGitInitTool() *GitInitTool { return &GitInitTool{} }

func (t *GitInitTool) Name() string { return "git_init" }

func (t *GitInitTool) Description() string {
	return "Initialize a new git repository in the workspace so changes can be tracked, committed, " +
		"and reviewed. Use this once when the workspace is not yet a git repository. If it already " +
		"is one, this reports that and changes nothing."
}

func (t *GitInitTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{}).JSON()
}

func (t *GitInitTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	if _, err := gogit.PlainInit(rootAbs, false); err != nil {
		if errors.Is(err, gogit.ErrRepositoryAlreadyExists) {
			return tools.ToolResult{Content: "Already a git repository: " + rootAbs}, nil
		}
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: "Initialized empty git repository in " + rootAbs}, nil
}

var _ tools.Tool = (*GitInitTool)(nil)
