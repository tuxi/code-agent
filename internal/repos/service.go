package repos

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
	_ "modernc.org/sqlite"
)

const cloneStateSchema = `
CREATE TABLE IF NOT EXISTS repo_clone_requests (
    request_id     TEXT PRIMARY KEY,
    payload_hash   TEXT NOT NULL,
    status         TEXT NOT NULL,
    workspace_path TEXT,
    workspace_rel  TEXT,
    error_code     TEXT,
    error_message  TEXT,
    temp_path      TEXT,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);`

// Request is the stable Public Git Clone v1 input.
type Request struct {
	RequestID string
	URL       string
	Ref       string
	Name      string
	Depth     *int
}

type operation struct {
	done chan struct{}
	res  *CloneResult
	err  error
}

// Service owns the public-clone root, durable request idempotency, and in-flight
// request coalescing. It is safe for concurrent HTTP handlers.
type Service struct {
	projectsRoot string
	tempRoot     string
	db           *sql.DB

	mu       sync.Mutex
	inflight map[string]*operation
	clone    func(context.Context, normalizedRequest, string) error
}

type normalizedRequest struct {
	RequestID string `json:"request_id"`
	URL       string `json:"url"`
	Ref       string `json:"ref,omitempty"`
	Name      string `json:"name"`
	Depth     int    `json:"depth"`
}

type storedRequest struct {
	payloadHash   string
	status        string
	workspacePath string
	workspaceRel  string
	errorCode     string
	errorMessage  string
	tempPath      string
}

