package mcpbridge

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ManualService owns immutable routing handles, not a current Session. opMu
// serializes write admission/replacement; reads never hold it while waiting.
type ManualService struct {
	ctx                      context.Context
	cancel                   context.CancelFunc
	manifest, configuredRoot string
	boundary                 *Client
	boundaryErr              error
	opMu                     sync.Mutex
	mu                       sync.RWMutex
	anchor                   *connectionBinding
	bindings                 map[string]*manualBinding
	sessions                 map[string]string
	creations                map[string]*manualCreation
}

type manualBinding struct {
	mu      sync.RWMutex
	id      string
	c       *Client
	retired bool
}

type manualCreation struct {
	in  ManualCreateInput
	ref *ManualBindingRef
}

type ManualIdentity struct {
	ServerID string `json:"server_id"`
	StoreID  string `json:"store_id"`
	Baseline string `json:"baseline_commit"`
}

type ManualCreateInput struct {
	ManualIdentity
	RequestID string `json:"request_id"`
}

type ManualBindInput struct {
	ManualIdentity
	SessionID        string `json:"session_id"`
	ReplaceBindingID string `json:"replace_binding_id,omitempty"`
}

type ManualBindingRef struct {
	ManualIdentity
	BindingID string `json:"binding_id"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace_path"`
}

type ManualConnectionInfo struct {
	ManualBindingRef
	IdentityVerified            bool   `json:"identity_verified"`
	ExecutionBound              bool   `json:"execution_bound"`
	HeadCommit                  string `json:"head_commit,omitempty"`
	Reconnected                 bool   `json:"reconnected,omitempty"`
	BindingsRequireRevalidation bool   `json:"bindings_require_revalidation,omitempty"`
}

// NewManualService performs local boundary inspection only. A missing grant,
// offline daemon or unavailable workspace must not break MCP initialization.
func NewManualService(ctx context.Context, manifest, workspace string) (*ManualService, error) {
	if !filepath.IsAbs(manifest) || !filepath.IsAbs(workspace) {
		return nil, bridgeError("invalid_tool_arguments", nil)
	}
	runCtx, cancel := context.WithCancel(ctx)
	s := &ManualService{ctx: runCtx, cancel: cancel, manifest: manifest, configuredRoot: workspace,
		bindings: map[string]*manualBinding{}, sessions: map[string]string{}, creations: map[string]*manualCreation{}}
	s.boundary, s.boundaryErr = pinManualBoundary(ctx, workspace)
	return s, nil
}

