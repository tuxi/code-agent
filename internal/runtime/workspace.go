package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/mcp"
	"code-agent/internal/session"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// WorkspaceInstance holds workspace-scoped resources shared across conversations
// targeting the same project: the per-workspace store, skill registry, and — when
// the registry has MCP enabled — the workspace's own MCP manager and tool
// registry, built from <root>/.mcp.json. Built-in tools remain stateless and
// receive their workspace via ExecutionContext at call time; the per-workspace
// ToolReg exists because the MCP tool SET differs per workspace, not because
// built-in tool instances do.
type WorkspaceInstance struct {
	RootPath string
	SkillReg *skills.Registry
	Store    session.Store

	// MCPMgr owns this workspace's MCP server connections (subprocesses and remote
	// sessions). Nil when MCP is not enabled on the registry or the workspace
	// configures no servers.
	MCPMgr *mcp.Manager

	// ToolReg is the base registry cloned + this workspace's MCP tools. Nil when
	// the workspace adds no MCP tools — callers then use their base registry
	// directly (no pointless clone).
	ToolReg *tools.Registry

	// mcpOnce gates MCP connection so it happens exactly once, OUTSIDE the
	// registry's mutex — a cold `npx` server can take tens of seconds to start, and
	// holding the registry lock that long would stall every other workspace and the
	// event-reader endpoints.
	mcpOnce sync.Once

	// mcpCfg stores the EnableMCP parameters for this instance so hot-reload
	// (Phase 2b) can re-resolve .mcp.json without consulting the registry.
	mcpCfg *workspaceMCP

	// mcpMTime records the latest mod-time of .mcp.json/.mcp.local.json at the
	// last connection or reload. ServeRunBuilder.Build checks it per-turn.
	mcpMTime time.Time

	// mu serialises hot-reload against concurrent reads of MCPMgr/ToolReg/mcpMTime.
	mu sync.RWMutex

	// lastAccess feeds idle pruning (Phase 2a). Updated on every Get().
	lastAccess time.Time
}

// workspaceMCP carries everything buildInstance needs to resolve and connect a
// workspace's MCP servers. Set once via EnableMCP before the first Get.
type workspaceMCP struct {
	ctx           context.Context // daemon-lifetime; MCP sessions outlive one request
	base          *tools.Registry // cloned per workspace; MCP tools registered on top
	cfg           app.Config      // Agent.ToolAllowed gating + Profile (stdio filter)
	injected      []mcp.ServerConfig
	inheritClaude bool
}

// WorkspaceRegistry caches WorkspaceInstances by absolute workspace path. It also
// tracks session→workspace mappings so history endpoints can route to the correct
// per-workspace store. Built-in tools are global (shared across all workspaces);
// MCP tools are workspace-scoped when EnableMCP was called.
type WorkspaceRegistry struct {
	mu        sync.Mutex
	instances map[string]*WorkspaceInstance

	// sessionWorkspaces maps session IDs to workspace instances so event-reader
	// endpoints can resolve which store to query.
	sessionWorkspaces map[string]*WorkspaceInstance

	// globalSkillsDir is the optional user-level skills directory. Passed to
	// skills.Load as the first (global) dir; project-local skills override it.
	globalSkillsDir string

	// mcp, when non-nil, makes every instance resolve <root>/.mcp.json and connect
	// its servers on first access. Nil (tests, or callers that manage MCP globally)
	// leaves instances without MCPMgr/ToolReg.
	mcp *workspaceMCP
}

// NewWorkspaceRegistry creates a registry that builds instances on demand. Caller
// must call Close() to shut down all per-workspace stores (and MCP managers, when
// enabled). globalSkillsDir is the optional user-level skills directory (shared
// across workspaces); see app.Config.
//
// Get returns an error when workspacePath is empty — there is no default/false
// workspace. The daemon (codeagentd) requires every conversation to declare its
// workspace_path; the CLI (codeagent) always passes os.Getwd() as the workspace.
func NewWorkspaceRegistry(globalSkillsDir string) *WorkspaceRegistry {
	return &WorkspaceRegistry{
		instances:         make(map[string]*WorkspaceInstance),
		sessionWorkspaces: make(map[string]*WorkspaceInstance),
		globalSkillsDir:   globalSkillsDir,
	}
}