// NewService initializes Public Git Clone v1. stateDir is Runtime-owned storage
// (Application Support for embedded, ~/.codeagent for daemon), never Documents.
func NewService(projectsRoot, stateDir string) (*Service, error) {
	if strings.TrimSpace(projectsRoot) == "" {
		return nil, errors.New("projects root is required")
	}
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("clone state directory is required")
	}
	root, err := filepath.Abs(projectsRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve projects root: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create projects root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("canonicalize projects root: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create clone state dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "repo-clone-v1.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(cloneStateSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize clone state: %w", err)
	}
	installPublicHTTPSTransport()
	s := &Service{projectsRoot: root, tempRoot: root, db: db, inflight: make(map[string]*operation)}
	s.clone = s.cloneInto
	if err := s.reconcileInterrupted(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() error { return s.db.Close() }

func (s *Service) ProjectsRoot() string { return s.projectsRoot }

// Clone executes or replays a request. Terminal successes and failures are
// durable; concurrent duplicates wait for the same operation.
func (s *Service) Clone(ctx context.Context, req Request) (*CloneResult, error) {
	norm, payloadHash, err := normalizeRequest(req)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if running := s.inflight[norm.RequestID]; running != nil {
		s.mu.Unlock()
		select {
		case <-running.done:
			return running.res, running.err
		case <-ctx.Done():
			return nil, &CloneError{Code: "cancelled", Err: ctx.Err()}
		}
	}
	stored, found, err := s.load(norm.RequestID)
	if err != nil {
		s.mu.Unlock()
		return nil, &CloneError{Code: "io_error", Err: err}
	}
	if found {
		if stored.payloadHash != payloadHash {
			s.mu.Unlock()
			return nil, &CloneError{Code: "destination_conflict", Err: errors.New("request_id is already bound to different clone parameters")}
		}
		if stored.status != "pending" {
			s.mu.Unlock()
			return s.replayStored(stored)
		}
		// A pending record without a live operation is left by a terminated
		// process. Reconciliation normally handles it at startup; fail closed if
		// another process created it after that pass.
		s.mu.Unlock()
		return nil, &CloneError{Code: "cancelled", Err: errors.New("clone was interrupted before completion")}
	}
	if err := s.insertPending(norm.RequestID, payloadHash); err != nil {
		s.mu.Unlock()
		return nil, &CloneError{Code: "io_error", Err: err}
	}
	op := &operation{done: make(chan struct{})}
	s.inflight[norm.RequestID] = op
	s.mu.Unlock()

	res, runErr := s.execute(ctx, norm)
	if persistErr := s.storeTerminal(norm.RequestID, res, runErr); persistErr != nil {
		res = nil
		runErr = &CloneError{Code: "io_error", Err: persistErr}
	}

	s.mu.Lock()
	op.res, op.err = res, runErr
	delete(s.inflight, norm.RequestID)
	close(op.done)
	s.mu.Unlock()
	return res, runErr
}

func normalizeRequest(req Request) (normalizedRequest, string, error) {
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" || len(requestID) > 128 {
		return normalizedRequest{}, "", &CloneError{Code: "invalid_request", Err: errors.New("request_id is required and must be at most 128 characters")}
	}
	cloneURL, cerr := normalizeURL(req.URL)
	if cerr != nil {
		return normalizedRequest{}, "", cerr
	}
	if len(cloneURL) > 4096 {
		return normalizedRequest{}, "", &CloneError{Code: "invalid_url", Err: errors.New("URL is too long")}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = repoNameFromURL(cloneURL)
	}
	if !validName(name) {
		return normalizedRequest{}, "", &CloneError{Code: "invalid_name", Err: fmt.Errorf("invalid target name %q", name)}
	}
	depth := DefaultDepth
	if req.Depth != nil {
		depth = *req.Depth
		if depth < 0 {
			return normalizedRequest{}, "", &CloneError{Code: "invalid_request", Err: errors.New("depth must be zero or a positive integer")}
		}
	}
	norm := normalizedRequest{RequestID: requestID, URL: cloneURL, Ref: strings.TrimSpace(req.Ref), Name: name, Depth: depth}
	if len(norm.Ref) > 1024 {
		return normalizedRequest{}, "", &CloneError{Code: "invalid_request", Err: errors.New("ref is too long")}
	}
	if strings.HasPrefix(norm.Ref, "refs/") && !strings.HasPrefix(norm.Ref, "refs/heads/") && !strings.HasPrefix(norm.Ref, "refs/tags/") {
		return normalizedRequest{}, "", &CloneError{Code: "invalid_request", Err: errors.New("ref must identify a branch or tag")}
	}
	payload, _ := json.Marshal(struct {
		URL   string `json:"url"`
		Ref   string `json:"ref"`
		Name  string `json:"name"`
		Depth int    `json:"depth"`
	}{norm.URL, norm.Ref, norm.Name, norm.Depth})
	sum := sha256.Sum256(payload)
	return norm, hex.EncodeToString(sum[:]), nil
}

func (s *Service) execute(ctx context.Context, req normalizedRequest) (*CloneResult, error) {
	temp, err := os.MkdirTemp(s.tempRoot, ".codeagent-clone-")
	if err != nil {
		return nil, &CloneError{Code: "io_error", Err: err}
	}
	defer os.RemoveAll(temp)
	if _, err := s.db.Exec(`UPDATE repo_clone_requests SET temp_path=?, updated_at=? WHERE request_id=?`, temp, time.Now().UTC().Format(time.RFC3339Nano), req.RequestID); err != nil {
		return nil, &CloneError{Code: "io_error", Err: err}
	}
	if err := s.clone(ctx, req, temp); err != nil {
		var ce *CloneError
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, classifyCloneError(err)
	}

	for suffix := 0; suffix < 10000; suffix++ {
		name := req.Name
		if suffix > 0 {
			name = fmt.Sprintf("%s%d", req.Name, suffix)
		}
		target := filepath.Join(s.projectsRoot, name)
		if !isUnder(target, s.projectsRoot) {
			return nil, &CloneError{Code: "invalid_name", Err: errors.New("destination escapes projects root")}
		}
		if err := renameNoReplace(temp, target); err == nil {
			return &CloneResult{AbsPath: target, Rel: name}, nil
		} else if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
			continue
		} else {
			return nil, &CloneError{Code: "io_error", Err: err}
		}
	}
	return nil, &CloneError{Code: "destination_conflict", Err: errors.New("could not allocate an unused project name")}
}

func (s *Service) cloneInto(ctx context.Context, req normalizedRequest, temp string) error {
	transportURL, err := url.Parse(req.URL)
	if err != nil {
		return &CloneError{Code: "invalid_url", Err: err}
	}
	transportURL.Scheme = publicHTTPSProtocol
	ref, err := resolveReference(ctx, transportURL.String(), req.Ref)
	if err != nil {
		return err
	}
	opts := &gogit.CloneOptions{URL: transportURL.String(), Depth: req.Depth}
	if ref != "" {
		opts.ReferenceName = ref
		opts.SingleBranch = true
	}
	repo, err := gogit.PlainCloneContext(ctx, temp, false, opts)
	if err != nil {
		return err
	}
	cfg, err := repo.Config()
	if err != nil {
		return &CloneError{Code: "io_error", Err: err}
	}
	if origin := cfg.Remotes["origin"]; origin != nil {
		origin.URLs = []string{req.URL}
	}
	if err := repo.SetConfig(cfg); err != nil {
		return &CloneError{Code: "io_error", Err: err}
	}
	return nil
}

func resolveReference(ctx context.Context, remoteURL, ref string) (plumbing.ReferenceName, error) {
	if ref == "" {
		return "", nil
	}
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}})
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{})
	if err != nil {
		return "", classifyCloneError(err)
	}
	candidates := []plumbing.ReferenceName{plumbing.ReferenceName(ref)}
	if !strings.HasPrefix(ref, "refs/") {
		candidates = []plumbing.ReferenceName{plumbing.NewBranchReferenceName(ref), plumbing.NewTagReferenceName(ref)}
	}
	for _, candidate := range candidates {
		for _, remoteRef := range refs {
			if remoteRef.Name() == candidate {
				return candidate, nil
			}
		}
	}
	return "", &CloneError{Code: "ref_not_found", Err: fmt.Errorf("remote ref %q was not found", ref)}
}

