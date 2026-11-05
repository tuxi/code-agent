package mcpbridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Selection is a startup opt-in for a trusted local client holding an owner
// grant. It is not a server-scoped grant, sandbox, or managed admission bypass.
// Each project retains the existing single-workspace execution checks.
type workspaceRouter struct {
	mu          sync.Mutex
	selectionMu sync.Mutex
	primary     *ManualService
	services    []*ManualService
	creations   map[string]string
	paths       map[string]workspaceAlias
	closed      bool
}

type workspaceAlias struct {
	service *ManualService
	scope   historyScope
}

func NewMultiWorkspaceManualService(ctx context.Context, manifest, workspace string, readWorkspaces []string) (*ManualService, error) {
	s, err := NewManualServiceWithReadWorkspaces(ctx, manifest, workspace, readWorkspaces)
	if err != nil {
		return nil, err
	}
	if s.boundaryErr == nil {
		s.commonInfo, s.boundaryErr = gitCommonIdentity(ctx, s.boundary.root)
	}
	s.router = &workspaceRouter{primary: s, services: []*ManualService{s}, creations: map[string]string{}, paths: map[string]workspaceAlias{}}
	s.router.paths[filepath.Clean(workspace)] = workspaceAlias{s, pinHistoryScope(workspace)}
	if s.boundary != nil {
		s.router.paths[s.boundary.root] = workspaceAlias{s, pinHistoryScope(s.boundary.root)}
	}
	return s, nil
}

func (r *workspaceRouter) snapshot() []*ManualService {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ManualService(nil), r.services...)
}

func (r *workspaceRouter) close() {
	r.mu.Lock()
	r.closed = true
	children := append([]*ManualService(nil), r.services[1:]...)
	r.mu.Unlock()
	for _, child := range children {
		child.Close()
	}
}

func (s *ManualService) bindingOwner(id string) (*ManualService, error) {
	if s.router == nil {
		return s, nil
	}
	for _, candidate := range s.router.snapshot() {
		candidate.mu.RLock()
		b := candidate.bindings[id]
		candidate.mu.RUnlock()
		if b != nil {
			return candidate, nil
		}
	}
	return nil, bridgeError("binding_not_found", nil)
}

func (s *ManualService) reserveCreation(request string) error {
	if s.router == nil {
		return nil
	}
	if !validID(request) {
		return bridgeError("invalid_tool_arguments", nil)
	}
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, exists := r.creations[request]; exists && old != s.configuredRoot {
		return admissionError("request_conflict", "", request, nil)
	}
	r.creations[request] = s.configuredRoot
	return nil
}

func (s *ManualService) ConnectionInfoForWorkspace(ctx context.Context, path, binding string) (ManualConnectionInfo, error) {
	if binding != "" {
		if path != "" {
			return ManualConnectionInfo{}, bridgeError("invalid_tool_arguments", nil)
		}
		return s.ConnectionInfo(ctx, binding)
	}
	if path == "" {
		return s.ConnectionInfo(ctx, "")
	}
	selected, err := s.executionWorkspace(ctx, path, true)
	if err != nil {
		return ManualConnectionInfo{}, err
	}
	return selected.ConnectionInfo(ctx, "")
}

