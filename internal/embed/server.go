// Package embed runs the codeagent agent-wire server in-process, for hosts that
// link the runtime as a library instead of launching the `codeagent serve` CLI.
//
// This is the entry point used by the iOS/macOS app: the Swift side (AgentKit)
// calls StartServer to bring up the runtime bound to the loopback interface, then
// connects to it over the same HTTP/WS agent-wire protocol it would use against a
// remote Mac server. Config and secrets are injected in-memory (Options) because
// the app sandbox has no fixed config.yaml and no shell environment to read keys
// from. The assembly here mirrors cmd/codeagent.runServe; the two should evolve
// together.
package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/mcp"
	"code-agent/internal/model"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
)

// defaultCapabilities is the capability set advertised in the WebSocket hello
// handshake. Kept in sync with cmd/codeagent.defaultCapabilities — it is a static
// contract clients use to decide which protocol features to enable.
var defaultCapabilities = []string{
	"streaming",
	"thinking",
	"tool_streaming",
	"plan_mode",
	"subagents",
	"session_resume",
	"client_tool_execution",
}

// Options configures an embedded server. Every field is supplied in-memory by the
// host; nothing is read from disk or the environment except what the resolved
// config explicitly points at.
type Options struct {
	// WorkspaceDir is the agent's working root. On iOS this is the app container
	// (e.g. the Documents directory). Overrides whatever the config declares.
	WorkspaceDir string

	// ConfigYAML is the raw config document. Empty => built-in defaults
	// (see app.LoadConfigBytes).
	ConfigYAML string

	// MCPJSON is the raw Claude-compatible `.mcp.json` document configuring
	// external MCP servers ({"mcpServers": {...}}). The desktop backend reads this
	// from the workspace-root file; embedded hosts (iOS/macOS) have no fixed path,
	// so they inject it here, the same way ConfigYAML carries the main config.
	// Empty => no MCP servers. On a sandboxed (iOS) host, stdio servers are still
	// skipped — only http/sse servers connect (they need no subprocess).
	MCPJSON string

	// ModelName selects which configured model to use. Empty => default_model.
	ModelName string

	// Secrets supplies API keys without using environment variables. Keys may be
	// matched either by a model's api_key_env name or by the model's friendly
	// name; the value becomes that model's resolved API key. Intended to carry
	// secrets pulled from the iOS Keychain.
	Secrets map[string]string

	// Addr is the listen address. Empty => "127.0.0.1:0", i.e. an OS-assigned
	// ephemeral port on the loopback interface; read it back via Handle.Port.
	Addr string

	// Sandboxed selects the sandboxed capability profile (iOS): subprocess-based
	// tools (shell, git, gopls), MCP stdio servers, flux, and hooks are not
	// assembled. A non-sandboxed macOS app host leaves this false to get the full
	// desktop toolset.
	Sandboxed bool

	// DataDir is a writable directory for the runtime's own data (session
	// databases). On iOS the desktop default ($HOME/.codeagent) is unusable because
	// $HOME is the read-only app container, so the host must pass a writable path —
	// canonically Library/Application Support. Empty => fall back to WorkspaceDir;
	// if that is also empty, the desktop $HOME default is used.
	DataDir string
}

// Runtime is the set of live components Assemble builds that the lifecycle verbs
// (Suspend / ResumeSession / Reconfigure) operate on, distinct from the HTTP
// handler. Assemble returns it so the embedded Handle can drive suspend/resume and
// hot-reload; the CLI serve path ignores it (it uses process lifecycle).
type Runtime struct {
	Executor *conversation.TurnExecutor
	Builder  *runtime.ServeRunBuilder
	Repo     conversation.ConversationRepository
}

// Handle is a running embedded server. The host must call Stop to release the
// listener, the MCP subprocesses, and the SQLite stores.
type Handle struct {
	srv      *http.Server
	lis      net.Listener
	port     int
	cancel   context.CancelFunc
	closers  []func() // run in reverse on Stop, mirroring runServe's defers
	serveErr chan error

	// Lifecycle state (v1.2). srvCtx is the server-scoped context resumed turns run
	// under (so Stop cancels them); cfg + rt back Suspend/ResumeSession/Reconfigure.
	srvCtx context.Context
	cfg    app.Config
	rt     *Runtime
}

