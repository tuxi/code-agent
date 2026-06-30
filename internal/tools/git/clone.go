package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// GitCloneTool clones a remote git repository into the workspace in pure Go via
// go-git. It is the entry point for "pull a repo into the app and analyze it"
// on the sandboxed (iOS) profile. v1 supports public HTTPS repos only; private
// repo auth (token from Keychain) can be added without changing the tool surface.
type GitCloneTool struct{}

func NewGitCloneTool() *GitCloneTool { return &GitCloneTool{} }

func (t *GitCloneTool) Name() string { return "git_clone" }

func (t *GitCloneTool) Description() string {
	return "Clone a remote git repository into the workspace. Use this when asked to fetch, " +
		"download, or clone a project for analysis. The repository is cloned under the " +
		"workspace root; the directory name is derived from the URL unless a path is given. " +
		"Only public HTTPS repositories are supported."
}

type gitCloneInput struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

func (t *GitCloneTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"url": {
			Type:        "string",
			Description: "The git clone URL (https:// only). E.g. https://github.com/user/repo.git",
		},
		"path": {
			Type:        "string",
			Description: "Directory to clone into, relative to the workspace root. Defaults to the repository name derived from the URL.",
		},
		"branch": {
			Type:        "string",
			Description: "Branch to check out after cloning. Defaults to the repository default branch.",
		},
	}, "url").JSON()
}

func (t *GitCloneTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in gitCloneInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid git_clone input: %w", err)
		}
	}

	cloneURL := strings.TrimSpace(in.URL)
	if cloneURL == "" {
		return tools.ToolResult{}, fmt.Errorf("url is required")
	}

	u, err := url.Parse(cloneURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "file") {
		return tools.ToolResult{}, fmt.Errorf("only https:// (or file://) repository URLs are supported, got %q", cloneURL)
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	relPath := strings.TrimSpace(in.Path)
	if relPath == "" {
		relPath = repoNameFromURL(cloneURL)
	}
	targetAbs, ok := safeJoinWorkspace(rootAbs, relPath)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", relPath)
	}

	// go-git PlainClone refuses to clone into a non-empty directory. Check early so
	// the error is clearer.
	if entries, _ := filepath.Glob(filepath.Join(targetAbs, "*")); entries != nil && len(entries) > 0 {
		// Glob returns nil when the directory does not exist — that is fine and will
		// be created by PlainClone.
		return tools.ToolResult{}, fmt.Errorf("target directory %s is not empty — choose a different path or clone into a new directory", relPath)
	}

	opts := &gogit.CloneOptions{URL: cloneURL}
	if b := strings.TrimSpace(in.Branch); b != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(b)
		opts.SingleBranch = true
	}

	if _, err := gogit.PlainCloneContext(ctx, targetAbs, false, opts); err != nil {
		return tools.ToolResult{}, fmt.Errorf("clone failed: %w", err)
	}

	return tools.ToolResult{Content: "Cloned " + cloneURL + " into " + targetAbs}, nil
}

// repoNameFromURL extracts a human-friendly directory name from a git URL.
// "https://github.com/user/repo.git" → "repo".
func repoNameFromURL(raw string) string {
	name := raw
	// Strip trailing .git.
	name = strings.TrimSuffix(name, ".git")
	// Take the last path segment.
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		name = "repo"
	}
	return name
}

var _ tools.Tool = (*GitCloneTool)(nil)
