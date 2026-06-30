package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"code-agent/internal/session"
	"code-agent/internal/skills"
)

// WorkspaceInstance holds workspace-scoped resources shared across conversations
// targeting the same project. Tools are NOT workspace-scoped (they receive their
// workspace via ExecutionContext at call time), so this caches only the
// per-workspace store and skill registry.
type WorkspaceInstance struct {
	RootPath string
	SkillReg *skills.Registry
	Store    session.Store
}

// WorkspaceRegistry caches WorkspaceInstances by absolute workspace path. It also
// tracks session→workspace mappings so history endpoints can route to the correct
// per-workspace store. Tools are global (shared across all workspaces) and managed
// separately by the caller.
type WorkspaceRegistry struct {
	mu        sync.Mutex
	instances map[string]*WorkspaceInstance

	// sessionWorkspaces maps session IDs to workspace instances so event-reader
	// endpoints can resolve which store to query.
	sessionWorkspaces map[string]*WorkspaceInstance

	// defaultRoot is the server's configured default workspace (cfg.Workspace.Root),
	// used as a fallback when Get receives an empty workspacePath.
	defaultRoot string

	// globalSkillsDir is the optional user-level skills directory. Passed to
	// skills.Load as the first (global) dir; project-local skills override it.
	globalSkillsDir string
}

// NewWorkspaceRegistry creates a registry that builds instances on demand. Caller
// must call Close() to shut down all per-workspace stores. globalSkillsDir is the
// optional user-level skills directory (shared across workspaces); see app.Config.
func NewWorkspaceRegistry(defaultRoot, globalSkillsDir string) *WorkspaceRegistry {
	return &WorkspaceRegistry{
		instances:         make(map[string]*WorkspaceInstance),
		sessionWorkspaces: make(map[string]*WorkspaceInstance),
		defaultRoot:       defaultRoot,
		globalSkillsDir:   globalSkillsDir,
	}
}

// Get returns the WorkspaceInstance for the given path, creating it on first access.
// If workspacePath is empty, it falls back to the server's default workspace. The
// returned instance is safe for concurrent use (immutable after creation).
func (wr *WorkspaceRegistry) Get(workspacePath string) (*WorkspaceInstance, error) {
	root := workspacePath
	if root == "" {
		root = wr.defaultRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace_registry: abs(%q): %w", root, err)
	}
	root = abs

	wr.mu.Lock()
	defer wr.mu.Unlock()

	if inst, ok := wr.instances[root]; ok {
		return inst, nil
	}

	inst, err := wr.buildInstance(root)
	if err != nil {
		return nil, err
	}
	wr.instances[root] = inst
	return inst, nil
}

// RecordSession records the session→workspace mapping.
func (wr *WorkspaceRegistry) RecordSession(sessionID string, inst *WorkspaceInstance) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	wr.sessionWorkspaces[sessionID] = inst
}

// SessionEvents implements server.EventSource by routing the session ID to the
// correct per-workspace store.
func (wr *WorkspaceRegistry) SessionEvents(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	wr.mu.Lock()
	inst, ok := wr.sessionWorkspaces[sessionID]
	wr.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return inst.Store.SessionEvents(ctx, sessionID)
}

// Close closes every per-workspace store.
func (wr *WorkspaceRegistry) Close() error {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	var firstErr error
	for root, inst := range wr.instances {
		if err := inst.Store.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close store for %s: %w", root, err)
		}
	}
	wr.instances = nil
	return firstErr
}

// buildInstance assembles a WorkspaceInstance for a single root. It must be called
// with wr.mu held.
func (wr *WorkspaceRegistry) buildInstance(root string) (*WorkspaceInstance, error) {
	store, err := OpenStore(root)
	if err != nil {
		return nil, fmt.Errorf("workspace_registry: open store for %s: %w", root, err)
	}

	skillReg, err := skills.Load(wr.globalSkillsDir, filepath.Join(root, "skills"))
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("workspace_registry: load skills for %s: %w", root, err)
	}

	fmt.Fprintf(os.Stderr, "[workspace] initialized %s (%d skills)\n",
		root, skillReg.Len())
	if len(skillReg.Skipped) > 0 {
		for label, reason := range skillReg.Skipped {
			fmt.Fprintf(os.Stderr, "[workspace]   skipped skill %q: %s\n", label, reason)
		}
	}

	return &WorkspaceInstance{
		RootPath: root,
		SkillReg: skillReg,
		Store:    store,
	}, nil
}