// Port returns the actual TCP port the server is listening on. With Addr empty
// this is the OS-assigned ephemeral port the host should hand to AgentKit.
func (h *Handle) Port() int { return h.port }

// Stop shuts the server down and releases every resource acquired by StartServer.
// It is safe to call once; further calls are no-ops.
func (h *Handle) Stop() error {
	if h.srv == nil {
		return nil
	}
	err := h.srv.Close()
	if h.cancel != nil {
		h.cancel()
	}
	for i := len(h.closers) - 1; i >= 0; i-- {
		h.closers[i]()
	}
	h.srv = nil
	return err
}

// suspendTimeout bounds how long Suspend waits for in-flight turns to unwind. The
// host runs its own (shorter) background watchdog; correctness does not depend on
// this completing (the per-iteration checkpoint already persisted a consistent
// history), so this is only an upper bound on the paused-status flush.
const suspendTimeout = 2 * time.Second

// Suspend cancels every in-flight turn as an app suspend and records each as
// paused, returning once they have unwound (bounded by suspendTimeout) — the host
// calls it in its background grace window instead of Stop (v1.2 §3.1). It does NOT
// tear down the server: the process stays resumable on return to the foreground.
// Safe to call when idle (no-op) and repeatedly (idempotent).
func (h *Handle) Suspend() error {
	if h.rt == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), suspendTimeout)
	defer cancel()
	h.rt.Executor.SuspendAll(ctx)
	return nil
}

// ResumeSession continues a paused turn for the given session (v1.2 §3.2). It
// validates the session exists, then drives the resume ASYNCHRONOUSLY under the
// server-scoped context and returns immediately: progress and outcome flow over
// the event stream (turn_resumed / turn_finished / turn_paused / turn_failed) and
// the turn_status field, not this call's return. A resume of a session already
// running is a no-op. The error covers only failure to START (unknown session).
func (h *Handle) ResumeSession(sessionID string) error {
	if h.rt == nil {
		return fmt.Errorf("runtime not started")
	}
	if _, err := h.rt.Repo.Load(h.srvCtx, sessionID); err != nil {
		return err
	}
	go func() {
		// BeginTurn inside Resume enforces mutual exclusion; a concurrent turn makes
		// this a no-op (ErrBusy), which is the intended "already running" behavior.
		_, _ = h.rt.Executor.Resume(h.srvCtx, sessionID)
	}()
	return nil
}

// Reconfigure hot-swaps the API keys and/or model without dropping the server or
// changing the port (v1.2 §3.3) — the fix for the setting-page churn that
// restart() caused. secretsJSON is the same shape Start takes (pass "" to keep
// current keys); modelName selects a configured model (pass "" to keep current).
// The swap lands at the next turn boundary; in-flight turns finish on the old
// config.
func (h *Handle) Reconfigure(secretsJSON, modelName string) error {
	if h.rt == nil {
		return fmt.Errorf("runtime not started")
	}
	secrets, err := parseSecretsJSON(secretsJSON)
	if err != nil {
		return err
	}
	// Start from the base config, re-inject secrets, and select the (possibly new)
	// model, so a bare model switch keeps the existing keys and vice versa.
	cfg := h.cfg
	injectSecrets(&cfg, secrets)
	mc, err := cfg.SelectModel(modelName)
	if err != nil {
		return err
	}
	provider, err := runtime.BuildProvider(mc, cfg.Provider)
	if err != nil {
		return err
	}
	h.rt.Builder.Reconfigure(mc, provider)
	return nil
}