func (s *Service) insertPending(requestID, payloadHash string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT INTO repo_clone_requests(request_id,payload_hash,status,created_at,updated_at) VALUES(?,?,?,?,?)`, requestID, payloadHash, "pending", now, now)
	return err
}

func (s *Service) load(requestID string) (storedRequest, bool, error) {
	var r storedRequest
	err := s.db.QueryRow(`SELECT payload_hash,status,COALESCE(workspace_path,''),COALESCE(workspace_rel,''),COALESCE(error_code,''),COALESCE(error_message,''),COALESCE(temp_path,'') FROM repo_clone_requests WHERE request_id=?`, requestID).
		Scan(&r.payloadHash, &r.status, &r.workspacePath, &r.workspaceRel, &r.errorCode, &r.errorMessage, &r.tempPath)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRequest{}, false, nil
	}
	return r, err == nil, err
}

func (s *Service) replayStored(r storedRequest) (*CloneResult, error) {
	if r.status == "succeeded" {
		path := filepath.Join(s.projectsRoot, r.workspaceRel)
		if r.workspaceRel == "" || !isUnder(path, s.projectsRoot) {
			return nil, &CloneError{Code: "io_error", Err: errors.New("stored clone destination is invalid")}
		}
		return &CloneResult{AbsPath: path, Rel: r.workspaceRel}, nil
	}
	code := r.errorCode
	if code == "" {
		code = "io_error"
	}
	return nil, &CloneError{Code: code, Err: errors.New(r.errorMessage)}
}

func (s *Service) storeTerminal(requestID string, res *CloneResult, runErr error) error {
	status, code, message := "succeeded", "", ""
	path, rel := "", ""
	if res != nil {
		path, rel = res.AbsPath, res.Rel
	}
	if runErr != nil {
		status = "failed"
		var ce *CloneError
		if errors.As(runErr, &ce) {
			code, message = ce.Code, ce.Err.Error()
			if code == "cancelled" {
				status = "cancelled"
			}
		} else {
			code, message = "io_error", runErr.Error()
		}
	}
	_, err := s.db.Exec(`UPDATE repo_clone_requests SET status=?,workspace_path=?,workspace_rel=?,error_code=?,error_message=?,temp_path='',updated_at=? WHERE request_id=?`,
		status, path, rel, code, message, time.Now().UTC().Format(time.RFC3339Nano), requestID)
	return err
}

func (s *Service) reconcileInterrupted() error {
	rows, err := s.db.Query(`SELECT request_id,temp_path FROM repo_clone_requests WHERE status='pending'`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id, temp string
		if err := rows.Scan(&id, &temp); err != nil {
			return err
		}
		if temp != "" && filepath.Dir(temp) == s.tempRoot && strings.HasPrefix(filepath.Base(temp), ".codeagent-clone-") {
			_ = os.RemoveAll(temp)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE repo_clone_requests SET status='cancelled',error_code='cancelled',error_message='clone was interrupted before completion',temp_path='',updated_at=? WHERE request_id=?`, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
			return err
		}
	}
	return nil
}
