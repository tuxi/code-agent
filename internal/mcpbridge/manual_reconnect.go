package mcpbridge

import (
	"context"
	"strings"
)

type ManualReconnectInput struct {
	ServerID  string `json:"server_id"`
	StoreID   string `json:"store_id"`
	Workspace string `json:"workspace_path"`
}

type manualConnectionInput struct {
	BindingID string                `json:"binding_id,omitempty"`
	Workspace string                `json:"workspace_path,omitempty"`
	Reconnect *ManualReconnectInput `json:"reconnect,omitempty"`
}

// Reconnect adopts a user-renewed grant only for the previously verified service
// and pinned workspace. Session Clients and uncertain admissions stay untouched;
// their existing explicit Bind replacement path performs separate revalidation.
func (s *ManualService) Reconnect(ctx context.Context, in ManualReconnectInput) (ManualConnectionInfo, error) {
	owner, err := s.executionWorkspace(ctx, in.Workspace, false)
	if err != nil {
		return ManualConnectionInfo{}, err
	}
	if owner != s {
		return owner.Reconnect(ctx, in)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !validID(in.ServerID) || !validID(in.StoreID) || in.Workspace == "" {
		return ManualConnectionInfo{}, bridgeError("invalid_tool_arguments", nil)
	}
	if err := s.checkBoundary(ctx); err != nil {
		return ManualConnectionInfo{}, err
	}
	if in.Workspace != s.boundary.root {
		return ManualConnectionInfo{}, bridgeError("workspace_mismatch", nil)
	}
	s.authorityMu.Lock()
	anchor := s.anchor
	s.authorityMu.Unlock()
	if anchor == nil || anchor.verifiedServerID != in.ServerID || anchor.verifiedStoreID != in.StoreID {
		return ManualConnectionInfo{}, bridgeError("identity_mismatch", nil)
	}
	// Refuse a different local target before sending even an identity request.
	c, err := s.probeIdentity(ctx, true, &in)
	if err != nil {
		return ManualConnectionInfo{}, err
	}
	defer c.Close()
	info, err := s.connectionInfo(ctx, c)
	if err != nil {
		return ManualConnectionInfo{}, err
	}
	if c.binding.verifiedServerID != in.ServerID || c.binding.verifiedStoreID != in.StoreID {
		return ManualConnectionInfo{}, bridgeError("identity_mismatch", nil)
	}
	s.authorityMu.Lock()
	if err := c.checkBinding(); err != nil {
		s.authorityMu.Unlock()
		return ManualConnectionInfo{}, err
	}
	copy := *c.binding
	s.anchor = &copy
	s.authorityMu.Unlock()
	services := []*ManualService{s}
	if s.router != nil {
		services = s.router.snapshot()
	}
	for _, service := range services {
		service.mu.RLock()
		bindings := make([]*manualBinding, 0, len(service.bindings))
		for _, b := range service.bindings {
			bindings = append(bindings, b)
		}
		service.mu.RUnlock()
		for _, b := range bindings {
			b.mu.RLock()
			if !b.retired && b.c.binding.digest != copy.digest {
				info.BindingsRequireRevalidation = true
			}
			b.mu.RUnlock()
		}
	}
	info.Reconnected = true
	return info, nil
}

func (s *ManualService) connectionInfo(ctx context.Context, c *Client) (ManualConnectionInfo, error) {
	head, cut, err := gitOutput(ctx, c.root, "rev-parse", "HEAD")
	if err != nil || cut {
		return ManualConnectionInfo{}, bridgeError("workspace_mismatch", err)
	}
	if err := s.validateClientBoundary(ctx, c); err != nil {
		return ManualConnectionInfo{}, err
	}
	if err := c.checkBinding(); err != nil {
		return ManualConnectionInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return ManualConnectionInfo{}, err
	}
	info := ManualConnectionInfo{ManualBindingRef: ManualBindingRef{ManualIdentity: ManualIdentity{ServerID: c.binding.verifiedServerID, StoreID: c.binding.verifiedStoreID}, Workspace: c.root}, IdentityVerified: true, WorkspaceSelection: s.router != nil, HeadCommit: strings.TrimSpace(head)}
	services := []*ManualService{s}
	if s.router != nil {
		services = s.router.snapshot()
	}
	seen := map[string]bool{}
	for _, service := range services {
		if service.boundaryErr == nil {
			info.ExecutionWorkspaces = append(info.ExecutionWorkspaces, service.boundary.root)
		}
		for _, scope := range service.readScopes {
			if scope.err == nil && !seen[scope.root] {
				info.ReadWorkspaces = append(info.ReadWorkspaces, scope.root)
				seen[scope.root] = true
			}
		}
	}
	return info, nil
}