// StartServer assembles the runtime and starts the agent-wire HTTP/WS server on
// the loopback interface, returning once it is listening. The server runs until
// Handle.Stop is called. The assembly mirrors cmd/codeagent.runServe.
func StartServer(ctx context.Context, opt Options) (*Handle, error) {
	cfg, err := app.LoadConfigBytes([]byte(opt.ConfigYAML))
	if err != nil {
		return nil, err
	}
	// MCP servers are injected as a Claude-compatible `.mcp.json` document rather
	// than embedded in the YAML config. Empty => no MCP.
	if cfg.MCP, err = mcp.ParseJSON([]byte(opt.MCPJSON)); err != nil {
		return nil, err
	}
	if opt.WorkspaceDir != "" {
		cfg.Workspace.Root = opt.WorkspaceDir
	}
	if opt.Sandboxed {
		cfg.Profile = app.ProfileSandboxed
		cfg.Hooks = nil // hooks run `sh -c`; disable them on a no-subprocess host
	}

	// Redirect the session store off $HOME (the read-only iOS container) to a
	// writable host-supplied directory. Done before any store opens.
	dataDir := opt.DataDir
	if dataDir == "" {
		dataDir = opt.WorkspaceDir
	}
	if dataDir != "" {
		runtime.SetStoreBaseDir(filepath.Join(dataDir, ".codeagent"))
		// User-level skills — shared across all workspaces. On iOS this is where
		// bundled + user-imported skills live (Application Support/skills/).
		cfg.GlobalSkillsDir = filepath.Join(dataDir, "skills")
	}

	injectSecrets(&cfg, opt.Secrets)

	mc, err := cfg.SelectModel(opt.ModelName)
	if err != nil {
		return nil, err
	}
	provider, err := runtime.BuildProvider(mc, cfg.Provider)
	if err != nil {
		return nil, err
	}

	// A cancellable context scoped to the server's lifetime; Stop cancels it so
	// observers and background goroutines tied to it wind down.
	srvCtx, cancel := context.WithCancel(ctx)

	h := &Handle{cancel: cancel, serveErr: make(chan error, 1), srvCtx: srvCtx, cfg: cfg}
	// On any error after this point, release whatever we already acquired.
	ok := false
	defer func() {
		if !ok {
			cancel()
			for i := len(h.closers) - 1; i >= 0; i-- {
				h.closers[i]()
			}
		}
	}()

	handler, rt, closers, err := Assemble(srvCtx, cfg, mc, provider)
	if err != nil {
		return nil, err
	}
	h.closers = closers
	h.rt = rt

	// Reconcile any turn left mid-flight by a previous process death (jetsam) to
	// paused, so the host lists a single "paused" status to offer "continue"
	// (v1.2 §3.2). Best-effort — a failure here must not block startup.
	_ = rt.Executor.ReconcileInterrupted(srvCtx)

	addr := opt.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	h.srv = &http.Server{Handler: handler}
	h.lis = lis
	h.port = lis.Addr().(*net.TCPAddr).Port

	go func() {
		err := h.srv.Serve(lis)
		if err != nil && err != http.ErrServerClosed {
			h.serveErr <- err
		}
		close(h.serveErr)
	}()

	ok = true
	return h, nil
}

