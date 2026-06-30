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
	"fmt"
	"net"
	"net/http"
	"path/filepath"

	"code-agent/internal/app"
	"code-agent/internal/conversation"
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

// Handle is a running embedded server. The host must call Stop to release the
// listener, the MCP subprocesses, and the SQLite stores.
type Handle struct {
	srv      *http.Server
	lis      net.Listener
	port     int
	cancel   context.CancelFunc
	closers  []func() // run in reverse on Stop, mirroring runServe's defers
	serveErr chan error
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

// StartServer assembles the runtime and starts the agent-wire HTTP/WS server on
// the loopback interface, returning once it is listening. The server runs until
// Handle.Stop is called. The assembly mirrors cmd/codeagent.runServe.
func StartServer(ctx context.Context, opt Options) (*Handle, error) {
	cfg, err := app.LoadConfigBytes([]byte(opt.ConfigYAML))
	if err != nil {
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

	h := &Handle{cancel: cancel, serveErr: make(chan error, 1)}
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

	handler, closers, err := Assemble(srvCtx, cfg, mc, provider)
	if err != nil {
		return nil, err
	}
	h.closers = closers

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
func Assemble(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider) (http.Handler, []func(), error) {
	root := cfg.Workspace.Root
	var closers []func()
	release := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	telemetryStore, err := runtime.OpenStore(root)
	if err != nil {
		return nil, nil, err
	}
	closers = append(closers, func() { telemetryStore.Close() })
	runtime.AttachObserver(provider, telemetryStore, ctx)

	toolReg, _, mcpMgr, planRef, err := runtime.BuildRegistry(ctx, cfg, mc, provider, telemetryStore, nil)
	if err != nil {
		release()
		return nil, nil, err
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
	rb := &runtime.ServeRunBuilder{
		Cfg: cfg, MC: mc, Provider: provider,
		ToolReg: toolReg, WSReg: wsReg, PlanRef: planRef,
	}
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, rb)
	executor.SetTitleGenerator(conversation.NewLLMTitleGenerator(provider, mc.Model))

	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{
		ServerName:   "codeagent/" + mc.Model,
		Capabilities: defaultCapabilities,
	})
	return handler, closers, nil
}

// injectSecrets overrides resolved API keys from the host-supplied secrets map.
// A secret may be keyed by a model's api_key_env name or by its friendly name;
// the model-name match takes precedence. Empty values are ignored.
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
}

// LoopbackURL returns the ws scheme base URL the host should hand to its client,
// e.g. for building the conversation stream endpoint.
func (h *Handle) LoopbackURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d", h.port)
}