func pinManualBoundary(ctx context.Context, path string) (*Client, error) {
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, bridgeError("workspace_mismatch", err)
	}
	top, cut, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil || cut {
		return nil, bridgeError("workspace_mismatch", err)
	}
	canonical, err := filepath.EvalSymlinks(strings.TrimSpace(top))
	if err != nil || canonical != root {
		return nil, bridgeError("workspace_mismatch", err)
	}
	gitDir, cut, err := gitOutput(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil || cut {
		return nil, bridgeError("workspace_mismatch", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, bridgeError("workspace_mismatch", err)
	}
	gitInfo, err := os.Stat(strings.TrimSpace(gitDir))
	if err != nil {
		return nil, bridgeError("workspace_mismatch", err)
	}
	return &Client{root: root, rootInfo: rootInfo, gitDir: strings.TrimSpace(gitDir), gitInfo: gitInfo}, nil
}

func (s *ManualService) checkBoundary(ctx context.Context) error {
	if s.ctx.Err() != nil {
		return bridgeError("connection_closed", nil)
	}
	if s.boundaryErr != nil {
		return s.boundaryErr
	}
	current, err := pinManualBoundary(ctx, s.configuredRoot)
	if err != nil {
		return err
	}
	pinned := s.boundary
	if current.root != pinned.root || current.gitDir != pinned.gitDir || !os.SameFile(current.rootInfo, pinned.rootInfo) || !os.SameFile(current.gitInfo, pinned.gitInfo) {
		return bridgeError("workspace_mismatch", nil)
	}
	return nil
}

func (s *ManualService) Close() {
	s.cancel()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.bindings {
		b.c.Close()
	}
}

func (s *ManualService) validateClientBoundary(ctx context.Context, c *Client) error {
	pinned := s.boundary
	if c.root != pinned.root || c.gitDir != pinned.gitDir || !os.SameFile(c.rootInfo, pinned.rootInfo) || !os.SameFile(c.gitInfo, pinned.gitInfo) {
		return bridgeError("workspace_mismatch", nil)
	}
	return s.checkBoundary(ctx)
}

// probe has no Session and can only be used for explicitly coded read routes.
// It does not adopt a replacement grant; Bind or explicit Reconnect publishes it.
func (s *ManualService) probe(ctx context.Context, replacement bool) (*Client, error) {
	return s.probeIdentity(ctx, replacement, nil)
}

func (s *ManualService) probeIdentity(ctx context.Context, replacement bool, expected *ManualReconnectInput) (*Client, error) {
	if err := s.checkBoundary(ctx); err != nil {
		return nil, err
	}
	m, digest, err := loadConnection(s.manifest)
	if err != nil {
		return nil, err
	}
	if m.StoreID == "" {
		return nil, bridgeError("identity_mismatch", nil)
	}
	if expected != nil && (m.ServerID != expected.ServerID || m.StoreID != expected.StoreID) {
		return nil, bridgeError("identity_mismatch", nil)
	}
	u, err := parseEndpoint(m.Endpoint)
	if err != nil {
		return nil, bridgeError("grant_invalid", err)
	}
	runCtx, cancel := context.WithCancel(s.ctx)
	c := &Client{base: u, token: m.AccessToken, ctx: runCtx, cancel: cancel,
		root: s.boundary.root, rootInfo: s.boundary.rootInfo, gitDir: s.boundary.gitDir, gitInfo: s.boundary.gitInfo,
		http:    &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("runtime redirects are disabled") }},
		binding: &connectionBinding{path: s.manifest, digest: digest, verifiedServerID: m.ServerID, verifiedStoreID: m.StoreID}}
	if err := c.verifyRuntimeIdentity(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.checkBinding(); err != nil {
		c.Close()
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor == nil {
		copy := *c.binding
		s.anchor = &copy
	} else if s.anchor.verifiedServerID != m.ServerID || s.anchor.verifiedStoreID != m.StoreID {
		c.Close()
		return nil, bridgeError("identity_mismatch", nil)
	} else if s.anchor.digest != digest && !replacement {
		c.Close()
		return nil, bridgeError("grant_replaced", nil)
	}
	return c, nil
}

func validManualIdentity(in ManualIdentity) bool {
	return validID(in.ServerID) && validID(in.StoreID) && regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`).MatchString(in.Baseline)
}

func matchesManualIdentity(c *Client, in ManualIdentity) bool {
	return c.binding != nil && c.binding.verifiedServerID == in.ServerID && c.binding.verifiedStoreID == in.StoreID
}

func (b *manualBinding) ref() ManualBindingRef {
	return ManualBindingRef{ManualIdentity: ManualIdentity{ServerID: b.c.binding.verifiedServerID, StoreID: b.c.binding.verifiedStoreID, Baseline: b.c.baseline},
		BindingID: b.id, ID: b.c.id, SessionID: b.c.id, Workspace: b.c.root}
}

func (s *ManualService) publish(c *Client) *manualBinding {
	b := &manualBinding{id: uuid.NewString(), c: c}
	s.mu.Lock()
	s.bindings[b.id], s.sessions[c.id] = b, b.id
	copy := *c.binding
	s.anchor = &copy
	s.mu.Unlock()
	return b
}

func (s *ManualService) acquire(ctx context.Context, binding, session string) (*manualBinding, func(), error) {
	s.mu.RLock()
	b := s.bindings[binding]
	s.mu.RUnlock()
	if b == nil {
		return nil, nil, bridgeError("binding_not_found", nil)
	}
	b.mu.RLock()
	fail := func(err error) (*manualBinding, func(), error) { b.mu.RUnlock(); return nil, nil, err }
	if b.retired {
		return fail(bridgeError("binding_not_found", nil))
	}
	if session != "" && b.c.id != session {
		return fail(bridgeError("session_mismatch", nil))
	}
	if err := s.checkBoundary(ctx); err != nil {
		return fail(err)
	}
	if err := b.c.verifyRuntimeIdentity(ctx); err != nil {
		return fail(err)
	}
	return b, b.mu.RUnlock, nil
}

func (s *ManualService) ConnectionInfo(ctx context.Context, binding string) (ManualConnectionInfo, error) {
	if binding != "" {
		b, release, err := s.acquire(ctx, binding, "")
		if err != nil {
			return ManualConnectionInfo{}, err
		}
		defer release()
		return ManualConnectionInfo{ManualBindingRef: b.ref(), IdentityVerified: true, ExecutionBound: true}, nil
	}
	c, err := s.probe(ctx, false)
	if err != nil {
		return ManualConnectionInfo{}, err
	}
	defer c.Close()
	return s.connectionInfo(ctx, c)
}

func (s *ManualService) Create(ctx context.Context, in ManualCreateInput) (ManualBindingRef, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !validID(in.RequestID) || !validManualIdentity(in.ManualIdentity) {
		return ManualBindingRef{}, bridgeError("invalid_tool_arguments", nil)
	}
	probe, err := s.probe(ctx, false)
	if err != nil {
		return ManualBindingRef{}, err
	}
	defer probe.Close()
	if !matchesManualIdentity(probe, in.ManualIdentity) {
		return ManualBindingRef{}, bridgeError("identity_mismatch", nil)
	}
	if previous := s.creations[in.RequestID]; previous != nil {
		if previous.in != in {
			return ManualBindingRef{}, admissionError("request_conflict", "", in.RequestID, nil)
		}
		if previous.ref == nil {
			return ManualBindingRef{}, admissionError("create_unknown", "", in.RequestID, nil)
		}
		b, release, err := s.acquire(ctx, previous.ref.BindingID, previous.ref.ID)
		if err != nil {
			return ManualBindingRef{}, err
		}
		defer release()
		return b.ref(), nil
	}
	if len(s.bindings) >= 64 {
		return ManualBindingRef{}, bridgeError("binding_limit", nil)
	}
	if err := s.checkOtherWriters(ctx, probe, ""); err != nil {
		return ManualBindingRef{}, err
	}
	c, _, err := newFromManifestWithLifetime(s.ctx, ctx, s.manifest, s.boundary.root, "", "")
	if err != nil {
		return ManualBindingRef{}, err
	}
	if err := s.validateClientBoundary(ctx, c); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if c.binding.digest != probe.binding.digest || !matchesManualIdentity(c, in.ManualIdentity) {
		c.Close()
		return ManualBindingRef{}, bridgeError("grant_replaced", nil)
	}
	if c.baseline != in.Baseline {
		c.Close()
		return ManualBindingRef{}, bridgeError("baseline_changed", nil)
	}
	creation := &manualCreation{in: in}
	s.creations[in.RequestID] = creation
	if _, err := c.Create(ctx, in.RequestID); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if err := c.checkBinding(); err != nil {
		c.Close()
		return ManualBindingRef{}, admissionError("create_unknown", "", in.RequestID, err)
	}
	if err := ctx.Err(); err != nil {
		c.Close()
		return ManualBindingRef{}, admissionError("create_unknown", "", in.RequestID, err)
	}
	ref := s.publish(c).ref()
	creation.ref = &ref
	return ref, nil
}

func (s *ManualService) Bind(ctx context.Context, in ManualBindInput) (ManualBindingRef, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !validID(in.SessionID) || !validManualIdentity(in.ManualIdentity) {
		return ManualBindingRef{}, bridgeError("invalid_tool_arguments", nil)
	}
	s.mu.RLock()
	old := s.bindings[s.sessions[in.SessionID]]
	replace := s.bindings[in.ReplaceBindingID]
	s.mu.RUnlock()
	if in.ReplaceBindingID != "" && (old == nil || old != replace) {
		return ManualBindingRef{}, bridgeError("binding_not_found", nil)
	}
	if old != nil {
		old.mu.Lock()
		defer old.mu.Unlock()
		if old.c.baseline != in.Baseline {
			return ManualBindingRef{}, bridgeError("baseline_changed", nil)
		}
	}
	p, err := s.probe(ctx, in.ReplaceBindingID != "")
	if err != nil {
		return ManualBindingRef{}, err
	}
	defer p.Close()
	if !matchesManualIdentity(p, in.ManualIdentity) {
		return ManualBindingRef{}, bridgeError("identity_mismatch", nil)
	}
	if old != nil && in.ReplaceBindingID == "" {
		if err := old.c.checkBinding(); err != nil {
			return ManualBindingRef{}, err
		}
		return old.ref(), nil
	}
	if len(s.bindings) >= 64 {
		return ManualBindingRef{}, bridgeError("binding_limit", nil)
	}
	c, _, err := newFromManifestWithLifetime(s.ctx, ctx, s.manifest, s.boundary.root, in.Baseline, "")
	if err != nil {
		return ManualBindingRef{}, err
	}
	if err := s.validateClientBoundary(ctx, c); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if c.binding.digest != p.binding.digest || !matchesManualIdentity(c, in.ManualIdentity) {
		c.Close()
		return ManualBindingRef{}, bridgeError("grant_replaced", nil)
	}
	// Check the caller's exact identity before reading any Session. A grant
	// rotated between probe and construction must not retarget even a read.
	if err := c.reattach(ctx, in.SessionID); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if err := c.checkBinding(); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if err := s.validateClientBoundary(ctx, c); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if err := ctx.Err(); err != nil {
		c.Close()
		return ManualBindingRef{}, err
	}
	if old != nil {
		// Retain unresolved requests across grant changes. A fresh Client must
		// never erase the fact that a previous side effect may have happened.
		for key, previous := range old.c.submissions {
			copy := *previous
			if recovered := c.submissions[key]; recovered != nil && recovered.receipt != nil {
				copy.receipt, copy.recovered = recovered.receipt, true
			}
			c.submissions[key] = &copy
		}
		old.c.mu.Lock()
		for turn, expected := range old.c.expectations {
			c.expectations[turn] = expected
		}
		old.c.mu.Unlock()
		old.retired = true
		old.c.Close()
	}
	return s.publish(c).ref(), nil
}

// checkOtherWriters only inspects this service's known Sessions. It is not an
// OS isolation claim or a replacement for the daemon's managed admission.
func (s *ManualService) checkOtherWriters(ctx context.Context, probe *Client, except string) error {
	for request, creation := range s.creations {
		if creation.ref == nil {
			return admissionError("create_unknown", "", request, nil)
		}
	}
	s.mu.RLock()
	var others []*manualBinding
	for session, id := range s.sessions {
		if session != except {
			others = append(others, s.bindings[id])
		}
	}
	s.mu.RUnlock()
	for _, b := range others {
		b.c.sendMu.Lock()
		known := make(map[string]*Receipt, len(b.c.submissions))
		for request, previous := range b.c.submissions {
			if previous.receipt != nil {
				copy := *previous.receipt
				known[request] = &copy
			} else {
				known[request] = nil
			}
		}
		b.c.sendMu.Unlock()
		events, err := probe.events(ctx, b.c.id, 0)
		if err != nil {
			return err
		}
		for request, receipt := range known {
			turn := ""
			if receipt != nil {
				turn = receipt.TurnID
			}
			for _, e := range events {
				if e.Kind == "turn_accepted" && e.RequestID == request {
					turn = e.TurnID
				}
			}
			if turn == "" {
				return admissionError("admission_unknown", b.c.id, request, nil)
			}
			done := false
			for _, e := range events {
				if e.TurnID == turn && (e.Kind == "turn_finished" || e.Kind == "turn_failed" || e.Kind == "turn_cancelled") {
					done = true
				}
			}
			if !done {
				return admissionError("session_busy", b.c.id, request, nil)
			}
		}
		// Also check resumed/queued work not sent through this process.
		if err := probe.sessionIdle(ctx, b.c.id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sessionIdle(ctx context.Context, session string) error {
	var activity struct {
		Delta    *bool `json:"is_delta"`
		Sessions *[]struct {
			ID        string `json:"session_id"`
			State     string `json:"state"`
			Approvals *int   `json:"pending_approval_count"`
			Tools     *int   `json:"pending_client_tool_count"`
		} `json:"sessions"`
	}
	if err := c.request(ctx, "GET", "/v1/activity", nil, &activity); err != nil {
		return err
	}
	if activity.Delta == nil || *activity.Delta || activity.Sessions == nil {
		return bridgeError("evidence_incomplete", nil)
	}
	matches := 0
	for _, a := range *activity.Sessions {
		if a.ID != session {
			continue
		}
		matches++
		if (a.State != "idle" && a.State != "done" && a.State != "failed" && a.State != "cancelled") || a.Approvals == nil || *a.Approvals != 0 || a.Tools == nil || *a.Tools != 0 {
			return bridgeError("session_busy", nil)
		}
	}
	if matches > 1 {
		return bridgeError("evidence_incomplete", nil)
	}
	return nil
}