// Assemble wires the runtime's execution-model components (tool registry, MCP
// manager, workspace registry, conversation repository, turn executor) and
// returns the agent-wire HTTP handler plus the cleanup functions the caller must
// run (in any order) when shutting down. It is the single assembly path shared by
// the embedded server (StartServer) and the `codeagent serve` CLI (runServe), so
// both frontends expose identical runtime behavior.
//
// The provider must already be built (callers differ in how they resolve creds:
// the CLI from env, the embedded host from injected secrets). On error, any
// resources opened before the failure are released before returning.
func Assemble(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider) (http.Handler, *Runtime, []func(), error) {
	root := cfg.Workspace.Root
	var closers []func()
	release := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	telemetryStore, err := runtime.OpenStore(root)
	if err != nil {
		return nil, nil, nil, err
	}
	closers = append(closers, func() { telemetryStore.Close() })
	runtime.AttachObserver(provider, telemetryStore, ctx)

	toolReg, _, mcpMgr, planRef, jobSink, err := runtime.BuildRegistry(ctx, cfg, mc, provider, telemetryStore, nil)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	closers = append(closers, func() { mcpMgr.Close() })

	wsReg := runtime.NewWorkspaceRegistry(root, cfg.GlobalSkillsDir)
	closers = append(closers, func() { wsReg.Close() })

	// Re-anchor persisted workspace refs only on the sandboxed (iOS) host, where the
	// sandbox path changes across launches. On desktop the root may be "." / cwd, and
	// re-anchoring there would wrongly rebind sessions to the launch directory — so
	// pass "" to keep absolute behavior unchanged.
	currentWorkspaceDir := ""
	if cfg.Profile == app.ProfileSandboxed {
		currentWorkspaceDir = root
	}
	repo := conversation.NewSQLiteRepository(
		telemetryStore,
		mc.ContextWindow,
		cfg.CompactThreshold(mc),
		mc.Model,
		currentWorkspaceDir,
		func(workspacePath string) string {
			inst, err := wsReg.Get(workspacePath)
			if err != nil {
				return ""
			}
			return inst.SkillReg.PromptIndex()
		},
	)
	eventStore := &conversation.StoreEventAdapter{Store: telemetryStore}

	active := conversation.NewActiveTurnRegistry()
	subs := conversation.NewSubscriptionManager()
	rb := runtime.NewServeRunBuilder(cfg, mc, provider, toolReg, wsReg, planRef)
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, rb)
	executor.SetTitleGenerator(conversation.NewLLMTitleGenerator(provider, mc.Model))
	// Job bracket events reach the owning conversation's live subscribers (P8.7
	// §8.4-2) — persisted copies are already handled inside the sink.
	if jobSink != nil {
		jobSink.SetLiveResolver(subs.Emitter)
	}

	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{
		ServerName:    "codeagent/" + mc.Model,
		Capabilities:  defaultCapabilities,
		WorkspaceRoot: root,
		Granter:       rb.Rules(),
		Prompts:       mcpMgr,
	})
	rt := &Runtime{Executor: executor, Builder: rb, Repo: repo}
	return handler, rt, closers, nil
}

// parseSecretsJSON decodes the JSON secrets object Reconfigure receives (gomobile
// cannot bridge a map, so secrets cross as a JSON string). Empty input yields a
// nil map, i.e. "keep the current keys".
func parseSecretsJSON(secretsJSON string) (map[string]string, error) {
	if secretsJSON == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(secretsJSON), &m); err != nil {
		return nil, fmt.Errorf("invalid secretsJSON: %w", err)
	}
	return m, nil
}

// injectSecrets overrides resolved API keys from the host-supplied secrets map.
// A secret may be keyed by a model's api_key_env name or by its friendly name;
// the model-name match takes precedence. Empty values are ignored.
//
// Web search provider keys (Tavily, Brave) are also injected here: a secret whose
// key matches the configured tavily_api_key_env or brave_api_key_env is set on the
// WebSearchConfig, following the same pattern as model keys.
func injectSecrets(cfg *app.Config, secrets map[string]string) {
	if len(secrets) == 0 {
		return
	}
	for name, mc := range cfg.Models {
		if v := secrets[mc.APIKeyEnv]; v != "" {
			mc.APIKey = v
		}
		if v := secrets[name]; v != "" {
			mc.APIKey = v
		}
		cfg.Models[name] = mc
	}

	// Web search provider keys: match by the env-var name declared in config
	// (e.g. TAVILY_API_KEY). This lets the iOS app pull search keys from the
	// Keychain the same way it does model keys — no env vars needed.
	if cfg.Web.Search.TavilyAPIKeyEnv != "" {
		if v := secrets[cfg.Web.Search.TavilyAPIKeyEnv]; v != "" {
			cfg.Web.Search.TavilyKey = v
		}
	}
	if cfg.Web.Search.BraveAPIKeyEnv != "" {
		if v := secrets[cfg.Web.Search.BraveAPIKeyEnv]; v != "" {
			cfg.Web.Search.BraveKey = v
		}
	}
}

// LoopbackURL returns the ws scheme base URL the host should hand to its client,
// e.g. for building the conversation stream endpoint.
func (h *Handle) LoopbackURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d", h.port)
}
