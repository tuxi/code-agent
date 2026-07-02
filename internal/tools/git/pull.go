package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitPullTool fetches and integrates changes from a remote into the current
// branch in pure Go via go-git. It is the counterpart to git_clone for
// repositories that already exist in the workspace. It exists for the
// sandboxed (iOS) profile where the shell tool is unavailable.
//
// Instead of using worktree.PullContext (which internally calls isFastForward
// and can fail with "object not found" on repos with missing objects or
// shallow clones), we implement pull as Fetch + IsAncestor + Reset. This
// gives clearer error messages and handles edge cases more gracefully.
type GitPullTool struct{}

func NewGitPullTool() *GitPullTool { return &GitPullTool{} }

func (t *GitPullTool) Name() string { return "git_pull" }

func (t *GitPullTool) Description() string {
	return "Pull changes from a remote into the current branch of the workspace repository. " +
		"Use this to update an already-cloned repository with the latest changes from its remote. " +
		"Only fast-forward merges are supported. If the workspace is already up-to-date, " +
		"this reports that and is a no-op."
}

type gitPullInput struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
}

func (t *GitPullTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"remote": {
			Type:        "string",
			Description: "Name of the remote to pull from. Defaults to 'origin'.",
		},
		"branch": {
			Type:        "string",
			Description: "Remote branch to pull. Defaults to the current branch's upstream tracking branch.",
		},
	}).JSON()
}

func (t *GitPullTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in gitPullInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid git_pull input: %w", err)
		}
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	repo, err := gogit.PlainOpen(rootAbs)
	if err != nil {
		if errors.Is(err, gogit.ErrRepositoryNotExists) {
			return tools.ToolResult{Content: "Not a git repository. Use git_clone to clone a repository first."}, nil
		}
		return tools.ToolResult{}, err
	}

	wt, err := repo.Worktree()
	if err != nil {
		return tools.ToolResult{}, err
	}

	// Resolve the remote name.
	remoteName := strings.TrimSpace(in.Remote)
	if remoteName == "" {
		remoteName = "origin"
	}

	remote, err := repo.Remote(remoteName)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("remote %q not found: %w", remoteName, err)
	}

	// Determine which branch to pull.
	currentBranch := currentBranchName(repo)
	targetBranch := strings.TrimSpace(in.Branch)
	if targetBranch == "" {
		targetBranch = currentBranch
	}
	if targetBranch == "" {
		return tools.ToolResult{}, fmt.Errorf("could not determine current branch; specify 'branch' explicitly")
	}

	// Record the local HEAD hash before fetch so we can report what changed.
	localHash := currentHeadHash(repo)

	// Step 1: Fetch from the remote.
	fetchOpts := &gogit.FetchOptions{RemoteName: remoteName}
	if err := remote.FetchContext(ctx, fetchOpts); err != nil {
		if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return tools.ToolResult{Content: "Already up-to-date."}, nil
		}
		return tools.ToolResult{}, fmt.Errorf("fetch from %q failed: %w", remoteName, err)
	}

	// Step 2: Find the remote tracking reference.
	// For branch "main" on remote "origin", this is refs/remotes/origin/main.
	remoteRefName := plumbing.ReferenceName(
		fmt.Sprintf("refs/remotes/%s/%s", remoteName, targetBranch),
	)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf(
			"remote tracking branch %s not found after fetch — the remote may not have a branch named %q",
			remoteRefName, targetBranch,
		)
	}
	remoteHash := remoteRef.Hash()

	// Step 3: Get commit objects for comparison.
	remoteCommit, err := object.GetCommit(repo.Storer, remoteHash)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return tools.ToolResult{}, fmt.Errorf(
				"fetched objects are incomplete — the repository may be a shallow clone. " +
					"Try re-cloning with git_clone to get a full copy.",
			)
		}
		return tools.ToolResult{}, fmt.Errorf("failed to read remote commit: %w", err)
	}

	localCommit, err := object.GetCommit(repo.Storer, localHash)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return tools.ToolResult{}, fmt.Errorf(
				"local HEAD commit is missing — the repository may be a shallow clone. " +
					"Try re-cloning with git_clone to get a full copy.",
			)
		}
		return tools.ToolResult{}, fmt.Errorf("failed to read local commit: %w", err)
	}

	// Step 4: Verify this is a fast-forward update.
	// localCommit must be an ancestor of remoteCommit.
	isAncestor, err := localCommit.IsAncestor(remoteCommit)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf(
			"failed to verify fast-forward: %w. The repository history may be incomplete; "+
				"try re-cloning with git_clone", err,
		)
	}
	if !isAncestor {
		return tools.ToolResult{}, fmt.Errorf(
			"non-fast-forward pull: local %s has diverged from %s/%s. "+
				"Use a different approach to merge or rebase local changes",
			currentBranch, remoteName, targetBranch,
		)
	}

	// Step 5: Reset the worktree and branch to the remote commit.
	if err := wt.Reset(&gogit.ResetOptions{
		Commit: remoteHash,
		Mode:   gogit.HardReset,
	}); err != nil {
		return tools.ToolResult{}, fmt.Errorf("reset to %s failed: %w", shortHash(remoteHash.String()), err)
	}

	return tools.ToolResult{
		Content: fmt.Sprintf("Pulled %s/%s (%s → %s). HEAD is now at %s.",
			remoteName, targetBranch,
			shortHash(localHash.String()),
			shortHash(remoteHash.String()),
			shortHash(remoteHash.String()),
		),
	}, nil
}

// currentBranchName returns the short name of the current branch (e.g. "main"),
// or "" if HEAD is detached or the repo has no commits.
func currentBranchName(repo *gogit.Repository) string {
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	if head.Name().IsBranch() {
		return head.Name().Short()
	}
	return ""
}

// currentHeadHash returns the hash of HEAD, or a zero hash on error.
func currentHeadHash(repo *gogit.Repository) plumbing.Hash {
	head, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash
	}
	return head.Hash()
}

var _ tools.Tool = (*GitPullTool)(nil)