// Paths never select a current project. create/bind must name an already opened
// project. Only connection_info may open one, without creating a Session/Turn.
func (s *ManualService) executionWorkspace(ctx context.Context, path string, open bool) (*ManualService, error) {
	if s.router == nil {
		if path == "" {
			return s, nil
		}
		if !filepath.IsAbs(path) {
			return nil, bridgeError("invalid_tool_arguments", nil)
		}
		if err := s.checkBoundary(ctx); err != nil {
			return nil, err
		}
		root, err := filepath.EvalSymlinks(path)
		if err != nil || root != s.boundary.root {
			return nil, bridgeError("workspace_mismatch", nil)
		}
		return s, nil
	}
	if !filepath.IsAbs(path) {
		return nil, bridgeError("workspace_selection_required", nil)
	}
	r := s.router
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	r.mu.Lock()
	closed := r.closed
	alias, knownPath := r.paths[filepath.Clean(path)]
	services := append([]*ManualService(nil), r.services...)
	r.mu.Unlock()
	if closed || s.ctx.Err() != nil {
		return nil, bridgeError("connection_closed", nil)
	}
	// Every explicitly selected spelling is pinned, including an alias added
	// after its canonical project was already selected.
	if knownPath {
		if err := alias.scope.check(); err != nil {
			return nil, err
		}
		return alias.service, alias.service.checkBoundary(ctx)
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, bridgeError("workspace_mismatch", nil)
	}
	for _, candidate := range services {
		if candidate.boundary != nil && root == candidate.boundary.root {
			if !open {
				return nil, bridgeError("workspace_not_selected", nil)
			}
			if err := candidate.checkBoundary(ctx); err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.paths[filepath.Clean(path)] = workspaceAlias{candidate, pinHistoryScope(path)}
			r.mu.Unlock()
			return candidate, nil
		}
	}
	if !open {
		return nil, bridgeError("workspace_not_selected", nil)
	}
	if len(services) >= 32 {
		return nil, bridgeError("workspace_limit", nil)
	}
	// All children share the verified Runtime/Store and authorization generation.
	// A new workspace cannot silently adopt a rotated or foreign grant.
	child, err := NewManualService(ctx, s.manifest, path)
	if err != nil {
		return nil, err
	}
	// Child lifetime follows the MCP connection, not this selection request.
	child.cancel()
	child.ctx, child.cancel = context.WithCancel(r.primary.ctx)
	child.manualAuthority = s.manualAuthority
	c, err := child.probe(ctx, false)
	if err != nil {
		child.Close()
		return nil, err
	}
	c.Close()
	child.commonInfo, err = gitCommonIdentity(ctx, child.boundary.root)
	if err != nil {
		child.Close()
		return nil, err
	}
	for _, candidate := range services {
		if candidate.boundary == nil {
			continue
		}
		// Do not let a replaced project identity or linked Git metadata evade the
		// original writer guard. An unavailable old boundary must be reconciled.
		if err := candidate.checkBoundary(ctx); err != nil {
			child.Close()
			return nil, err
		}
		if overlappingRoots(child.boundary.root, candidate.boundary.root) {
			child.Close()
			return nil, bridgeError("workspace_overlap", nil)
		}
		if candidate.commonInfo == nil || os.SameFile(child.commonInfo, candidate.commonInfo) {
			child.Close()
			return nil, bridgeError("workspace_overlap", nil)
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		child.Close()
		return nil, bridgeError("connection_closed", nil)
	}
	child.router = r
	r.services = append(r.services, child)
	r.paths[filepath.Clean(path)] = workspaceAlias{child, pinHistoryScope(path)}
	r.paths[child.boundary.root] = workspaceAlias{child, pinHistoryScope(child.boundary.root)}
	r.mu.Unlock()
	return child, nil
}

func overlappingRoots(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	rel, err = filepath.Rel(b, a)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func gitCommonIdentity(ctx context.Context, root string) (os.FileInfo, error) {
	dir, cut, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil || cut {
		return nil, bridgeError("workspace_mismatch", err)
	}
	path := strings.TrimSpace(dir)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return os.Stat(path)
}

func (s *ManualService) historyOwner(path string) (*ManualService, error) {
	if s.router == nil {
		return s, nil
	}
	s.router.mu.Lock()
	alias, known := s.router.paths[filepath.Clean(path)]
	s.router.mu.Unlock()
	if known {
		if err := alias.scope.check(); err != nil {
			return nil, err
		}
	}
	services := s.router.snapshot()
	for _, candidate := range services {
		for _, scope := range candidate.readScopes {
			if filepath.Clean(path) == filepath.Clean(scope.configured) {
				return candidate, scope.check()
			}
		}
	}
	for _, candidate := range services {
		if _, err := candidate.historyScope(path); err == nil {
			return candidate, nil
		}
	}
	return nil, bridgeError("read_workspace_not_allowed", nil)
}
