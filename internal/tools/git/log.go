package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitLogTool shows recent commit history in pure Go via go-git, for the sandboxed
// (iOS) profile where the shell tool that would run `git log` is unavailable.
// Read-only.
type GitLogTool struct {
	DefaultLimit int
	MaxLimit     int
}

func NewGitLogTool() *GitLogTool {
	return &GitLogTool{DefaultLimit: 10, MaxLimit: 50}
}

type gitLogInput struct {
	Limit int    `json:"limit"`
	Path  string `json:"path"`
}

func (t *GitLogTool) Name() string { return "git_log" }

func (t *GitLogTool) Description() string {
	return "Show recent git commit history: short hash, date, author, and subject, most recent " +
		"first. Optionally limit the count or filter to commits touching a path. Read-only."
}

func (t *GitLogTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"limit": {
			Type:        "integer",
			Description: "Maximum number of commits to return (default 10).",
		},
		"path": {
			Type:        "string",
			Description: "Limit history to commits that touched this path. Empty means the whole repo.",
		},
	}).JSON()
}

func (t *GitLogTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in gitLogInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid git_log input: %w", err)
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = t.DefaultLimit
	}
	if limit > t.MaxLimit {
		limit = t.MaxLimit
	}

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

	head, err := repo.Head()
	if err != nil {
		return tools.ToolResult{Content: "No commits yet."}, nil
	}

	opts := &gogit.LogOptions{From: head.Hash()}
	if p := strings.TrimSpace(in.Path); p != "" {
		rel := filepath.ToSlash(filepath.Clean(p))
		opts.FileName = &rel
	}
	iter, err := repo.Log(opts)
	if err != nil {
		return tools.ToolResult{}, err
	}
	defer iter.Close()

	var b strings.Builder
	n := 0
	for n < limit {
		c, err := iter.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return tools.ToolResult{}, err
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s\n",
			shortHash(c.Hash.String()),
			c.Author.When.Format("2006-01-02"),
			c.Author.Name,
			subjectOf(c))
		n++
	}

	if n == 0 {
		return tools.ToolResult{Content: "No commits."}, nil
	}
	return tools.ToolResult{Content: strings.TrimRight(b.String(), "\n")}, nil
}

// subjectOf returns the first line of a commit message.
func subjectOf(c *object.Commit) string {
	return firstLine(c.Message)
}

var _ tools.Tool = (*GitLogTool)(nil)
