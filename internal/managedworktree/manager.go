package managedworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"code-agent/internal/session"
	"code-agent/internal/workspace"
	"code-agent/internal/worktree"
)

const (
	CodeNotRequested       = "managed_worktree_not_requested"
	CodeNotGitRepository   = "workspace_not_git_repository"
	CodeBaseRefUnavailable = "base_ref_unavailable"
	CodeNameConflict       = "worktree_name_conflict"
	CodePathConflict       = "worktree_path_conflict"
	CodeBranchConflict     = "worktree_branch_conflict"
	CodeNestedNotAllowed   = "worktree_nested_not_allowed"
	CodeEscapeDetected     = "worktree_escape_detected"
	CodeProvisionFailed    = "worktree_provision_failed"
	CodeMissing            = "worktree_missing"
)

type Error struct {
	Code            string
	Message         string
	ClientRequestID string
	SessionID       string
	Cause           error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

type Warning struct {
	Code    string
	Message string
}

type CreateRequest struct {
	ClientRequestID     string
	SourceWorkspacePath string
	SourceWorkspaceID   string
	BaseWorkspaceID     string
	SuggestedName       string
	BaseRef             worktree.BaseRef
}

type CreateResult struct {
	Session  *session.Session
	Record   worktree.Record
	Warnings []Warning
}

type ConversationRepository interface {
	CreateWithID(ctx context.Context, id, workspacePath, workspaceExtID string) (*session.Session, error)
	Load(ctx context.Context, id string) (*session.Session, error)
	Save(ctx context.Context, sess *session.Session) error
}

type gitRunner interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

type execGit struct{}

func (execGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Manager owns opt-in provisioning. It does not consume turn scheduler slots.
type Manager struct {
	store worktree.Store
	repo  ConversationRepository
	git   gitRunner
	now   func() time.Time

	requestLocks sync.Map // client_request_id → *sync.Mutex
}

func New(store worktree.Store, repo ConversationRepository) *Manager {
	return &Manager{store: store, repo: repo, git: execGit{}, now: time.Now}
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if strings.TrimSpace(req.ClientRequestID) == "" {
		return CreateResult{}, m.err(req, "invalid_request", "client_request_id is required", "", nil)
	}
	lock := m.requestLock(req.ClientRequestID)
	lock.Lock()
	defer lock.Unlock()
	return m.createLocked(ctx, req)
}

func (m *Manager) createLocked(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if req.BaseRef == "" {
		req.BaseRef = worktree.BaseRefHead
	}
	if req.BaseRef != worktree.BaseRefHead && req.BaseRef != worktree.BaseRefFresh {
		return CreateResult{}, m.err(req, CodeBaseRefUnavailable, "base_ref must be head or fresh", "", nil)
	}
	source, err := canonicalExistingDir(req.SourceWorkspacePath)
	if err != nil {
		return CreateResult{}, m.err(req, CodeNotGitRepository, "source workspace is not available", "", err)
	}
	projectRoot, err := m.git.Run(ctx, source, "rev-parse", "--show-toplevel")
	if err != nil {
		return CreateResult{}, m.err(req, CodeNotGitRepository, "source workspace is not a Git repository", "", err)
	}
	projectRoot, err = canonicalExistingDir(projectRoot)
	if err != nil {
		return CreateResult{}, m.err(req, CodeNotGitRepository, "Git project root is not available", "", err)
	}
	gitDir, gitDirErr := gitDirPath(ctx, m.git, projectRoot)
	commonDir, commonErr := gitCommonDir(ctx, m.git, projectRoot)
	if gitDirErr != nil || commonErr != nil {
		return CreateResult{}, m.err(req, CodeNotGitRepository, "Git repository identity could not be resolved", "", errors.Join(gitDirErr, commonErr))
	}
	if !workspace.SamePath(gitDir, commonDir) {
		return CreateResult{}, m.err(req, CodeNestedNotAllowed, "cannot create a managed worktree from another managed worktree", "", nil)
	}
	baseCommit, err := m.resolveBaseCommit(ctx, projectRoot, req.BaseRef)
	if err != nil {
		return CreateResult{}, m.err(req, CodeBaseRefUnavailable, "requested base ref is unavailable", "", err)
	}

	sessionID := session.NewID()
	shortID := shortID(sessionID)
	name := slug(req.SuggestedName) + "-" + shortID
	branch := "codeagent/" + name
	target := filepath.Join(projectRoot, ".codeagent", "worktrees", name)
	if err := workspace.ValidateManagedTarget(projectRoot, target); err != nil {
		return CreateResult{}, m.err(req, CodeEscapeDetected, "managed worktree target escapes the configured root", "", err)
	}
	now := m.now().UTC()
	candidate := worktree.Record{
		SessionID:           sessionID,
		ClientRequestID:     req.ClientRequestID,
		BaseWorkspaceID:     req.BaseWorkspaceID,
		SourceWorkspaceID:   req.SourceWorkspaceID,
		CheckoutWorkspaceID: "checkout_" + shortID,
		SourceWorkspacePath: projectRoot,
		WorktreePath:        target,
		Name:                name,
		Branch:              branch,
		BaseRef:             req.BaseRef,
		BaseCommit:          baseCommit,
		State:               worktree.StateReserved,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	record, created, err := m.store.ReserveWorktree(ctx, candidate)
	if err != nil {
		return CreateResult{}, m.err(req, CodeNameConflict, "managed worktree reservation conflicts with an existing object", "", err)
	}
	if !created {
		if err := validateDuplicateRequest(record, req, projectRoot); err != nil {
			return CreateResult{}, m.err(req, CodeNameConflict, "client_request_id belongs to a different create request", record.SessionID, err)
		}
		if record.State == worktree.StateReady {
			sess, loadErr := m.repo.Load(ctx, record.SessionID)
			if loadErr != nil {
				return CreateResult{}, m.err(req, CodeMissing, "managed worktree session is missing", record.SessionID, loadErr)
			}
			return CreateResult{Session: sess, Record: record, Warnings: warningsFromSession(sess)}, nil
		}
	}

	record.State = worktree.StateProvisioning
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(ctx, record); err != nil {
		return CreateResult{}, m.err(req, CodeProvisionFailed, "could not persist provisioning state", record.SessionID, err)
	}
	warnings := m.sourceWarnings(ctx, projectRoot)
	if err := m.provision(ctx, projectRoot, record); err != nil {
		m.markFailed(ctx, &record, err)
		return CreateResult{}, err
	}

	sess, err := m.repo.CreateWithID(ctx, record.SessionID, record.WorktreePath, "")
	if err != nil {
		provisionErr := m.err(req, CodeProvisionFailed, "worktree was created but session persistence failed", record.SessionID, err)
		m.markFailed(ctx, &record, provisionErr)
		return CreateResult{}, provisionErr
	}
	attachSessionMetadata(sess, record, warnings)
	if err := m.repo.Save(ctx, sess); err != nil {
		provisionErr := m.err(req, CodeProvisionFailed, "worktree session metadata could not be persisted", record.SessionID, err)
		m.markFailed(ctx, &record, provisionErr)
		return CreateResult{}, provisionErr
	}
	record.State = worktree.StateReady
	record.LastErrorCode = ""
	record.LastErrorMessage = ""
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(ctx, record); err != nil {
		return CreateResult{}, m.err(req, CodeProvisionFailed, "could not commit ready worktree state", record.SessionID, err)
	}
	return CreateResult{Session: sess, Record: record, Warnings: warnings}, nil
}

func (m *Manager) provision(ctx context.Context, projectRoot string, record worktree.Record) error {
	if err := workspace.ValidateManagedTarget(projectRoot, record.WorktreePath); err != nil {
		return m.recordErr(record, CodeEscapeDetected, "managed worktree target escapes the configured root", err)
	}
	if _, err := os.Lstat(record.WorktreePath); err == nil {
		return m.recordErr(record, CodePathConflict, "managed worktree path already exists", nil)
	} else if !os.IsNotExist(err) {
		return m.recordErr(record, CodePathConflict, "managed worktree path cannot be inspected", err)
	}
	if _, err := m.git.Run(ctx, projectRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+record.Branch); err == nil {
		return m.recordErr(record, CodeBranchConflict, "managed worktree branch already exists", nil)
	}
	common, err := gitCommonDir(ctx, m.git, projectRoot)
	if err != nil {
		return m.recordErr(record, CodeProvisionFailed, "source Git common dir could not be resolved", err)
	}
	if err := ensureLocalExclude(common); err != nil {
		return m.recordErr(record, CodeProvisionFailed, "managed worktree root could not be added to local Git exclude", err)
	}
	if err := os.MkdirAll(filepath.Dir(record.WorktreePath), 0o755); err != nil {
		return m.recordErr(record, CodeProvisionFailed, "managed worktree root could not be created", err)
	}
	if _, err := m.git.Run(ctx, projectRoot, "worktree", "add", "-b", record.Branch, record.WorktreePath, record.BaseCommit); err != nil {
		return m.recordErr(record, CodeProvisionFailed, "git worktree add failed", err)
	}
	target, err := canonicalExistingDir(record.WorktreePath)
	if err != nil {
		return m.recordErr(record, CodeProvisionFailed, "created worktree path is not accessible", err)
	}
	if !workspace.SamePath(target, record.WorktreePath) {
		return m.recordErr(record, CodeEscapeDetected, "created worktree resolved outside its reserved path", nil)
	}
	targetCommon, err := gitCommonDir(ctx, m.git, target)
	if err != nil || !workspace.SamePath(common, targetCommon) {
		return m.recordErr(record, CodeProvisionFailed, "created worktree does not belong to the source repository", err)
	}
	return nil
}

var excludeMu sync.Mutex

func ensureLocalExclude(commonDir string) error {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	infoDir := filepath.Join(commonDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(infoDir, "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	const pattern = "/.codeagent/worktrees/"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(pattern + "\n")
	return err
}

func (m *Manager) resolveBaseCommit(ctx context.Context, root string, baseRef worktree.BaseRef) (string, error) {
	if baseRef == worktree.BaseRefHead {
		return m.git.Run(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	}
	ref, err := m.git.Run(ctx, root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil || ref == "" {
		return "", errors.New("origin/HEAD is not configured; fresh does not fetch implicitly")
	}
	return m.git.Run(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
}

func (m *Manager) sourceWarnings(ctx context.Context, root string) []Warning {
	status, err := m.git.Run(ctx, root, "status", "--porcelain", "--untracked-files=normal")
	if err == nil && status != "" {
		return []Warning{{Code: "source_workspace_dirty", Message: "Uncommitted and untracked files were not copied into the worktree."}}
	}
	return nil
}

func attachSessionMetadata(sess *session.Session, record worktree.Record, warnings []Warning) {
	if sess.Metadata == nil {
		sess.Metadata = map[string]any{}
	}
	sess.SetExecutionPolicy(session.ExecutionPolicyIsolatedWorktree)
	sess.Metadata[session.MetaWorkspaceID] = record.CheckoutWorkspaceID
	sess.Metadata[session.MetaBaseWorkspaceID] = record.BaseWorkspaceID
	sess.Metadata[session.MetaManagedWorktree] = map[string]any{
		"managed": true, "name": record.Name, "branch": record.Branch,
		"base_ref": string(record.BaseRef), "state": string(worktree.StateReady),
	}
	if len(warnings) > 0 {
		items := make([]map[string]any, 0, len(warnings))
		for _, warning := range warnings {
			items = append(items, map[string]any{"code": warning.Code, "message": warning.Message})
		}
		sess.Metadata[session.MetaWorktreeWarnings] = items
	}
}

func warningsFromSession(sess *session.Session) []Warning {
	raw, ok := sess.Metadata[session.MetaWorktreeWarnings]
	if !ok {
		return nil
	}
	var out []Warning
	switch items := raw.(type) {
	case []map[string]any:
		for _, item := range items {
			code, _ := item["code"].(string)
			message, _ := item["message"].(string)
			if code != "" {
				out = append(out, Warning{Code: code, Message: message})
			}
		}
	case []any:
		for _, rawItem := range items {
			item, _ := rawItem.(map[string]any)
			code, _ := item["code"].(string)
			message, _ := item["message"].(string)
			if code != "" {
				out = append(out, Warning{Code: code, Message: message})
			}
		}
	}
	return out
}

func validateDuplicateRequest(record worktree.Record, req CreateRequest, projectRoot string) error {
	if record.BaseWorkspaceID != req.BaseWorkspaceID || record.SourceWorkspaceID != req.SourceWorkspaceID || record.SourceWorkspacePath != projectRoot || record.BaseRef != req.BaseRef {
		return errors.New("idempotency key scope mismatch")
	}
	return nil
}

func (m *Manager) markFailed(ctx context.Context, record *worktree.Record, err error) {
	record.State = worktree.StateFailed
	record.UpdatedAt = m.now().UTC()
	var managedErr *Error
	if errors.As(err, &managedErr) {
		record.LastErrorCode = managedErr.Code
		record.LastErrorMessage = managedErr.Message
	}
	_ = m.store.UpdateWorktree(context.WithoutCancel(ctx), *record)
}

func (m *Manager) requestLock(requestID string) *sync.Mutex {
	lock, _ := m.requestLocks.LoadOrStore(requestID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Manager) err(req CreateRequest, code, message, sessionID string, cause error) *Error {
	return &Error{Code: code, Message: message, ClientRequestID: req.ClientRequestID, SessionID: sessionID, Cause: cause}
}

func (m *Manager) recordErr(record worktree.Record, code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, ClientRequestID: record.ClientRequestID, SessionID: record.SessionID, Cause: cause}
}

func canonicalExistingDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(real), nil
}

func gitCommonDir(ctx context.Context, git gitRunner, root string) (string, error) {
	common, err := git.Run(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	return canonicalExistingDir(common)
}

func gitDirPath(ctx context.Context, git gitRunner, root string) (string, error) {
	dir, err := git.Run(ctx, root, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return canonicalExistingDir(dir)
}

func shortID(sessionID string) string {
	parts := strings.Split(sessionID, "-")
	id := parts[len(parts)-1]
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return id
}

func slug(input string) string {
	input = strings.TrimSpace(strings.ToLower(input))
	var b strings.Builder
	lastDash := false
	for _, r := range input {
		allowed := unicode.IsLetter(r) || unicode.IsDigit(r)
		if allowed {
			if b.Len() >= 40 {
				break
			}
			b.WriteRune(r)
			lastDash = false
		} else if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(b.String(), "-.")
	if result == "" || result == "." || result == ".." {
		return "task"
	}
	return result
}