// EnableMCP turns on workspace-scoped MCP: every instance resolves its own
// <root>/.mcp.json (layered local > project > user, matching Claude Code) and
// connects those servers on first access, registering their tools onto a clone
// of base. injected servers (an embedded host's in-memory MCPJSON) participate at
// the LOWEST precedence, like an extra user-scope layer, so a workspace file can
// shadow them by name. Must be called before the first Get — instances built
// earlier would silently miss MCP.
//
// ctx should be daemon-scoped, not request-scoped: MCP sessions live until the
// workspace closes.
func (wr *WorkspaceRegistry) EnableMCP(ctx context.Context, base *tools.Registry, cfg app.Config, injected []mcp.ServerConfig, inheritClaude bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	wr.mcp = &workspaceMCP{
		ctx:           ctx,
		base:          base,
		cfg:           cfg,
		injected:      injected,
		inheritClaude: inheritClaude,
	}
}

// Get returns the WorkspaceInstance for the given path, creating it on first access.
// workspacePath is required — empty returns an error (there is no default workspace
// in daemon mode; the CLI always passes os.Getwd()). The returned instance is safe
// for concurrent use (immutable after creation).
func (wr *WorkspaceRegistry) Get(workspacePath string) (*WorkspaceInstance, error) {
	if workspacePath == "" {
		return nil, fmt.Errorf("workspace_registry: workspace_path is required")
	}
	abs, err := filepath.Abs(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("workspace_registry: abs(%q): %w", workspacePath, err)
	}
	root := abs

	wr.mu.Lock()
	inst, ok := wr.instances[root]
	var mcpCfg *workspaceMCP
	if !ok {
		inst, err = wr.buildInstance(root)
		if err != nil {
			wr.mu.Unlock()
			return nil, err
		}
		wr.instances[root] = inst
	}
	inst.lastAccess = time.Now()
	mcpCfg = wr.mcp
	wr.mu.Unlock()

	// Connect MCP outside the registry lock (see WorkspaceInstance.mcpOnce).
	// Concurrent Gets for the SAME workspace serialize on the Once — the loser
	// blocks until tools are ready, then reads a fully-built ToolReg (the Once
	// provides the happens-before edge). Gets for OTHER workspaces proceed freely.
	if mcpCfg != nil {
		inst.mcpOnce.Do(func() { inst.initMCP(mcpCfg) })
	}
	return inst, nil
}

// initMCP resolves this workspace's MCP config, connects its servers, and builds
// the workspace tool registry. Failures are logged, never fatal: MCP is an
// opt-in enhancement, and a broken .mcp.json (or a dead server) must not make the
// whole workspace unusable — the conversation still gets every built-in tool.
func (inst *WorkspaceInstance) initMCP(mc *workspaceMCP) {
	resolved, err := mcp.ResolveDesktop(inst.RootPath, mc.inheritClaude)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[workspace] %s: .mcp.json error (MCP disabled for this workspace): %v\n", inst.RootPath, err)
		resolved = mcp.Config{}
	}
	// Host-injected servers layer BELOW the workspace files: same-name workspace
	// entries win, mirroring how user scope sits below project scope.
	servers := mcp.Merge(mcp.Config{Servers: mc.injected}, resolved).Servers
	if !mc.cfg.Profile.AllowsSubprocess() {
		servers = mcp.RemoteServers(servers)
	}
	if len(servers) == 0 {
		return // no MCP for this workspace; ToolReg stays nil → callers use base
	}

	fmt.Fprintf(os.Stderr, "[workspace] %s: connecting %d MCP server(s)…\n", inst.RootPath, len(servers))
	mgr := mcp.NewManager(McpTraceWriter())
	if err := mgr.Connect(mc.ctx, servers); err != nil {
		// Connect only errors on setup problems; per-server failures are in Summary.
		fmt.Fprintf(os.Stderr, "[workspace] %s: MCP connect: %v\n", inst.RootPath, err)
		mgr.Close()
		return
	}
	if s := mgr.Summary(); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}

	reg := mc.base.Clone()
	for _, tool := range mgr.Tools() {
		if !mc.cfg.Agent.ToolAllowed(tool.Name()) {
			continue
		}
		if err := reg.Register(tool); err != nil {
			// Name collision with a built-in (or a duplicate across servers): skip the
			// remote tool, keep the workspace usable.
			fmt.Fprintf(os.Stderr, "[workspace] %s: skip MCP tool %s: %v\n", inst.RootPath, tool.Name(), err)
		}
	}
	inst.mu.Lock()
	inst.MCPMgr = mgr
	inst.ToolReg = reg
	inst.mcpCfg = mc
	inst.mcpMTime = latestMCPFileTime(inst.RootPath)
	inst.mu.Unlock()
}

