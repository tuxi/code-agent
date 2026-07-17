// Package repos provides repository operations exposed over the runtime HTTP
// surface (as opposed to the agent's git_* tools).
package repos

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// CloneOptions is the input to Clone. Only URL is required.
type CloneOptions struct {
	URL   string // public HTTPS Git URL or the backwards-compatible "owner/repo" shorthand
	Ref   string // optional branch/tag; empty = the repo's default branch
	Name  string // optional target directory name; empty = derived from the URL
	Depth int    // optional shallow depth; 0 means use the default (DefaultDepth)
}

// CloneResult is returned on success.
type CloneResult struct {
	AbsPath string // absolute path the repo was cloned to (this launch only)
	Rel     string // path relative to the projects root — the portable identity
}

// CloneError is a structured error whose Code the host switches on.
type CloneError struct {
	Code string // stable Public Git Clone v1 error code
	Err  error
}

func (e *CloneError) Error() string { return e.Code + ": " + e.Err.Error() }

// DefaultDepth is the shallow depth used when CloneOptions.Depth is 0. A working
// copy without full history is enough for "analyze/edit a repo" and saves space.
const DefaultDepth = 1

// Clone is the legacy function API. New HTTP callers should use Service, which
// adds request idempotency, SSRF-safe transport, and atomic publication.
// Clone clones a public repository into workspaceRoot/<name>. The target is
// confined to workspaceRoot; a name collision is resolved by appending a numeric
// suffix (repo, repo1, …) and never overwrites. Errors are returned as *CloneError
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

	rel, targetAbs, err := uniqueTarget(rootAbs, base)
	if err != nil {
		return nil, &CloneError{Code: "io_error", Err: fmt.Errorf("inspect clone destination: %w", err)}
	}
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
	installPublicHTTPSTransport()
	transportURL, err := url.Parse(cloneURL)
	if err != nil {
		return nil, &CloneError{Code: "invalid_url", Err: err}
	}
	transportURL.Scheme = publicHTTPSProtocol
	cloneOpts.URL = transportURL.String()
	if ref := strings.TrimSpace(opt.Ref); ref != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(ref)
		cloneOpts.SingleBranch = true
	}

	repo, err := gogit.PlainCloneContext(ctx, targetAbs, false, cloneOpts)
	if err != nil {
		// Best-effort: remove a partial checkout so a retry sees a clean target.
		_ = os.RemoveAll(targetAbs)
		return nil, classifyCloneError(err)
	}
	if cfg, err := repo.Config(); err == nil {
		if origin := cfg.Remotes["origin"]; origin != nil {
			origin.URLs = []string{cloneURL}
			_ = repo.SetConfig(cfg)
		}
	}

	return &CloneResult{AbsPath: targetAbs, Rel: rel}, nil
}

// normalizeURL accepts a full public HTTPS Git URL or the backwards-compatible
// GitHub "owner/repo" shorthand. Network-address policy is enforced by the safe
// transport at DNS resolution and redirect time.
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
	if !strings.EqualFold(u.Scheme, "https") {
		return "", &CloneError{Code: "invalid_url", Err: fmt.Errorf("only https:// is supported, got %q", u.Scheme)}
	}
	if u.User != nil {
		return "", &CloneError{Code: "invalid_url", Err: errors.New("URL credentials are not allowed")}
	}
	if u.Hostname() == "" {
		return "", &CloneError{Code: "invalid_url", Err: errors.New("URL host is required")}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", &CloneError{Code: "invalid_url", Err: errors.New("localhost URLs are not allowed")}
	}
	if addr, err := netip.ParseAddr(host); err == nil && !isPublicIP(addr.Unmap()) {
		return "", &CloneError{Code: "invalid_url", Err: fmt.Errorf("destination address %s is not public", addr)}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", &CloneError{Code: "invalid_url", Err: errors.New("URL query parameters and fragments are not allowed")}
	}
	u.Scheme = "https"
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
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
	case errors.Is(err, context.Canceled):
		return &CloneError{Code: "cancelled", Err: err}
	case errors.Is(err, context.DeadlineExceeded):
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

// uniqueTarget returns the first non-colliding "<base>", "<base>1", … under root,
// matching the host's copy-in uniqueDestination semantics. It returns both the
// relative name and the absolute path.
func uniqueTarget(rootAbs, base string) (rel, abs string, err error) {
	for i := 1; ; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s%d", base, i-1)
		}
		abs = filepath.Join(rootAbs, candidate)
		if _, statErr := os.Lstat(abs); errors.Is(statErr, os.ErrNotExist) {
			return candidate, abs, nil
		} else if statErr != nil {
			return "", "", statErr
		}
	}
}

// validName rejects names that would escape the workspace.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len([]byte(name)) > 255 {
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
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
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
