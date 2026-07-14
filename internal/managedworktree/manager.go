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
	CodeInUse              = "worktree_in_use"
	CodeDirty              = "worktree_dirty"
	CodeRemoveFailed       = "worktree_remove_failed"
	CodeRequestConflict    = "worktree_request_conflict"
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

type RemoveRequest struct {
	SessionID string
	RequestID string
	Force     bool
}

type RemoveResult struct {
	Record worktree.Record
}

// Orphan is a Git-registered checkout below the managed root that has no
// Runtime reservation. Reconciliation reports it but never removes it.
type Orphan struct {
	SourceWorkspacePath string
	WorktreePath        string
	Branch              string
}

type ReconcileReport struct {
	Ready   []worktree.Record
	Missing []worktree.Record
	Failed  []worktree.Record
	Removed []worktree.Record
	Orphans []Orphan
	Issues  []ReconcileIssue
}

type ReconcileIssue struct {
	SessionID string
	Code      string
	Message   string
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
	busy  func(sessionID string) bool

	requestLocks sync.Map // client_request_id → *sync.Mutex
}

func New(store worktree.Store, repo ConversationRepository) *Manager {
	return &Manager{store: store, repo: repo, git: execGit{}, now: time.Now}
}

// SetBusyChecker wires the turn scheduler/active registry without coupling the
// provisioner to the conversation package. It must be configured before serving
// remove requests.
func (m *Manager) SetBusyChecker(check func(sessionID string) bool) { m.busy = check }

func (m *Manager) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if strings.TrimSpace(req.ClientRequestID) == "" {
		return CreateResult{}, m.err(req, "invalid_request", "client_request_id is required", "", nil)
	}
	lock := m.requestLock(req.ClientRequestID)
	lock.Lock()
	defer lock.Unlock()
	return m.createLocked(ctx, req)
}

// Reconcile repairs durable reservations after a Runtime restart. It never
// removes unknown Git worktrees: those are returned as Orphans for diagnostics.
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	records, err := m.store.ListWorktrees(ctx)
	if err != nil {
		return ReconcileReport{}, err
	}
	knownPaths := make(map[string]bool, len(records))
	sources := make(map[string]string)
	var report ReconcileReport
	for i := range records {
		record := records[i]
		knownPaths[pathIdentity(record.WorktreePath)] = true
		sources[pathIdentity(record.SourceWorkspacePath)] = record.SourceWorkspacePath
		if reconcileErr := m.reconcileRecord(ctx, &record); reconcileErr != nil {
			issue := ReconcileIssue{SessionID: record.SessionID, Code: CodeProvisionFailed, Message: reconcileErr.Error()}
			var managedErr *Error
			if errors.As(reconcileErr, &managedErr) {
				issue.Code = managedErr.Code
				issue.Message = managedErr.Message
			}
			report.Issues = append(report.Issues, issue)
		}
		switch record.State {
		case worktree.StateReady:
			report.Ready = append(report.Ready, record)
		case worktree.StateMissing:
			report.Missing = append(report.Missing, record)
		case worktree.StateRemoved:
			report.Removed = append(report.Removed, record)
		case worktree.StateFailed, worktree.StateRemoveFailed:
			report.Failed = append(report.Failed, record)
		}
	}
	for _, source := range sources {
		entries, listErr := m.gitWorktrees(ctx, source)
		if listErr != nil {
			report.Issues = append(report.Issues, ReconcileIssue{Code: CodeNotGitRepository, Message: "could not list Git worktrees for " + source})
			continue
		}
		for _, entry := range entries {
			if knownPaths[pathIdentity(entry.Path)] || !isManagedCheckoutPath(source, entry.Path) {
				continue
			}
			report.Orphans = append(report.Orphans, Orphan{
				SourceWorkspacePath: source,
				WorktreePath:        entry.Path,
				Branch:              strings.TrimPrefix(entry.Branch, "refs/heads/"),
			})
		}
	}
	return report, nil
}

