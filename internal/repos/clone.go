// Package repos provides repository operations exposed over the agent-wire HTTP
// surface (as opposed to the agent's git_* tools). Its first member is Clone,
// which clones a public GitHub repository into the workspace so the host can
// "Import from GitHub" before a conversation starts. See
// docs/ios_github_clone_spec.md.
package repos

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// CloneOptions is the input to Clone. Only URL is required.
type CloneOptions struct {
	URL   string // "https://github.com/owner/repo" or the "owner/repo" shorthand
	Ref   string // optional branch/tag; empty = the repo's default branch
	Name  string // optional target directory name; empty = derived from the URL
	Depth int    // optional shallow depth; 0 means use the default (DefaultDepth)
}

// CloneResult is returned on success.
type CloneResult struct {
	AbsPath string // absolute path the repo was cloned to (this launch only)
	Rel     string // path relative to the workspace root — the portable identity
}

// CloneError is a structured error whose Code the host switches on. See spec §3.
type CloneError struct {
	Code string // invalid_url | invalid_name | repo_not_found | ref_not_found | network_error | io_error
	Err  error
}

func (e *CloneError) Error() string { return e.Code + ": " + e.Err.Error() }

// DefaultDepth is the shallow depth used when CloneOptions.Depth is 0. A working
// copy without full history is enough for "analyze/edit a repo" and saves space.
const DefaultDepth = 1

// Clone clones a public GitHub repository into workspaceRoot/<name>. The target is
// confined to workspaceRoot; a name collision is resolved by appending a numeric
// suffix (repo, repo-2, …) and never overwrites. Errors are returned as *CloneError
// with a stable Code.
func Clone(ctx context.Context, workspaceRoot string, opt CloneOptions) (*CloneResult, error) {
	cloneURL, cerr := normalizeURL(opt.URL)
	if cerr != nil {
		return nil, cerr
	}

	base := strings.TrimSpace(opt.Name)
	if base == "" {
		base = repoNameFromURL(cloneURL)
	}
	if !validName(base) {
		return nil, &CloneError{Code: "invalid_name", Err: fmt.Errorf("invalid target name %q (no slashes, '..', or absolute paths)", base)}
	}

	rootAbs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, &CloneError{Code: "io_error", Err: err}
	}

	rel, targetAbs := uniqueTarget(rootAbs, base)
	// Defense in depth: the resolved target must still be under the workspace.
	if !isUnder(targetAbs, rootAbs) {
		return nil, &CloneError{Code: "invalid_name", Err: fmt.Errorf("target escapes workspace: %s", base)}
	}

	depth := opt.Depth
	if depth == 0 {
		depth = DefaultDepth
	}
	if depth < 0 {
		depth = 0 // explicit full history
	}

	cloneOpts := &gogit.CloneOptions{
		URL:   cloneURL,
		Depth: depth,
	}
	if ref := strings.TrimSpace(opt.Ref); ref != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(ref)
		cloneOpts.SingleBranch = true
	}

	if _, err := gogit.PlainCloneContext(ctx, targetAbs, false, cloneOpts); err != nil {
		// Best-effort: remove a partial checkout so a retry sees a clean target.
		_ = os.RemoveAll(targetAbs)
		return nil, classifyCloneError(err)
	}

	return &CloneResult{AbsPath: targetAbs, Rel: rel}, nil
}

// normalizeURL accepts a full https GitHub URL or the "owner/repo" shorthand and
// returns a canonical https URL. Non-GitHub hosts and other schemes are rejected.
func normalizeURL(raw string) (string, *CloneError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &CloneError{Code: "invalid_url", Err: errors.New("url is required")}
	}
	// Shorthand: "owner/repo" (no scheme).
	if !strings.Contains(raw, "://") {
		parts := strings.Split(strings.Trim(raw, "/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), nil
		}
		return "", &CloneError{Code: "invalid_url", Err: fmt.Errorf("not a recognizable repo URL or owner/repo shorthand: %q", raw)}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", &CloneError{Code: "invalid_url", Err: err}
	}
	if u.Scheme != "https" {
		return "", &CloneError{Code: "invalid_url", Err: fmt.Errorf("only https:// is supported, got %q", u.Scheme)}
	}
	host := strings.ToLower(u.Host)
	if host != "github.com" && host != "www.github.com" {
		return "", &CloneError{Code: "invalid_url", Err: fmt.Errorf("only github.com is supported, got %q", u.Host)}
	}
	return raw, nil
}

// classifyCloneError maps a go-git clone error to a structured CloneError.
func classifyCloneError(err error) *CloneError {
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound),
		errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		// In v1 (public only) a private/missing repo is reported the same way.
		return &CloneError{Code: "repo_not_found", Err: err}
	case errors.Is(err, plumbing.ErrReferenceNotFound),
		errors.Is(err, gogit.NoMatchingRefSpecError{}):
		return &CloneError{Code: "ref_not_found", Err: err}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &CloneError{Code: "network_error", Err: err}
	}
	// Fall back to substring matching — go-git wraps several errors as plain strings.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "authentication required"),
		strings.Contains(msg, "authorization failed"),
		strings.Contains(msg, "repository not found"),
		strings.Contains(msg, "404"):
		return &CloneError{Code: "repo_not_found", Err: err}
	case strings.Contains(msg, "couldn't find remote ref"),
		strings.Contains(msg, "reference not found"),
		strings.Contains(msg, "remote ref"):
		return &CloneError{Code: "ref_not_found", Err: err}
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "network is unreachable"):
		return &CloneError{Code: "network_error", Err: err}
	case strings.Contains(msg, "no space"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "read-only"):
		return &CloneError{Code: "io_error", Err: err}
	}
	// Unknown — treat as a (possibly retryable) network error rather than guessing.
	return &CloneError{Code: "network_error", Err: err}
}

// uniqueTarget returns the first non-colliding "<base>", "<base>-2", … under root,
// matching the host's copy-in uniqueDestination semantics. It returns both the
// relative name and the absolute path.
func uniqueTarget(rootAbs, base string) (rel, abs string) {
	for i := 1; ; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		abs = filepath.Join(rootAbs, candidate)
		if !existsNonEmpty(abs) {
			return candidate, abs
		}
	}
}

// existsNonEmpty reports whether a directory exists and contains entries. A missing
// or empty directory is a valid clone target.
func existsNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false // missing — fine to use
	}
	return len(entries) > 0
}

// validName rejects names that would escape the workspace.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	if filepath.IsAbs(name) {
		return false
	}
	return true
}

// isUnder reports whether path is at or below base.
func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// repoNameFromURL derives a directory name from a git URL: the last path segment
// without a trailing ".git".
func repoNameFromURL(raw string) string {
	name := strings.TrimSuffix(raw, ".git")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		name = "repo"
	}
	return name
}
