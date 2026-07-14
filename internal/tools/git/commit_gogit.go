package git

import (
	"context"
	"errors"
	"strings"
	"time"

	"code-agent/internal/workspace"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// gogitCommitter implements committer in pure Go via go-git, for hosts that cannot
// spawn the git binary (iOS). It mirrors the exec backend's behavior: when all is
// set it stages everything (respecting .gitignore) and reports the staged set, then
// commits; an empty working tree is reported as "nothing to commit" (ExitCode 1)
// rather than an error.
type gogitCommitter struct{}

func (c *gogitCommitter) Commit(ctx context.Context, rootAbs, msg string, all bool) (gitCommitResult, string, error) {
	res := gitCommitResult{}

	repo, err := gogit.PlainOpen(rootAbs)
	if err != nil {
		if errors.Is(err, gogit.ErrRepositoryNotExists) {
			res.ExitCode = 128
			return res, "not a git repository: " + rootAbs, nil
		}
		return res, "open repository failed: " + err.Error(), err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return res, "open worktree failed: " + err.Error(), err
	}
	wt.Excludes = append(wt.Excludes, gitignore.ParsePattern("/"+workspace.ManagedWorktreesRelativeRoot+"/", nil))

	if all {
		// AddWithOptions{All:true} stages modified, deleted, and new files, honoring
		// the worktree's .gitignore — the go-git equivalent of `git add -A`.
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			return res, "stage all failed: " + err.Error(), err
		}
		if st, err := wt.Status(); err == nil {
			res.Staged = strings.TrimSpace(st.String())
		}
	}

	hash, err := wt.Commit(msg, &gogit.CommitOptions{Author: commitSignature(repo)})
	if err != nil {
		if errors.Is(err, gogit.ErrEmptyCommit) {
			// Mirror the git binary: exit 1, no error, so the model can react.
			res.ExitCode = 1
			res.Stderr = "nothing to commit, working tree clean"
			return res, "", nil
		}
		return res, "commit failed: " + err.Error(), err
	}

	res.ExitCode = 0
	res.Hash = hash.String()
	res.ShortHash = shortHash(res.Hash)
	res.Subject = firstLine(msg)
	return res, "", nil
}

// commitSignature resolves the commit author from the repo's effective git config
// (local → global → system), falling back to a CodeAgent identity when none is set
// (the common case on iOS, which has no git config). go-git requires an explicit
// author, unlike the git binary which derives one itself.
func commitSignature(repo *gogit.Repository) *object.Signature {
	name, email := "CodeAgent", "codeagent@localhost"
	if cfg, err := repo.ConfigScoped(config.LocalScope); err == nil {
		if cfg.User.Name != "" {
			name = cfg.User.Name
		}
		if cfg.User.Email != "" {
			email = cfg.User.Email
		}
	}
	return &object.Signature{Name: name, Email: email, When: time.Now()}
}