// Remove safely removes a managed checkout while preserving its branch and
// conversation. The request id and force decision are durable so a crash in the
// removing state can resume with the exact same semantics.
func (m *Manager) Remove(ctx context.Context, req RemoveRequest) (RemoveResult, error) {
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.RequestID) == "" {
		return RemoveResult{}, &Error{Code: "invalid_request", Message: "session_id and request_id are required", SessionID: req.SessionID}
	}
	lock := m.requestLock("remove\x00" + req.SessionID)
	lock.Lock()
	defer lock.Unlock()
	record, err := m.store.WorktreeBySessionID(ctx, req.SessionID)
	if err != nil {
		return RemoveResult{}, &Error{Code: CodeMissing, Message: "managed worktree reservation was not found", SessionID: req.SessionID, Cause: err}
	}
	if record.RemoveRequestID == req.RequestID {
		if record.RemoveForce != req.Force {
			return RemoveResult{}, m.recordErr(record, CodeRequestConflict, "request_id was already used with a different force value", nil)
		}
		if record.State == worktree.StateRemoving || record.State == worktree.StateRemoveFailed {
			if err := m.finishRemoval(ctx, &record); err != nil {
				return RemoveResult{}, err
			}
		}
		return RemoveResult{Record: record}, nil
	}
	if record.State == worktree.StateRemoved {
		return RemoveResult{Record: record}, nil
	}
	if m.busy != nil && m.busy(record.SessionID) {
		return RemoveResult{}, m.recordErr(record, CodeInUse, "managed worktree has an active or queued turn", nil)
	}
	if !req.Force {
		dirty, inspectErr := m.hasRemovalRisk(ctx, record)
		if inspectErr != nil {
			return RemoveResult{}, inspectErr
		}
		if dirty {
			return RemoveResult{}, m.recordErr(record, CodeDirty, "managed worktree has dirty, untracked, or new committed changes", nil)
		}
	}
	record.State = worktree.StateRemoving
	record.RemoveRequestID = req.RequestID
	record.RemoveForce = req.Force
	record.LastErrorCode = ""
	record.LastErrorMessage = ""
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(ctx, record); err != nil {
		return RemoveResult{}, m.recordErr(record, CodeRemoveFailed, "could not persist removing state", err)
	}
	if err := m.finishRemoval(ctx, &record); err != nil {
		return RemoveResult{}, err
	}
	return RemoveResult{Record: record}, nil
}

func (m *Manager) reconcileRecord(ctx context.Context, record *worktree.Record) error {
	switch record.State {
	case worktree.StateRemoved, worktree.StateRetained:
		return nil
	case worktree.StateRemoving, worktree.StateRemoveFailed:
		return m.finishRemoval(ctx, record)
	}

	_, statErr := os.Lstat(record.WorktreePath)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		err := m.recordErr(*record, CodeProvisionFailed, "managed worktree path could not be inspected", statErr)
		m.markFailed(ctx, record, err)
		return err
	}
	if exists {
		if err := m.verifyCheckout(ctx, *record); err != nil {
			m.markFailed(ctx, record, err)
			return err
		}
		return m.finalizeReady(ctx, record)
	}

	if record.State == worktree.StateReady || record.State == worktree.StateMissing {
		return m.markMissing(ctx, record)
	}
	registered, err := m.registeredAt(ctx, *record)
	if err == nil && registered {
		return m.markMissing(ctx, record)
	}
	if record.State == worktree.StateFailed {
		switch record.LastErrorCode {
		case CodeNameConflict, CodePathConflict, CodeBranchConflict, CodeNestedNotAllowed, CodeEscapeDetected:
			return nil
		}
	}
	record.State = worktree.StateProvisioning
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(ctx, *record); err != nil {
		return m.recordErr(*record, CodeProvisionFailed, "could not persist provisioning recovery", err)
	}
	if err := m.provision(ctx, record.SourceWorkspacePath, *record); err != nil {
		m.markFailed(ctx, record, err)
		return err
	}
	return m.finalizeReady(ctx, record)
}