// Prompts implements server.PromptService by collecting MCP prompts across ALL
// active workspace instances. The /v1/prompts endpoint carries no workspace
// context on the wire yet, so prompts from every workspace are returned —
// duplicates (same server+name) are deduplicated (first workspace wins).
// Per-workspace prompts need a wire protocol change.
func (wr *WorkspaceRegistry) Prompts() []mcp.PromptSpec {
	wr.mu.Lock()
	instances := make(map[string]*WorkspaceInstance, len(wr.instances))
	for k, v := range wr.instances {
		instances[k] = v
	}
	wr.mu.Unlock()

	seen := make(map[string]bool)
	var all []mcp.PromptSpec
	for _, inst := range instances {
		if inst.MCPMgr == nil {
			continue
		}
		for _, p := range inst.MCPMgr.Prompts() {
			if seen[p.Command] {
				continue
			}
			seen[p.Command] = true
			all = append(all, p)
		}
	}
	return all
}

// RenderPrompt implements server.PromptService. It searches all active
// workspace instances for the given command; the first MCP manager that
// carries it renders the prompt. Returns an error when no active workspace
// has a matching prompt.
func (wr *WorkspaceRegistry) RenderPrompt(ctx context.Context, command string, args []string) (string, error) {
	wr.mu.Lock()
	instances := make(map[string]*WorkspaceInstance, len(wr.instances))
	for k, v := range wr.instances {
		instances[k] = v
	}
	wr.mu.Unlock()

	for _, inst := range instances {
		if inst.MCPMgr == nil {
			continue
		}
		for _, p := range inst.MCPMgr.Prompts() {
			if p.Command == command {
				return inst.MCPMgr.RenderPrompt(ctx, command, args)
			}
		}
	}
	return "", fmt.Errorf("no active workspace has prompt %q", command)
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

// ── Phase 2b: hot-reload ──────────────────────────────────────────────

// latestMCPFileTime returns the most recent modification time of .mcp.json
// and .mcp.local.json in root. A missing file contributes the zero time.
func latestMCPFileTime(root string) time.Time {
	var latest time.Time
	for _, name := range []string{".mcp.json", ".mcp.local.json"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil {
			if t := fi.ModTime(); t.After(latest) {
				latest = t
			}
		}
	}
	return latest
}

// CheckReloadMCP reports true when the workspace's .mcp.json or
// .mcp.local.json have been modified since the last connection or reload.
func (inst *WorkspaceInstance) CheckReloadMCP() bool {
	inst.mu.RLock()
	last := inst.mcpMTime
	inst.mu.RUnlock()
	if last.IsZero() {
		return false
	}
	return latestMCPFileTime(inst.RootPath).After(last)
}

// ReloadMCP re-reads the workspace .mcp.json, tears down stale servers,
// starts new ones, and atomically swaps ToolReg. On failure the previous
// state stays intact; in-flight turns see the old registry until they end.
func (inst *WorkspaceInstance) ReloadMCP() (changed bool) {
	mc := inst.mcpCfg
	if mc == nil {
		return false
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()

	newTime := latestMCPFileTime(inst.RootPath)
	if !newTime.After(inst.mcpMTime) {
		return false
	}

	resolved, err := mcp.ResolveDesktop(inst.RootPath, mc.inheritClaude)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[workspace] %s: .mcp.json reload error: %v\n", inst.RootPath, err)
		return false
	}
	servers := mcp.Merge(mcp.Config{Servers: mc.injected}, resolved).Servers
	if !mc.cfg.Profile.AllowsSubprocess() {
		servers = mcp.RemoteServers(servers)
	}

	newMgr := mcp.NewManager(McpTraceWriter())
	if len(servers) > 0 {
		if err := newMgr.Connect(mc.ctx, servers); err != nil {
			fmt.Fprintf(os.Stderr, "[workspace] %s: MCP reload connect: %v\n", inst.RootPath, err)
			newMgr.Close()
			return false
		}
	}
	if s := newMgr.Summary(); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}

	newReg := mc.base.Clone()
	for _, tool := range newMgr.Tools() {
		if !mc.cfg.Agent.ToolAllowed(tool.Name()) {
			continue
		}
		if err := newReg.Register(tool); err != nil {
			fmt.Fprintf(os.Stderr, "[workspace] %s: skip MCP tool %s on reload: %v\n", inst.RootPath, tool.Name(), err)
		}
	}

	oldMgr := inst.MCPMgr
	inst.MCPMgr = newMgr
	inst.ToolReg = newReg
	inst.mcpMTime = newTime
	if oldMgr != nil {
		oldMgr.Close()
	}
	fmt.Fprintf(os.Stderr, "[workspace] %s: MCP reloaded (%d servers)\n", inst.RootPath, len(servers))
	return true
}

