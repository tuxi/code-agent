package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"code-agent/internal/workspace"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pmezard/go-difflib/difflib"
)

// gogitDiffer computes a workspace diff in pure Go (no subprocess), for iOS. It
// reports the working tree against the last commit — equivalent to `git diff HEAD`,
// which is the question an agent actually asks ("what have I changed since the last
// commit"). Because the sandboxed agent stages and commits atomically (git_commit
// all=true), the staged/unstaged distinction is not meaningful here, so the `staged`
// flag is treated the same as the default. Untracked files are omitted, matching
// `git diff` semantics. Binary files are reported as differing without a textual diff.
type gogitDiffer struct{}

func (d *gogitDiffer) Diff(ctx context.Context, rootAbs string, in diffInput) (string, error) {
	repo, err := gogit.PlainOpen(rootAbs)
	if err != nil {
		if errors.Is(err, gogit.ErrRepositoryNotExists) {
			return "", fmt.Errorf("not a git repository: %s", rootAbs)
		}
		return "", err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	status, err := wt.Status()
	if err != nil {
		return "", err
	}

	// HEAD tree provides the "before" content. It is absent on an unborn repo (no
	// commits yet), in which case every tracked change is treated as a new file.
	var headTree *object.Tree
	if ref, err := repo.Head(); err == nil {
		if c, err := repo.CommitObject(ref.Hash()); err == nil {
			headTree, _ = c.Tree()
		}
	}

	// Collect changed, tracked paths (skip untracked — git diff ignores them),
	// optionally filtered to in.Path, sorted for deterministic output.
	var paths []string
	for p, st := range status {
		if workspace.ShouldSkipPath(rootAbs, filepath.Join(rootAbs, filepath.FromSlash(p))) {
			continue
		}
		if st.Staging == gogit.Untracked && st.Worktree == gogit.Untracked {
			continue
		}
		if in.Path != "" && !pathUnder(p, in.Path) {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out strings.Builder
	for _, p := range paths {
		before := blobContent(headTree, p)
		after := worktreeContent(rootAbs, p)
		if before == after {
			continue
		}
		if isBinary(before) || isBinary(after) {
			fmt.Fprintf(&out, "diff --git a/%s b/%s\nBinary files a/%s and b/%s differ\n", p, p, p, p)
			continue
		}
		if in.Stat {
			adds, dels := countChanges(before, after)
			fmt.Fprintf(&out, " %s | %d +%d -%d\n", p, adds+dels, adds, dels)
			continue
		}
		ud := difflib.UnifiedDiff{
			A:        difflib.SplitLines(before),
			B:        difflib.SplitLines(after),
			FromFile: "a/" + p,
			ToFile:   "b/" + p,
			Context:  3,
		}
		text, err := difflib.GetUnifiedDiffString(ud)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", p, p)
		out.WriteString(text)
	}
	return out.String(), nil
}

// blobContent returns the content of path in the given tree, or "" if the tree is
// nil or the path is absent (a newly added file).
func blobContent(tree *object.Tree, path string) string {
	if tree == nil {
		return ""
	}
	f, err := tree.File(path)
	if err != nil {
		return ""
	}
	content, err := f.Contents()
	if err != nil {
		return ""
	}
	return content
}

// worktreeContent reads path from disk under rootAbs, or "" if absent (deleted).
func worktreeContent(rootAbs, path string) string {
	b, err := os.ReadFile(filepath.Join(rootAbs, filepath.FromSlash(path)))
	if err != nil {
		return ""
	}
	return string(b)
}

// pathUnder reports whether repo-relative path p equals or is under filter.
func pathUnder(p, filter string) bool {
	return p == filter || strings.HasPrefix(p, filter+"/")
}

// isBinary heuristically detects binary content by a NUL byte, as git does.
func isBinary(s string) bool {
	return strings.IndexByte(s, 0) >= 0
}

// countChanges counts added and deleted lines between before and after, for stat.
func countChanges(before, after string) (adds, dels int) {
	m := difflib.NewMatcher(difflib.SplitLines(before), difflib.SplitLines(after))
	for _, op := range m.GetOpCodes() {
		switch op.Tag {
		case 'r':
			dels += op.I2 - op.I1
			adds += op.J2 - op.J1
		case 'd':
			dels += op.I2 - op.I1
		case 'i':
			adds += op.J2 - op.J1
		}
	}
	return adds, dels
}