func (m *Manager) finalizeReady(ctx context.Context, record *worktree.Record) error {
	sess, err := m.repo.Load(ctx, record.SessionID)
	if err != nil {
		sess, err = m.repo.CreateWithID(ctx, record.SessionID, record.WorktreePath, "")
		if err != nil {
			provisionErr := m.recordErr(*record, CodeProvisionFailed, "worktree exists but session recovery failed", err)
			m.markFailed(ctx, record, provisionErr)
			return provisionErr
		}
	}
	warnings := warningsFromSession(sess)
	if len(warnings) == 0 {
		warnings = m.sourceWarnings(ctx, record.SourceWorkspacePath)
	}
	attachSessionMetadataState(sess, *record, warnings, worktree.StateReady, false)
	if err := m.repo.Save(ctx, sess); err != nil {
		provisionErr := m.recordErr(*record, CodeProvisionFailed, "worktree session metadata recovery failed", err)
		m.markFailed(ctx, record, provisionErr)
		return provisionErr
	}
	record.State = worktree.StateReady
	record.LastErrorCode = ""
	record.LastErrorMessage = ""
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(ctx, *record); err != nil {
		record.State = worktree.StateFailed
		record.LastErrorCode = CodeProvisionFailed
		record.LastErrorMessage = "could not persist ready worktree state"
		return m.recordErr(*record, CodeProvisionFailed, record.LastErrorMessage, err)
	}
	return nil
}

func (m *Manager) markMissing(ctx context.Context, record *worktree.Record) error {
	record.State = worktree.StateMissing
	record.LastErrorCode = CodeMissing
	record.LastErrorMessage = "managed worktree checkout is missing and needs rebind"
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(ctx, *record); err != nil {
		return err
	}
	m.updateSessionState(ctx, *record, worktree.StateMissing, true)
	return nil
}

func (m *Manager) verifyCheckout(ctx context.Context, record worktree.Record) error {
	if err := workspace.ValidateManagedTarget(record.SourceWorkspacePath, record.WorktreePath); err != nil {
		return m.recordErr(record, CodeEscapeDetected, "managed worktree path escaped its source workspace", err)
	}
	target, err := canonicalExistingDir(record.WorktreePath)
	if err != nil || !workspace.SamePath(target, record.WorktreePath) {
		return m.recordErr(record, CodeMissing, "managed worktree checkout is not accessible", err)
	}
	sourceCommon, err := gitCommonDir(ctx, m.git, record.SourceWorkspacePath)
	if err != nil {
		return m.recordErr(record, CodeNotGitRepository, "source Git common dir is unavailable", err)
	}
	if err := ensureLocalExclude(sourceCommon); err != nil {
		return m.recordErr(record, CodeProvisionFailed, "managed worktree root could not be added to local Git exclude", err)
	}
	targetCommon, err := gitCommonDir(ctx, m.git, target)
	if err != nil || !workspace.SamePath(sourceCommon, targetCommon) {
		return m.recordErr(record, CodeEscapeDetected, "managed checkout does not belong to its source repository", err)
	}
	branch, err := m.git.Run(ctx, target, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || branch != "refs/heads/"+record.Branch {
		return m.recordErr(record, CodeBranchConflict, "managed checkout branch does not match its reservation", err)
	}
	return nil
}

func (m *Manager) hasRemovalRisk(ctx context.Context, record worktree.Record) (bool, error) {
	if _, err := os.Lstat(record.WorktreePath); err != nil {
		if os.IsNotExist(err) {
			return false, m.recordErr(record, CodeMissing, "managed worktree checkout is missing", err)
		}
		return false, m.recordErr(record, CodeRemoveFailed, "managed worktree path could not be inspected", err)
	}
	if err := m.verifyCheckout(ctx, record); err != nil {
		return false, err
	}
	status, err := m.git.Run(ctx, record.WorktreePath, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, m.recordErr(record, CodeRemoveFailed, "managed worktree status could not be inspected", err)
	}
	if status != "" {
		return true, nil
	}
	commits, err := m.git.Run(ctx, record.WorktreePath, "rev-list", "--count", record.BaseCommit+"..HEAD")
	if err != nil {
		return false, m.recordErr(record, CodeRemoveFailed, "managed worktree commits could not be inspected", err)
	}
	return strings.TrimSpace(commits) != "0", nil
}

func (m *Manager) finishRemoval(ctx context.Context, record *worktree.Record) error {
	args := []string{"worktree", "remove"}
	if record.RemoveForce {
		args = append(args, "--force")
	}
	args = append(args, record.WorktreePath)
	_, err := m.git.Run(ctx, record.SourceWorkspacePath, args...)
	if err != nil {
		registered, listErr := m.registeredAt(ctx, *record)
		if listErr != nil || registered {
			record.State = worktree.StateRemoveFailed
			record.LastErrorCode = CodeRemoveFailed
			record.LastErrorMessage = "git worktree remove failed"
			record.UpdatedAt = m.now().UTC()
			_ = m.store.UpdateWorktree(context.WithoutCancel(ctx), *record)
			return m.recordErr(*record, CodeRemoveFailed, "git worktree remove failed", err)
		}
	}
	record.State = worktree.StateRemoved
	record.LastErrorCode = ""
	record.LastErrorMessage = ""
	record.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateWorktree(context.WithoutCancel(ctx), *record); err != nil {
		return m.recordErr(*record, CodeRemoveFailed, "could not persist removed state", err)
	}
	m.updateSessionState(ctx, *record, worktree.StateRemoved, false)
	return nil
}

func (m *Manager) updateSessionState(ctx context.Context, record worktree.Record, state worktree.State, needsRebind bool) {
	sess, err := m.repo.Load(ctx, record.SessionID)
	if err != nil {
		return
	}
	attachSessionMetadataState(sess, record, warningsFromSession(sess), state, needsRebind)
	_ = m.repo.Save(context.WithoutCancel(ctx), sess)
}

type gitWorktreeEntry struct {
	Path   string
	Branch string
}

func (m *Manager) gitWorktrees(ctx context.Context, source string) ([]gitWorktreeEntry, error) {
	out, err := m.git.Run(ctx, source, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []gitWorktreeEntry
	var current *gitWorktreeEntry
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			entries = append(entries, gitWorktreeEntry{Path: strings.TrimPrefix(line, "worktree ")})
			current = &entries[len(entries)-1]
		case current != nil && strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(line, "branch ")
		}
	}
	return entries, nil
}