// ── Phase 2a: idle prune ─────────────────────────────────────────────

// PruneIdle closes MCP managers for workspaces whose last Get() was more
// than maxAge ago. Stores and instances are NOT evicted — only subprocess
// resources are released. A subsequent Get() reconnects on demand.
func (wr *WorkspaceRegistry) PruneIdle(maxAge time.Duration) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for root, inst := range wr.instances {
		if inst.lastAccess.Before(cutoff) && inst.MCPMgr != nil {
			inst.mu.Lock()
			inst.MCPMgr.Close()
			inst.MCPMgr = nil
			inst.ToolReg = nil
			inst.mcpCfg = nil
			inst.mcpMTime = time.Time{}
			inst.mcpOnce = sync.Once{}
			inst.mu.Unlock()
			fmt.Fprintf(os.Stderr, "[workspace] %s: MCP pruned (idle)\n", root)
		}
	}
}

// ReloadWorkspace triggers an explicit MCP reload for the named workspace.
// It is a no-op when the workspace is not loaded or has no MCP enabled.
func (wr *WorkspaceRegistry) ReloadWorkspace(workspacePath string) error {
	abs, err := filepath.Abs(workspacePath)
	if err != nil {
		return err
	}
	wr.mu.Lock()
	inst, ok := wr.instances[abs]
	wr.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace_registry: workspace not loaded: %s", abs)
	}
	if !inst.ReloadMCP() {
		return fmt.Errorf("workspace_registry: MCP reload for %s: no change or not enabled", abs)
	}
	return nil
}

// Close closes every per-workspace store and MCP manager.
func (wr *WorkspaceRegistry) Close() error {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	var firstErr error
	for root, inst := range wr.instances {
		// Synchronize with an in-flight initMCP: Do blocks until a running init
		// completes (or is a no-op if none ran), so the MCPMgr read below never
		// misses a manager created concurrently with shutdown — which would leak
		// its server subprocesses.
		inst.mcpOnce.Do(func() {})
		if inst.MCPMgr != nil {
			if err := inst.MCPMgr.Close(); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("close mcp for %s: %w", root, err)
			}
		}
		if err := inst.Store.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close store for %s: %w", root, err)
		}
	}
	wr.instances = nil
	return firstErr
}

// buildInstance assembles a WorkspaceInstance for a single root — store and
// skills only; MCP connects lazily in initMCP, outside the registry lock. It
// must be called with wr.mu held.
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
