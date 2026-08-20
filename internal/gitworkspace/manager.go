package gitworkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"code-agent/internal/conversation"
	"code-agent/internal/managedworktree"
	"code-agent/internal/workspace"
)

const Capability = "workspace_git_branch_v1"

const (
	CodeWorkspaceNotFound      = "workspace_not_found"
	CodeWorkspaceNotAuthorized = "workspace_not_authorized"
	CodeNotGitRepository       = "workspace_not_git_repository"
	CodeUnsupported            = "workspace_git_unsupported"
	CodeInvalidRef             = "workspace_git_invalid_ref"
	CodeBranchExists           = "workspace_git_branch_exists"
	CodeBranchNotFound         = "workspace_git_branch_not_found"
	CodeDirty                  = "workspace_git_dirty"
	CodeConflictState          = "workspace_git_conflict_state"
	CodeBusy                   = "workspace_git_busy"
	CodeSessionConflict        = "workspace_git_session_conflict"
	CodeManagedWorktree        = "workspace_git_managed_worktree"
	CodeCheckoutFailed         = "workspace_git_checkout_failed"
	CodeCreateFailed           = "workspace_git_create_failed"
)

type Checkout struct {
	Kind   string  `json:"kind"`
	Name   *string `json:"name"`
	Commit string  `json:"commit,omitempty"`
}

type Branch struct {
	Name                  string  `json:"name"`
	Commit                string  `json:"commit"`
	IsCurrent             bool    `json:"is_current"`
	IsCheckedOutElsewhere bool    `json:"is_checked_out_elsewhere"`
	WorktreePath          *string `json:"worktree_path"`
}

type Summary struct {
	ModifiedFiles  int `json:"modified_files"`
	UntrackedFiles int `json:"untracked_files"`
}

type State struct {
	WorkspacePath   string   `json:"workspace_path"`
	IsGitRepository bool     `json:"is_git_repository"`
	Head            Checkout `json:"head"`
	IsDirty         bool     `json:"is_dirty"`
	ModifiedFiles   int      `json:"modified_files"`
	UntrackedFiles  int      `json:"untracked_files"`
	ActiveWorktree  bool     `json:"active_worktree"`
	ConflictState   bool     `json:"-"`
}

type Result struct {
	WorkspacePath string   `json:"workspace_path"`
	Checkout      State    `json:"checkout"`
	Branches      []Branch `json:"branches"`
}

type Error struct {
	Code              string   `json:"code"`
	Message           string   `json:"message"`
	WorkspacePath     string   `json:"workspace_path,omitempty"`
	Checkout          *State   `json:"checkout,omitempty"`
	Summary           *Summary `json:"summary,omitempty"`
	Conflicts         []string `json:"conflicts,omitempty"`
	BaseWorkspacePath string   `json:"base_workspace_path,omitempty"`
	Cause             error    `json:"-"`
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}
func (e *Error) Unwrap() error { return e.Cause }

// Manager serializes all branch mutations and provides the workspace boundary.
// The boundary is the Runtime launch workspace; hosts that need a different
// authorization policy can construct a separate Manager with that root.
type Manager struct {
	root       string
	executor   *conversation.TurnExecutor
	managed    *managedworktree.Manager
	mu         sync.Mutex
	idempotent map[string]Result
}

func New(root string, executor *conversation.TurnExecutor, managed *managedworktree.Manager) *Manager {
	return &Manager{root: root, executor: executor, managed: managed, idempotent: make(map[string]Result)}
}

func (m *Manager) List(ctx context.Context, path string) (Result, error) {
	root, err := m.authorize(path)
	if err != nil {
		return Result{}, err
	}
	state, err := m.checkout(ctx, root)
	if err != nil {
		return Result{}, err
	}
	if !state.IsGitRepository {
		return Result{WorkspacePath: root, Checkout: state, Branches: []Branch{}}, nil
	}
	branches, err := m.branches(ctx, root, state)
	if err != nil {
		return Result{}, &Error{Code: CodeUnsupported, Message: "git branch listing is unavailable", WorkspacePath: root, Checkout: &state, Cause: err}
	}
	return Result{WorkspacePath: root, Checkout: state, Branches: branches}, nil
}