func (m *Manager) registeredAt(ctx context.Context, record worktree.Record) (bool, error) {
	entries, err := m.gitWorktrees(ctx, record.SourceWorkspacePath)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if pathIdentity(entry.Path) == pathIdentity(record.WorktreePath) {
			return true, nil
		}
	}
	return false, nil
}

func isManagedCheckoutPath(source, target string) bool {
	container := filepath.Join(source, filepath.FromSlash(workspace.ManagedWorktreesRelativeRoot))
	canonicalContainer, errA := workspace.CanonicalPath(container)
	canonicalTarget, errB := workspace.CanonicalPath(target)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(filepath.Dir(canonicalTarget), canonicalContainer)
}

func pathIdentity(path string) string {
	canonical, err := workspace.CanonicalPath(path)
	if err != nil {
		canonical, _ = filepath.Abs(path)
	}
	return strings.ToLower(filepath.Clean(canonical))
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
		if err := m.reconcileRecord(ctx, &record); err != nil {
			return CreateResult{}, err
		}
		if record.State != worktree.StateReady {
			return CreateResult{}, m.err(req, CodeMissing, "managed worktree is not ready", record.SessionID, nil)
		}
		sess, loadErr := m.repo.Load(ctx, record.SessionID)
		if loadErr != nil {
			return CreateResult{}, m.err(req, CodeMissing, "managed worktree session is missing", record.SessionID, loadErr)
		}
		return CreateResult{Session: sess, Record: record, Warnings: warningsFromSession(sess)}, nil
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
	attachSessionMetadataState(sess, record, warnings, worktree.StateReady, false)
}

func attachSessionMetadataState(sess *session.Session, record worktree.Record, warnings []Warning, state worktree.State, needsRebind bool) {
	if sess.Metadata == nil {
		sess.Metadata = map[string]any{}
	}
	sess.SetExecutionPolicy(session.ExecutionPolicyIsolatedWorktree)
	sess.Metadata[session.MetaWorkspaceID] = record.CheckoutWorkspaceID
	sess.Metadata[session.MetaBaseWorkspaceID] = record.BaseWorkspaceID
	sess.Metadata[session.MetaManagedWorktree] = map[string]any{
		"managed": true, "name": record.Name, "branch": record.Branch,
		"base_ref": string(record.BaseRef), "state": string(state),
		"needs_rebind": needsRebind,
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