func (m *Manager) Create(ctx context.Context, path, name, start string, checkout bool, requestID string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.authorize(path)
	if err != nil {
		return Result{}, err
	}
	if requestID != "" {
		if old, ok := m.idempotent[root+"\x00"+requestID]; ok {
			return old, nil
		}
	}
	state, err := m.checkout(ctx, root)
	if err != nil {
		return Result{}, err
	}
	if !state.IsGitRepository {
		return Result{}, &Error{Code: CodeNotGitRepository, Message: "workspace is not a git repository", WorkspacePath: root, Checkout: &state}
	}
	if err := m.guard(ctx, root, checkout); err != nil {
		return Result{}, err
	}
	if err := validRef(ctx, root, name); err != nil {
		return Result{}, &Error{Code: CodeInvalidRef, Message: "invalid local branch name", WorkspacePath: root, Checkout: &state, Cause: err}
	}
	if _, err := run(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+name); err == nil {
		return Result{}, &Error{Code: CodeBranchExists, Message: "local branch already exists", WorkspacePath: root, Checkout: &state}
	}
	if checkout && state.ConflictState {
		return Result{}, &Error{Code: CodeConflictState, Message: "workspace is in a merge or rebase conflict state", WorkspacePath: root, Checkout: &state}
	}
	if checkout && state.IsDirty {
		return Result{}, dirtyError(root, state)
	}
	if start == "" {
		start = "HEAD"
	}
	if _, err := run(ctx, root, "rev-parse", "--verify", start+"^{commit}"); err != nil {
		return Result{}, &Error{Code: CodeCreateFailed, Message: "start point was not found", WorkspacePath: root, Checkout: &state, Cause: err}
	}
	if _, err := run(ctx, root, "branch", "--no-track", name, start); err != nil {
		return Result{}, &Error{Code: CodeCreateFailed, Message: "git could not create the branch", WorkspacePath: root, Checkout: &state, Cause: err}
	}
	if checkout {
		if _, err := run(ctx, root, "switch", name); err != nil {
			return Result{}, &Error{Code: CodeCheckoutFailed, Message: "git could not checkout the new branch", WorkspacePath: root, Checkout: &state, Cause: err}
		}
	}
	result, err := m.List(ctx, root)
	if err != nil {
		return Result{}, err
	}
	if requestID != "" {
		m.idempotent[root+"\x00"+requestID] = result
	}
	return result, nil
}

func (m *Manager) Checkout(ctx context.Context, path, name string, allowDirty bool) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.authorize(path)
	if err != nil {
		return Result{}, err
	}
	state, err := m.checkout(ctx, root)
	if err != nil {
		return Result{}, err
	}
	if !state.IsGitRepository {
		return Result{}, &Error{Code: CodeNotGitRepository, Message: "workspace is not a git repository", WorkspacePath: root, Checkout: &state}
	}
	if err := m.guard(ctx, root, true); err != nil {
		return Result{}, err
	}
	// v1 never permits a dirty checkout. The request field is retained for
	// forward-compatible clients, but enabling it cannot bypass this invariant.
	if state.ConflictState {
		return Result{}, &Error{Code: CodeConflictState, Message: "workspace is in a merge or rebase conflict state", WorkspacePath: root, Checkout: &state}
	}
	if state.IsDirty {
		return Result{}, dirtyError(root, state)
	}
	if state.Head.Kind == "detached" && name == "" {
		return Result{}, &Error{Code: CodeBranchNotFound, Message: "branch name is required", WorkspacePath: root, Checkout: &state}
	}
	if _, err := run(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+name); err != nil {
		return Result{}, &Error{Code: CodeBranchNotFound, Message: "local branch was not found", WorkspacePath: root, Checkout: &state}
	}
	if _, err := run(ctx, root, "switch", name); err != nil {
		return Result{}, &Error{Code: CodeCheckoutFailed, Message: "git could not checkout the branch", WorkspacePath: root, Checkout: &state, Cause: err}
	}
	return m.List(ctx, root)
}

func (m *Manager) authorize(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", &Error{Code: CodeWorkspaceNotFound, Message: "workspace_path is required"}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", &Error{Code: CodeWorkspaceNotFound, Message: "invalid workspace path", Cause: err}
	}
	info, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return "", &Error{Code: CodeWorkspaceNotFound, Message: "workspace does not exist", WorkspacePath: abs}
	}
	if err != nil || !info.IsDir() {
		return "", &Error{Code: CodeWorkspaceNotFound, Message: "workspace is not a directory", WorkspacePath: abs}
	}
	root, err := workspace.CanonicalPath(m.root)
	if err != nil {
		return "", &Error{Code: CodeWorkspaceNotAuthorized, Message: "runtime workspace authorization is unavailable", Cause: err}
	}
	canonical, err := workspace.CanonicalPath(abs)
	if err != nil {
		return "", &Error{Code: CodeWorkspaceNotFound, Message: "workspace path cannot be resolved", WorkspacePath: abs, Cause: err}
	}
	if !workspace.IsSubPath(root, canonical) {
		return "", &Error{Code: CodeWorkspaceNotAuthorized, Message: "workspace is not authorized for this Runtime", WorkspacePath: canonical}
	}
	if m.managed != nil {
		records, _ := m.managed.ListWorktrees(context.Background())
		for _, record := range records {
			if workspace.SamePath(record.WorktreePath, canonical) {
				return "", &Error{Code: CodeManagedWorktree, Message: "managed worktree branch operations are not supported", WorkspacePath: canonical, BaseWorkspacePath: record.SourceWorkspacePath}
			}
		}
	}
	if workspace.IsSubPath(filepath.Join(root, workspace.ManagedWorktreesRelativeRoot), canonical) {
		return "", &Error{Code: CodeManagedWorktree, Message: "managed worktree branch operations are not supported", WorkspacePath: canonical, BaseWorkspacePath: root}
	}
	return canonical, nil
}

func (m *Manager) guard(ctx context.Context, root string, mutation bool) error {
	if !mutation || m.executor == nil {
		return nil
	}
	for _, activity := range m.executor.Activity() {
		sess, err := m.executor.LoadSession(ctx, activity.SessionID)
		if err == nil && workspace.SamePath(sess.WorkspacePath, root) {
			return &Error{Code: CodeBusy, Message: "workspace has an active Runtime turn", WorkspacePath: root}
		}
	}
	return nil
}

func dirtyError(root string, state State) error {
	return &Error{Code: CodeDirty, Message: "workspace has uncommitted changes", WorkspacePath: root, Checkout: &state, Summary: &Summary{ModifiedFiles: state.ModifiedFiles, UntrackedFiles: state.UntrackedFiles}}
}

func (m *Manager) checkout(ctx context.Context, root string) (State, error) {
	state := State{WorkspacePath: root, Head: Checkout{Kind: "none"}}
	if _, err := run(ctx, root, "rev-parse", "--git-dir"); err != nil {
		return state, nil
	}
	state.IsGitRepository = true
	branch, branchErr := run(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	commit, commitErr := run(ctx, root, "rev-parse", "HEAD")
	if branchErr == nil {
		n := strings.TrimSpace(branch)
		state.Head.Kind = "branch"
		state.Head.Name = &n
	} else if commitErr == nil {
		state.Head.Kind = "detached"
	} else {
		state.Head.Kind = "unborn"
	}
	state.Head.Commit = strings.TrimSpace(commit)
	porcelain, err := run(ctx, root, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return state, &Error{Code: CodeUnsupported, Message: "git status is unavailable", WorkspacePath: root, Checkout: &state, Cause: err}
	}
	for _, line := range strings.Split(strings.TrimSuffix(porcelain, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "??") {
			state.UntrackedFiles++
		} else {
			state.ModifiedFiles++
		}
		if strings.ContainsAny(line[:min(2, len(line))], "U") {
			state.IsDirty = true
			state.ConflictState = true
		}
	}
	state.IsDirty = state.IsDirty || state.ModifiedFiles > 0 || state.UntrackedFiles > 0
	gitDir, _ := run(ctx, root, "rev-parse", "--git-dir")
	common, _ := run(ctx, root, "rev-parse", "--git-common-dir")
	gitDirSlash := filepath.ToSlash(strings.TrimSpace(gitDir))
	state.ActiveWorktree = strings.Contains(gitDirSlash, "/worktrees/") && strings.TrimSpace(gitDir) != strings.TrimSpace(common)
	gitDirPath := strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDirPath) {
		gitDirPath = filepath.Join(root, gitDirPath)
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDirPath, marker)); err == nil {
			state.IsDirty = true
			state.ConflictState = true
		}
	}
	return state, nil
}

func (m *Manager) branches(ctx context.Context, root string, state State) ([]Branch, error) {
	refs, err := run(ctx, root, "for-each-ref", "--format=%(refname:short)%00%(objectname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	worktrees, _ := run(ctx, root, "worktree", "list", "--porcelain")
	checked := map[string]string{}
	var path string
	for _, line := range strings.Split(worktrees, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			path = strings.TrimPrefix(line, "worktree ")
		}
		if strings.HasPrefix(line, "branch refs/heads/") {
			checked[strings.TrimPrefix(line, "branch refs/heads/")] = path
		}
	}
	var out []Branch
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		b := Branch{Name: parts[0], Commit: parts[1], WorktreePath: nil}
		if state.Head.Name != nil && *state.Head.Name == b.Name {
			b.IsCurrent = true
		}
		if p, ok := checked[b.Name]; ok && !b.IsCurrent {
			b.IsCheckedOutElsewhere = true
			b.WorktreePath = &p
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsCurrent != out[j].IsCurrent {
			return out[i].IsCurrent
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func validRef(ctx context.Context, root, name string) error {
	if strings.TrimSpace(name) != name || strings.TrimSpace(name) == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("branch name is empty, whitespace-padded, or unsafe")
	}
	_, err := run(ctx, root, "check-ref-format", "--branch", name)
	return err
}
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
