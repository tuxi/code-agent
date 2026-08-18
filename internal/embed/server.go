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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/buildinfo"
	"code-agent/internal/controlplane"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/mcp"
	"code-agent/internal/model"
	"code-agent/internal/repos"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
	"code-agent/internal/settings"
	"os"
)

// defaultCapabilities is the capability set advertised in the WebSocket hello
// handshake. Kept in sync with cmd/codeagent.defaultCapabilities — it is a static
// contract clients use to decide which protocol features to enable.
var defaultCapabilities = []string{
	"streaming",
	"thinking",
	"reasoning_streaming",
	"tool_streaming",
	"plan_mode",
	"subagents",
	"child_streaming",
	"session_resume",
	"client_tool_execution",
	"workflow_execution",
	"conversation_fork_v1",
}

// Options configures an embedded server. Every field is supplied in-memory by the
// host; nothing is read from disk or the environment except what the resolved
// config explicitly points at.
type Options struct {
	// WorkspaceDir is the agent's working root. On iOS this is the app container
	// (e.g. the Documents directory). Overrides whatever the config declares.
	WorkspaceDir string

	// ConfigYAML is a legacy base config document. Production hosts pass ""
	// (the embedded runtime's real configuration comes from SettingsJSON /
	// the persisted <DataDir>/.codeagent/settings.json, which overrides this
	// base when it carries infrastructure). Tests use it to inject a minimal
	// base config. Empty => built-in defaults (see app.LoadConfigBytes).
	ConfigYAML string

	// MCPJSON is the raw Claude-compatible `.mcp.json` document configuring
	// external MCP servers ({"mcpServers": {...}}). The desktop backend reads this
	// from the workspace-root file; embedded hosts (iOS/macOS) have no fixed path,
	// so they inject it here, the same way ConfigYAML carries the main config.
	// Empty => no MCP servers. On a sandboxed (iOS) host, stdio servers are still
	// skipped — only http/sse servers connect (they need no subprocess).
	MCPJSON string

	// SettingsJSON is the raw project settings document (a Claude-style
	// settings.json: permissions / verify / hooks) injected in-memory, the same way
	// MCPJSON is — embedded hosts (iOS) have no fixed .codeagent/settings.json path.
	// Its blocks fold into the config layer (permissions, verify command, and — on a
	// non-sandboxed host — hooks). Empty => none. Secrets never belong here.
	SettingsJSON string

	// ModelName selects which configured model to use. Empty => default_model.
	ModelName string

	// Secrets supplies API keys without using environment variables. Keys may be
	// matched either by a model's api_key_env name or by the model's friendly
	// name; the value becomes that model's resolved API key. Intended to carry
	// secrets pulled from the iOS Keychain.
	Secrets map[string]string

	// ConnectionsJSON carries connection DEFINITIONS (non-secret) as a JSON
	// string: {"connections": {"<id>": {api, base_url, credential, models}}}.
	// R3.4/R3.5: definitions flow here instead of the ConfigYAML models block,
	// so hosts do not hand-assemble the full YAML document. Empty => no
	// connections injected.
	ConnectionsJSON string

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

	// MaxConcurrentTurns overrides runtime.max_concurrent_turns when positive.
	// Zero keeps the config value, whose safe default is one.
	MaxConcurrentTurns int

	// ServerAccessToken authenticates every Runtime HTTP and Agent Wire request
	// except the public health check. Embedded hosts generate a fresh 256-bit
	// random token for each start and keep it only in memory.
	ServerAccessToken string
}

type RuntimeServerOptions struct {
	Profile     string
	DisplayName string
	Auth        server.ServerAuth

	// Providers wires /v1/providers. Nil (zero value) disables the endpoints
	// (404), matching the pre-existing embedded behavior and the CLI serve path.
	Providers server.ProviderService

	// InjectSecrets wires POST /v1/secrets. Nil disables the endpoint.
	InjectSecrets func(targets map[credential.Target]credential.Credential) error

	// RuntimeModelsBuilder, when set, rebuilds the model catalog on each
	// GET /v1/runtime/models so a POST /v1/secrets makes models available
	// without a restart. Nil serves the startup snapshot.
	RuntimeModelsBuilder func() server.RuntimeModelCatalog
}

// Runtime is the set of live components Assemble builds that the lifecycle verbs
// (Suspend / ResumeSession / Reconfigure) operate on, distinct from the HTTP
// handler. Assemble returns it so the embedded Handle can drive suspend/resume and
// hot-reload; the CLI serve path ignores it (it uses process lifecycle).
type Runtime struct {
	Executor *conversation.TurnExecutor
	Builder  *runtime.ServeRunBuilder
	Repo     conversation.ConversationRepository
	Owner    *controlplane.Manager
}

// Handle is a running embedded server. The host must call Stop to release the
// listener, the MCP subprocesses, and the SQLite stores.
type Handle struct {
	srv              *http.Server
	lis              net.Listener
	port             int
	loopbackEndpoint string
	cancel           context.CancelFunc
	closers          []func() // run in reverse on Stop, mirroring runServe's defers
	serveErr         chan error

	// Lifecycle state (v1.2). srvCtx is the server-scoped context resumed turns run
	// under (so Stop cancels them); cfg + rt back Suspend/ResumeSession/Reconfigure.
	srvCtx     context.Context
	cfg        app.Config
	rt         *Runtime
	credential credential.Resolver
	modelName  string

	reconfigureMu sync.Mutex

	// The optional LAN listener shares the same Runtime handler and stores. Its
	// TLS identity and device validation material are injected by the host and
	// remain in memory only.
	sharedMu       sync.Mutex
	coreHandler    http.Handler
	sharedSrv      *http.Server
	sharedLis      net.Listener
	sharedAuth     *server.SharedDeviceAuthenticator
	sharedPort     int
	sharedEndpoint string
	sharedStatus   SharedListenerStatus
}

const (
	SharedListenerStopped  = "stopped"
	SharedListenerStarting = "starting"
	SharedListenerRunning  = "running"
	SharedListenerFailed   = "failed"

	sharedReadHeaderTimeout = 10 * time.Second
	sharedIdleTimeout       = 90 * time.Second
	sharedMaxHeaderBytes    = 64 << 10
)

// SharedListenerStatus is a control-plane snapshot. ListenOrigin describes the
// bound socket (and may contain 0.0.0.0/::); it is never a client-connectable
// endpoint and must not be placed in Bonjour or pairing QR payloads.
type SharedListenerStatus struct {
	State            string `json:"state"`
	ListenAddress    string `json:"listen_address,omitempty"`
	ListenOrigin     string `json:"listen_origin,omitempty"`
	Port             int    `json:"port"`
	StartedAt        string `json:"started_at,omitempty"`
	StoppedAt        string `json:"stopped_at,omitempty"`
	LastTransitionAt string `json:"last_transition_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
}

// SharedListenerOptions is the in-memory control-plane configuration supplied by
// AgentKit. Certificate/private-key PEM and bootstrap validation material must
// never be written to the Runtime config or logs.
type SharedListenerOptions struct {
	Addr               string
	CertificatePEM     string
	PrivateKeyPEM      string
	BootstrapSHA256    string
	BootstrapExpiresAt time.Time
	Devices            []server.SharedDeviceRecord
	EnrollmentTimeout  time.Duration
}

// Port returns the actual TCP port the server is listening on. With Addr empty
// this is the OS-assigned ephemeral port the host should hand to AgentKit.
func (h *Handle) Port() int { return h.port }

// Stop shuts the server down and releases every resource acquired by StartServer.
// It is safe to call once; further calls are no-ops.
func (h *Handle) Stop() error {
	sharedErr := h.StopSharedListener()
	if h.srv == nil {
		fmt.Fprintf(os.Stderr, "[lifecycle] Handle.Stop(): already nil, no-op\n")
		return sharedErr
	}
	fmt.Fprintf(os.Stderr, "[lifecycle] Handle.Stop(): closing server (port=%d)\n", h.port)
	err := h.srv.Close()
	if h.cancel != nil {
		h.cancel()
		fmt.Fprintf(os.Stderr, "[lifecycle] Handle.Stop(): context cancelled\n")
	}
	for i := len(h.closers) - 1; i >= 0; i-- {
		h.closers[i]()
	}
	fmt.Fprintf(os.Stderr, "[lifecycle] Handle.Stop(): %d closers run, DB closed\n", len(h.closers))
	h.srv = nil
	return errors.Join(sharedErr, err)
}

// StartSharedListener starts a TLS-only LAN listener around the same core
// Runtime handler used by the private loopback listener. It does not assemble a
// second Runtime and therefore preserves server_id, conversations and live event
// subscriptions across both clients.
func (h *Handle) StartSharedListener(opt SharedListenerOptions) error {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	if h.sharedSrv != nil {
		return fmt.Errorf("shared Runtime listener is already running")
	}
	h.sharedStatus = SharedListenerStatus{
		State:            SharedListenerStarting,
		LastTransitionAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if h.coreHandler == nil || h.srv == nil {
		return h.recordSharedFailureLocked(fmt.Errorf("runtime not started"))
	}
	certificate, err := tls.X509KeyPair(
		[]byte(opt.CertificatePEM),
		[]byte(opt.PrivateKeyPEM),
	)
	if err != nil {
		return h.recordSharedFailureLocked(
			fmt.Errorf("invalid shared Runtime TLS identity: %w", err),
		)
	}
	authenticator, err := server.NewSharedDeviceAuthenticator(
		opt.Devices,
		opt.BootstrapSHA256,
		opt.BootstrapExpiresAt,
		opt.EnrollmentTimeout,
	)
	if err != nil {
		return h.recordSharedFailureLocked(err)
	}
	addr := opt.Addr
	if addr == "" {
		addr = "0.0.0.0:0"
	}
	rawListener, err := net.Listen("tcp", addr)
	if err != nil {
		return h.recordSharedFailureLocked(err)
	}
	tlsListener := tls.NewListener(rawListener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	sharedServer := &http.Server{
		Handler:           authenticator.Handler(h.coreHandler),
		ReadHeaderTimeout: sharedReadHeaderTimeout,
		IdleTimeout:       sharedIdleTimeout,
		MaxHeaderBytes:    sharedMaxHeaderBytes,
	}
	tcpAddress, ok := rawListener.Addr().(*net.TCPAddr)
	if !ok {
		_ = rawListener.Close()
		return h.recordSharedFailureLocked(
			fmt.Errorf("shared Runtime listener returned unsupported address %T", rawListener.Addr()),
		)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	h.sharedSrv = sharedServer
	h.sharedLis = rawListener
	h.sharedAuth = authenticator
	h.sharedPort = tcpAddress.Port
	h.sharedEndpoint = "https://" + net.JoinHostPort(tcpAddress.IP.String(), fmt.Sprint(tcpAddress.Port))
	h.sharedStatus = SharedListenerStatus{
		State:            SharedListenerRunning,
		ListenAddress:    rawListener.Addr().String(),
		ListenOrigin:     h.sharedEndpoint,
		Port:             h.sharedPort,
		StartedAt:        now,
		LastTransitionAt: now,
	}

	go func() {
		err := sharedServer.Serve(tlsListener)
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[shared-runtime] listener stopped: %v\n", err)
			authenticator.CloseConnections()
			h.sharedMu.Lock()
			if h.sharedSrv == sharedServer {
				now := time.Now().UTC().Format(time.RFC3339Nano)
				h.sharedSrv = nil
				h.sharedLis = nil
				h.sharedAuth = nil
				h.sharedPort = 0
				h.sharedEndpoint = ""
				h.sharedStatus = SharedListenerStatus{
					State:            SharedListenerFailed,
					StoppedAt:        now,
					LastTransitionAt: now,
					LastError:        safeSharedListenerError(err),
				}
			}
			h.sharedMu.Unlock()
		}
	}()
	fmt.Fprintf(os.Stderr, "[shared-runtime] listener started port=%d\n", h.sharedPort)
	return nil
}

// StopSharedListener immediately closes the optional LAN surface without
// stopping the private Embedded Runtime.
func (h *Handle) StopSharedListener() error {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	if h.sharedSrv == nil {
		if h.sharedStatus.State != SharedListenerStopped {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			h.sharedStatus = SharedListenerStatus{
				State: SharedListenerStopped, StoppedAt: now, LastTransitionAt: now,
			}
		}
		return nil
	}
	h.sharedAuth.CloseConnections()
	err := h.sharedSrv.Close()
	h.sharedSrv = nil
	h.sharedLis = nil
	h.sharedAuth = nil
	h.sharedPort = 0
	h.sharedEndpoint = ""
	now := time.Now().UTC().Format(time.RFC3339Nano)
	h.sharedStatus = SharedListenerStatus{
		State: SharedListenerStopped, StoppedAt: now, LastTransitionAt: now,
	}
	fmt.Fprintln(os.Stderr, "[shared-runtime] listener stopped")
	return err
}

func (h *Handle) SharedPort() int {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	return h.sharedPort
}

func (h *Handle) SharedEndpoint() string {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	return h.sharedEndpoint
}

func (h *Handle) SharedListenerStatus() SharedListenerStatus {
	h.sharedMu.Lock()
	defer h.sharedMu.Unlock()
	status := h.sharedStatus
	if status.State == "" {
		status.State = SharedListenerStopped
	}
	return status
}

func (h *Handle) recordSharedFailureLocked(err error) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	h.sharedStatus = SharedListenerStatus{
		State:            SharedListenerFailed,
		StoppedAt:        now,
		LastTransitionAt: now,
		LastError:        safeSharedListenerError(err),
	}
	return err
}

func safeSharedListenerError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	const maxErrorBytes = 512
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	return message
}

func (h *Handle) PendingSharedEnrollments() []server.SharedEnrollment {
	h.sharedMu.Lock()
	authenticator := h.sharedAuth
	h.sharedMu.Unlock()
	if authenticator == nil {
		return nil
	}
	return authenticator.PendingEnrollments()
}

func (h *Handle) AcknowledgeSharedEnrollment(enrollmentID string) error {
	h.sharedMu.Lock()
	authenticator := h.sharedAuth
	h.sharedMu.Unlock()
	if authenticator == nil {
		return fmt.Errorf("shared Runtime listener is not running")
	}
	return authenticator.AcknowledgeEnrollment(enrollmentID)
}

func (h *Handle) RejectSharedEnrollment(enrollmentID string) error {
	h.sharedMu.Lock()
	authenticator := h.sharedAuth
	h.sharedMu.Unlock()
	if authenticator == nil {
		return fmt.Errorf("shared Runtime listener is not running")
	}
	return authenticator.RejectEnrollment(enrollmentID)
}

func (h *Handle) UpdateSharedDevices(devices []server.SharedDeviceRecord) error {
	h.sharedMu.Lock()
	authenticator := h.sharedAuth
	h.sharedMu.Unlock()
	if authenticator == nil {
		return fmt.Errorf("shared Runtime listener is not running")
	}
	return authenticator.UpdateDevices(devices)
}

func (h *Handle) RotateSharedBootstrap(hash string, expiresAt time.Time) error {
	h.sharedMu.Lock()
	authenticator := h.sharedAuth
	h.sharedMu.Unlock()
	if authenticator == nil {
		return fmt.Errorf("shared Runtime listener is not running")
	}
	return authenticator.RotateBootstrap(hash, expiresAt)
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
//
// Reconfigure swaps model and managed-tool credentials at the next turn
// boundary. Structural tool graph changes (provider kind, MCP server list, etc.)
// still require a server restart.
//
// R3.2: the 3-argument form also accepts connection DEFINITIONS via
// connectionsJSON (non-secret; "" = keep current). The 2-argument form below is
// the backward-compatible wrapper (connectionsJSON = "").
func (h *Handle) Reconfigure(connectionsJSON, secretsJSON, modelName string) error {
	h.reconfigureMu.Lock()
	defer h.reconfigureMu.Unlock()

	if h.rt == nil {
		return fmt.Errorf("runtime not started")
	}
	secrets, err := parseSecretsJSON(secretsJSON)
	if err != nil {
		return err
	}
	conns, err := parseConnectionsJSON(connectionsJSON)
	if err != nil {
		return err
	}
	// Start from the stored config, apply connection definitions + secrets, and
	// select the model. The copy-on-stack pattern keeps h.cfg unchanged on error
	// so a failed reconfigure doesn't leave a half-updated stored config.
	cfg := h.cfg
	if len(conns) > 0 {
		applyConnections(&cfg, conns)
	}
	credChain := h.credential
	if len(secrets) > 0 {
		injectedResolver := injectSecrets(&cfg, secrets)
		credChain = cfg.CredentialResolver(injectedResolver)
	}
	if credChain == nil {
		credChain = cfg.CredentialResolver(nil)
	}
	if len(cfg.Models) == 0 {
		if modelName != "" {
			return runtime.ModelNotConfiguredError{}
		}
		h.rt.Builder.Reconfigure(app.ModelConfig{}, nil, credChain)
		h.cfg = cfg
		h.credential = credChain
		h.modelName = ""
		return nil
	}
	selectedModelName := modelName
	if selectedModelName == "" {
		selectedModelName = h.modelName
	}
	mc, err := cfg.SelectModel(selectedModelName)
	if err != nil {
		return err
	}
	provider, err := runtime.BuildProvider(mc, cfg.Provider, credChain)
	if err != nil {
		return err
	}
	h.rt.Builder.Reconfigure(mc, provider, credChain)
	h.cfg = cfg
	h.credential = credChain
	h.modelName = selectedModelName
	return nil
}

// settingsHasInfrastructure reports whether a settings.File carries any
// infrastructure section (models/credentials/agent/provider/web/runtime/
// default_model/currency) as opposed to only behavior (permissions/verify/
// hooks). When true, the embedded cfg is rebuilt from the settings document
// (single config source); when false the YAML-derived cfg stands and the
// document supplies behavior only.
func settingsHasInfrastructure(sf settings.File) bool {
	if sf.DefaultModel != "" || sf.SubagentModel != "" || sf.Currency != "" {
		return true
	}
	if len(sf.Providers) > 0 || len(sf.Credentials) > 0 {
		return true
	}
	if sf.Agent.MaxSteps != 0 || sf.Agent.MaxParallelTools != 0 ||
		sf.Agent.CompactRatio != 0 || sf.Agent.CompactKeepRatio != 0 ||
		sf.Agent.ClientToolTimeoutSeconds != 0 || sf.Agent.SubagentModel != "" {
		return true
	}
	if sf.Provider.RequestTimeoutSeconds != 0 || sf.Provider.MaxRetries != 0 ||
		sf.Provider.BackoffMillis != 0 || sf.Provider.MaxBackoffSeconds != 0 {
		return true
	}
	if sf.Web.Search.Provider != "" || sf.Web.Search.FallbackProvider != "" ||
		sf.Web.Search.GatewayBaseURL != "" || sf.Web.Search.TopK != 0 ||
		sf.Web.Search.TimeoutSeconds != 0 || sf.Web.Search.TavilyAPIKeyEnv != "" ||
		sf.Web.Search.BraveAPIKeyEnv != "" || sf.Web.Fetch.TimeoutSeconds != 0 ||
		sf.Web.Fetch.CacheTTLSeconds != 0 {
		return true
	}
	return sf.Runtime.MaxConcurrentTurns != 0
}

// StartServer assembles the runtime and starts the agent-wire HTTP/WS server on
// the loopback interface, returning once it is listening. The server runs until
// Handle.Stop is called. The assembly mirrors cmd/codeagent.runServe.
func StartServer(ctx context.Context, opt Options) (*Handle, error) {
	if err := server.ValidateServerAccessToken(opt.ServerAccessToken); err != nil {
		return nil, fmt.Errorf("embedded Runtime requires an in-memory Server Access Token: %w", err)
	}
	if err := validateEmbeddedLoopbackAddress(opt.Addr); err != nil {
		return nil, err
	}
	cfg, err := app.LoadConfigBytes([]byte(opt.ConfigYAML))
	if err != nil {
		return nil, err
	}

	// Redirect the session store off $HOME (the read-only iOS container) to a
	// writable host-supplied directory. The same directory owns the persistent
	// settings.json — the embedded counterpart to codeagentd's
	// ~/.codeagent/settings.json — so /v1/providers changes survive restart.
	dataDir := opt.DataDir
	if dataDir == "" {
		dataDir = opt.WorkspaceDir
	}
	settingsPath := ""
	if dataDir != "" {
		runtime.SetStoreBaseDir(filepath.Join(dataDir, ".codeagent"))
		cfg.GlobalSkillsDir = filepath.Join(dataDir, "skills")
		cfg.GlobalPromptsDir = filepath.Join(dataDir, "prompts")
		settingsPath = filepath.Join(dataDir, ".codeagent", "settings.json")
	}

	// Merged settings view. The persisted settings.json is the durable
	// infrastructure source (providers/models/credentials): once it exists, the
	// runtime and /v1/providers both read/write that single file (daemon parity).
	// The host-injected SettingsJSON is a first-launch seed (persisted below) and
	// a behavior overlay (permissions/verify/hooks) that wins over disk.
	diskFile, hasDisk, err := loadDiskSettingsFile(settingsPath)
	if err != nil {
		return nil, err
	}
	if !hasDisk && opt.SettingsJSON != "" {
		if err := persistSettingsSeed(settingsPath, opt.SettingsJSON); err != nil {
			return nil, fmt.Errorf("persist embedded settings seed: %w", err)
		}
		diskFile, hasDisk, err = loadDiskSettingsFile(settingsPath)
		if err != nil {
			return nil, err
		}
	}

	var embeddedSettings settings.Settings
	if hasDisk && settingsHasInfrastructure(diskFile) {
		// Disk is authoritative for infrastructure.
		cfg = app.FromSettings(settingsFileToSettings(diskFile))
		embeddedSettings = settingsFileToSettings(diskFile)
	}
	if opt.SettingsJSON != "" {
		sf, err := settings.ParseJSON([]byte(opt.SettingsJSON))
		if err != nil {
			return nil, err
		}
		// Fallback for a migration/legacy host that never persisted a seed: the
		// injected infrastructure is authoritative only when disk has none.
		if settingsHasInfrastructure(sf) && (!hasDisk || !settingsHasInfrastructure(diskFile)) {
			cfg = app.FromSettings(settingsFileToSettings(sf))
		}
		// Behavior (permissions/verify/hooks) is always host-injected.
		embeddedSettings.Permissions = sf.Permissions
		embeddedSettings.Hooks = sf.Hooks
		if sf.Verify != nil {
			embeddedSettings.Verify = sf.Verify
		}
	}
	// R3.5: connection definitions injected via connectionsJSON are merged into
	// the config as the top (host) layer before MCP/skills assembly.
	if conns, err := parseConnectionsJSON(opt.ConnectionsJSON); err != nil {
		return nil, err
	} else {
		applyConnections(&cfg, conns)
	}
	// MCP servers are injected as a Claude-compatible `.mcp.json` document rather
	// than embedded in the YAML config. Empty => no MCP.
	if cfg.MCP, err = mcp.ParseJSON([]byte(opt.MCPJSON)); err != nil {
		return nil, err
	}
	if opt.WorkspaceDir != "" {
		// cfg.Workspace removed: workspaceDir flows through Assemble explicitly.
	}

	if opt.Sandboxed {
		cfg.Profile = app.ProfileSandboxed
		embeddedSettings.Hooks = nil // hooks run `sh -c`; disable them on a no-subprocess host
	}

	// A2 parity with codeagentd: seed a mutable resolver from the startup secrets
	// and let POST /v1/secrets update it live. injectSecrets still performs the
	// cfg mutation (credential ref alignment + web search keys); its entries are
	// copied into the mutable resolver so the startup set is also HTTP-visible.
	injectedResolver := injectSecrets(&cfg, opt.Secrets)
	mutableResolver := credential.NewMutableResolver()
	if sr, ok := injectedResolver.(credential.StaticResolver); ok {
		mutableResolver.SetAll(map[credential.Target]credential.Credential(sr))
	}
	credChain := cfg.CredentialResolver(mutableResolver)

	var mc app.ModelConfig
	var provider model.Provider
	if len(cfg.Models) > 0 {
		mc, err = cfg.SelectModel(opt.ModelName)
		if err != nil {
			return nil, err
		}
		provider, err = runtime.BuildProvider(mc, cfg.Provider, credChain)
		if err != nil {
			return nil, err
		}
	} else if opt.ModelName != "" {
		return nil, runtime.ModelNotConfiguredError{}
	}

	// A cancellable context scoped to the server's lifetime; Stop cancels it so
	// observers and background goroutines tied to it wind down.
	srvCtx, cancel := context.WithCancel(ctx)

	h := &Handle{cancel: cancel, serveErr: make(chan error, 1), srvCtx: srvCtx, cfg: cfg, credential: credChain, modelName: opt.ModelName}
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

	workspaceDir := opt.WorkspaceDir
	if opt.MaxConcurrentTurns > 0 {
		cfg.Runtime.MaxConcurrentTurns = opt.MaxConcurrentTurns
	}
	cloneStateDir := ""
	if dataDir != "" {
		cloneStateDir = filepath.Join(dataDir, ".codeagent", "clone")
	}
	profile := server.RuntimeProfileFullDesktop
	if opt.Sandboxed {
		profile = server.RuntimeProfileSandboxed
	}
	coreHandler, rt, closers, err := Assemble(
		srvCtx, cfg, embeddedSettings, mc, provider, credChain, workspaceDir, cloneStateDir,
		RuntimeServerOptions{
			Profile:   profile,
			Providers: server.NewProviderStore(settingsPath, nil),
			InjectSecrets: func(targets map[credential.Target]credential.Credential) error {
				mutableResolver.SetAll(targets)
				return nil
			},
			RuntimeModelsBuilder: func() server.RuntimeModelCatalog {
				return server.BuildRuntimeModelCatalog(cfg, mutableResolver)
			},
		},
	)
	if err != nil {
		return nil, err
	}
	h.closers = closers
	h.rt = rt
	h.coreHandler = coreHandler

	addr := opt.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	loopbackHandler := server.WithServerAuth(coreHandler, server.ServerAuth{
		Enabled: true, Token: opt.ServerAccessToken, PublicHealthz: true,
	})
	h.srv = &http.Server{Handler: loopbackHandler}
	h.lis = lis
	tcpAddress := lis.Addr().(*net.TCPAddr)
	h.port = tcpAddress.Port
	h.loopbackEndpoint = "ws://" + net.JoinHostPort(tcpAddress.IP.String(), fmt.Sprint(tcpAddress.Port))

	go func() {
		err := h.srv.Serve(lis)
		if err != nil && err != http.ErrServerClosed {
			h.serveErr <- err
		}
		close(h.serveErr)
	}()

	ok = true
	fmt.Fprintf(os.Stderr, "[lifecycle] StartServer() OK: port=%d dataDir=%s storeBase=%s\n", h.port, dataDir, runtime.StoreBaseDir())
	return h, nil
}

func validateEmbeddedLoopbackAddress(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid embedded Runtime listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("embedded Runtime private listener must bind to loopback")
	}
	return nil
}

// Assemble wires the runtime's execution-model components (tool registry, MCP
// manager, workspace registry, conversation repository, turn executor) and
// returns the agent-wire HTTP handler plus the cleanup functions the caller must
// run (in any order) when shutting down. It is the single assembly path shared by
// the embedded server (StartServer) and the `codeagent serve` CLI (runServe), so
// both frontends expose identical runtime behavior.
//
// The provider must already be built when models are configured (callers differ
// in how they resolve credentials). It is nil for an intentional models: {}
// Embedded Runtime. On error, resources opened before the failure are released.
func Assemble(ctx context.Context, cfg app.Config, set settings.Settings, mc app.ModelConfig, provider model.Provider, cred credential.Resolver, workspaceDir, cloneStateDir string, serverOptions RuntimeServerOptions) (http.Handler, *Runtime, []func(), error) {
	if workspaceDir == "" {
		workspaceDir, _ = os.Getwd()
	}
	var closers []func()
	release := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	runtimeInfo, runtimeModels, err := server.BuildRuntimeContract(
		cfg, workspaceDir, serverOptions.DisplayName, serverOptions.Profile,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	telemetryStore, err := runtime.OpenStore(workspaceDir)
	if err != nil {
		return nil, nil, nil, err
	}
	closers = append(closers, func() { telemetryStore.Close() })
	runtime.AttachObserver(provider, telemetryStore, ctx)

	// Base registry: built-ins only. MCP is workspace-scoped — each conversation
	// workspace resolves its own .mcp.json on first access (EnableMCP below).
	// cfg.MCP carries only host-INJECTED servers (the embedded MCPJSON document);
	// they participate as an extra user-scope layer under every workspace's own
	// files. The CLI serve path leaves cfg.MCP empty (see runServe).
	toolReg, _, planRef, jobSink, err := runtime.BuildBaseRegistry(ctx, cfg, mc, provider, cred, telemetryStore, workspaceDir, nil)
	if err != nil {
		release()
		return nil, nil, nil, err
	}

	wsReg := runtime.NewWorkspaceRegistry(cfg.GlobalSkillsDir)
	wsReg.EnableMCP(ctx, toolReg, cfg, cfg.MCP.Servers, false)
	closers = append(closers, func() { wsReg.Close() })

	// Re-anchor persisted workspace refs only on the sandboxed (iOS) host, where the
	// sandbox path changes across launches. On desktop the root may be "." / cwd, and
	// re-anchoring there would wrongly rebind sessions to the launch directory — so
	// pass "" to keep absolute behavior unchanged.
	currentWorkspaceDir := ""
	if cfg.Profile == app.ProfileSandboxed {
		currentWorkspaceDir = workspaceDir
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
	rb := runtime.NewServeRunBuilder(cfg, set, mc, provider, cred, toolReg, wsReg, planRef)
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, rb)
	executor.SetAssetRefReleaseService(rb)
	maxConcurrentTurns := cfg.RuntimeMaxConcurrentTurns()
	executor.SetTurnScheduler(conversation.NewTurnScheduler(maxConcurrentTurns))
	if provider != nil {
		executor.SetTitleGenerator(conversation.NewLLMTitleGenerator(provider, mc.Model))
	}
	managedWorktrees, worktreeReport, worktreeErr := runtime.ConfigureManagedWorktrees(ctx, telemetryStore, repo, executor, cfg.Profile != app.ProfileSandboxed)
	if worktreeErr != nil {
		fmt.Printf("codeagent embedded: managed worktrees disabled: %v\n", worktreeErr)
	} else if managedWorktrees != nil && (len(worktreeReport.Issues) > 0 || len(worktreeReport.Orphans) > 0) {
		fmt.Printf("codeagent embedded: managed worktree reconciliation: issues=%d orphans=%d missing=%d\n", len(worktreeReport.Issues), len(worktreeReport.Orphans), len(worktreeReport.Missing))
	}
	stateDir, err := runtime.StateDir()
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	owner, err := controlplane.NewManager(stateDir, runtimeInfo.ServerID, repo, executor, controlplane.Config{})
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	owner.SetTarget(controlplane.NewRuntimeTarget(executor, eventStore, repo, managedWorktrees))
	rb.SetSessionControl(owner)
	runtime.SetFluxExternalResolver(owner)
	if err := owner.Start(ctx); err != nil {
		_ = owner.Close()
		release()
		return nil, nil, nil, err
	}
	closers = append(closers, func() { _ = owner.Close() })
	ownerIdentity := owner.Identity()
	fmt.Fprintf(os.Stderr, "[control-plane] owner ready instance=%s endpoint=%s protocol=%d\n", ownerIdentity.InstanceID, ownerIdentity.Endpoint, ownerIdentity.ProtocolVersion)
	// Job bracket events reach the owning conversation's live subscribers (P8.7
	// §8.4-2) — persisted copies are already handled inside the sink.
	if jobSink != nil {
		jobSink.SetLiveResolver(subs.Emitter)
	}

	runtimeCapabilities := server.ConfiguredRuntimeCapabilities(maxConcurrentTurns)
	runtimeCapabilities.ManagedWorktree = managedWorktrees != nil
	if cloneStateDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cloneStateDir = filepath.Join(home, ".codeagent", "clone")
		}
	}
	cloneService, cloneErr := repos.NewService(workspaceDir, cloneStateDir)
	capabilities := append([]string(nil), defaultCapabilities...)
	_ = executor.ReconcileInterrupted(ctx)
	if cloneErr != nil {
		fmt.Printf("codeagent embedded: public git clone disabled: %v\n", cloneErr)
	} else {
		closers = append(closers, func() { _ = cloneService.Close() })
		capabilities = append(capabilities, "public_git_clone_v1")
	}
	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{
		ServerName:           buildinfo.ServerName(),
		RuntimeInfo:          runtimeInfo,
		RuntimeModels:        runtimeModels,
		Providers:            serverOptions.Providers,
		InjectSecrets:        serverOptions.InjectSecrets,
		RuntimeModelsBuilder: serverOptions.RuntimeModelsBuilder,
		ServerAuth:           serverOptions.Auth,
		Capabilities:         capabilities,
		CloneService:         cloneService,
		Granter:              rb.Rules(),
		Permissions:          server.NewPermissionStore(homeDir()),
		WorkspaceReloader:    wsReg.ReloadWorkspace,
		Prompts:              wsReg, // default workspace's MCP prompts; per-workspace needs a wire change
		CapabilityResolver: func(ctx context.Context) []string {
			persistence, ok := repo.(conversation.UserAssetsPersistenceCapability)
			if ok && persistence.SupportsUserAssetsPersistence() && rb.ImageInputCapability(ctx, cred) {
				return []string{"image_input"}
			}
			return nil
		},
		SessionReady: func(ctx context.Context, sessionID string) {
			if err := owner.Heartbeat(context.WithoutCancel(ctx)); err != nil {
				fmt.Fprintf(os.Stderr, "[control-plane] session ready reconcile: %v\n", err)
			}
			_, _ = executor.RecoverSessionTurnInputs(context.WithoutCancel(ctx), sessionID)
			go executor.FlushAssetRefReleases(context.WithoutCancel(ctx), cred)
		},
		OwnershipChanged: func(ctx context.Context) {
			if err := owner.Heartbeat(context.WithoutCancel(ctx)); err != nil {
				fmt.Fprintf(os.Stderr, "[control-plane] ownership reconcile: %v\n", err)
			}
		},
		RuntimeCapabilities: runtimeCapabilities,
		ManagedWorktrees:    managedWorktrees,
		SessionForks:        owner,
		WorkflowSnapshot:    runtime.NewWorkflowSnapshotFunc(),
	})
	rt := &Runtime{Executor: executor, Builder: rb, Repo: repo, Owner: owner}
	return handler, rt, closers, nil
}

// homeDir resolves the user home for the settings layer. Best-effort: an
// unresolvable home only disables the user-scope fallback file, never startup.
func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// loadDiskSettingsFile reads the embedded runtime's persistent settings.json
// (dataDir/.codeagent/settings.json). hasDisk is false when the file does not
// exist yet — a missing file is the normal first-launch state, not an error.
func loadDiskSettingsFile(path string) (settings.File, bool, error) {
	if path == "" {
		return settings.File{}, false, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return settings.File{}, false, nil
		}
		return settings.File{}, false, err
	}
	f, err := settings.LoadFile(path)
	return f, true, err
}

// persistSettingsSeed writes the host-injected settings document to disk on
// first launch so /v1/providers has a durable file to manage thereafter. The
// seed is the host's bundled template (Gateway + defaults); subsequent writes
// go through /v1/providers (settings.Persist) and win over this template.
func persistSettingsSeed(path, raw string) error {
	if path == "" || raw == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(raw), 0o644)
}

// settingsFileToSettings converts an on-disk settings document into the merged
// Settings view the runtime consumes. It mirrors the field list of the legacy
// explicit app.FromSettings call — infra blocks AND behavior blocks.
func settingsFileToSettings(f settings.File) settings.Settings {
	return settings.Settings{
		DefaultModel:  f.DefaultModel,
		SubagentModel: f.SubagentModel,
		Providers:     f.Providers,
		Credentials:   f.Credentials,
		Agent:         f.Agent,
		Provider:      f.Provider,
		Web:           f.Web,
		Runtime:       f.Runtime,
		Server:        f.Server,
		Currency:      f.Currency,
		Permissions:   f.Permissions,
		Verify:        f.Verify,
		Hooks:         f.Hooks,
		ApprovalMode:  f.ApprovalMode,
	}
}

// parseConnectionsJSON decodes the connectionsJSON document (R3.4) into a map
// of connection id → definition. Shape (design-connection-injection-channel §4):
//
//	{"connections": {"<id>": {"api": "openai", "base_url": "...", "credential": {...}}}}
//
// Empty input yields nil. Definitions are non-secret; credential carries only a
// source declaration (jwt/keychain/env/none), never a value.
func parseConnectionsJSON(connectionsJSON string) (map[string]connectionDefinition, error) {
	if connectionsJSON == "" {
		return nil, nil
	}
	var doc struct {
		Connections map[string]connectionDefinition `json:"connections"`
	}
	if err := json.Unmarshal([]byte(connectionsJSON), &doc); err != nil {
		return nil, fmt.Errorf("invalid connectionsJSON: %w", err)
	}
	return doc.Connections, nil
}

// connectionDefinition is the non-secret wire shape of a connection.
// Models lists the wire models this connection exposes; a connection with
// no models entry (gateway) creates a single model with an empty wire model
// (gateway chooses the model). When models is non-empty, each entry becomes
// one ModelConfig sharing the connection's base_url and credential.
type connectionDefinition struct {
	API        string                    `json:"api"`
	BaseURL    string                    `json:"base_url"`
	Credential *connectionCredentialDecl `json:"credential"`
	Models     []connectionModelDef      `json:"models"`
}

// connectionModelDef is one model profile within a connection definition.
// WireModelID is the string sent in the API request body; RuntimeAlias is the
// host-facing alias for the model picker (used as the friendly name when set);
// DisplayName is an optional human label.
type connectionModelDef struct {
	WireModelID  string `json:"wire_model_id"`
	RuntimeAlias string `json:"runtime_alias"`
	DisplayName  string `json:"display_name,omitempty"`
}

// connectionCredentialDecl declares where a connection's credential comes from
// (non-secret). It never carries a secret value.
//
// Namespace is explicit when the host wants to route outside the default for
// its Source: "gateway" (gateway/default) or an llm/<ref> name. When omitted,
// Source determines the namespace: jwt/injected → gateway/default, env → llm/<id>.
type connectionCredentialDecl struct {
	Source string `json:"source"` // "jwt" | "keychain" | "env" | "none"
	// Namespace overrides the Source-derived credential target. Reserved
	// value "gateway" → gateway/default; any other non-empty value is an
	// llm/<namespace> BYOK credential name.
	Namespace string `json:"namespace,omitempty"`
	Ref       string `json:"ref"` // credential name (llm/<ref>); legacy alias kept for compat
	Env       string `json:"env"` // env var name (source == env)
}

// applyConnections merges injected connection definitions into a Config: each
// model listed in a definition becomes a model keyed by its wire model (friendly
// name), sharing the connection's base_url and credential. When a definition
// declares no models (the gateway case), a single model with an empty wire model
// is created under the connection id so the gateway-picks-model semantic is
// preserved. Injected connections act as the top layer (design §8.3 层级 4) —
// they are added, not replacing config-declared models.
func applyConnections(cfg *app.Config, conns map[string]connectionDefinition) {
	if len(conns) == 0 {
		return
	}
	if cfg.Models == nil {
		cfg.Models = map[string]app.ModelConfig{}
	}
	for id, def := range conns {
		if id == "" {
			continue
		}
		api := def.API
		if api == "" {
			api = "openai"
		}
		var cred app.CredentialRef
		apiKeyEnv := ""
		if def.Credential != nil {
			// Resolve the credential target. Namespace, when set, wins; the
			// fallback discriminates on Ref (the wire uses source:"injected"
			// for BOTH gateway and BYOK — ref "gateway" vs <id> is the
			// differentiator). env → llm/<id>.
			ns := def.Credential.Namespace
			if ns == "" {
				switch def.Credential.Source {
				case "jwt", "injected", "keychain":
					if def.Credential.Ref == "gateway" || def.Credential.Ref == "" {
						ns = "gateway"
					} else {
						ns = "llm"
					}
				case "env":
					ns = "llm"
				}
			}
			switch ns {
			case "gateway":
				cred = app.CredentialRef{Namespace: "gateway", Name: "default"}
			case "llm":
				name := def.Credential.Ref
				if name == "" {
					name = id
				}
				cred = app.CredentialRef{Namespace: "llm", Name: name}
				if def.Credential.Source == "env" && def.Credential.Env != "" {
					apiKeyEnv = def.Credential.Env
				}
			}
		}
		// Record credential config for injected connections so the catalog
		// builder (probeModelAvailability) knows these are not env-sourced.
		// Done before the models/gateway branch so the gateway `continue`
		// below still records it.
		if def.Credential != nil {
			src := def.Credential.Source
			if src == "injected" || src == "jwt" || src == "keychain" {
				if !cred.IsZero() {
					if cfg.Credentials == nil {
						cfg.Credentials = map[string]map[string]app.CredentialConfig{}
					}
					if cfg.Credentials[cred.Namespace] == nil {
						cfg.Credentials[cred.Namespace] = map[string]app.CredentialConfig{}
					}
					cfg.Credentials[cred.Namespace][cred.Name] = app.CredentialConfig{Source: "injected"}
				}
			}
		}
		if len(def.Models) == 0 {
			// Gateway / fallback: single model with empty wire model
			// (gateway chooses). The friendly name is the connection id.
			cfg.Models[id] = app.ModelConfig{
				Name:       id,
				Provider:   api,
				BaseURL:    def.BaseURL,
				Model:      "",
				Credential: cred,
				APIKeyEnv:  apiKeyEnv,
			}
			continue
		}
		for _, m := range def.Models {
			if m.WireModelID == "" {
				continue
			}
			// Friendly name = host's runtime alias when provided (it is the
			// key the host's model picker and the catalog use); otherwise the
			// wire model id. DisplayName is presentation-only.
			friendlyName := m.WireModelID
			if m.RuntimeAlias != "" {
				friendlyName = m.RuntimeAlias
			}
			mc := app.ModelConfig{
				Name:       friendlyName,
				Provider:   api,
				BaseURL:    def.BaseURL,
				Model:      m.WireModelID,
				Credential: cred,
				APIKeyEnv:  apiKeyEnv,
			}
			if m.DisplayName != "" {
				mc.Catalog = app.ModelCatalogMetadata{DisplayName: m.DisplayName}
			}
			cfg.Models[friendlyName] = mc
		}
	}
}

// cannot bridge a map, so secrets cross as a JSON string). Empty input yields a
// nil map.
//
// The top-level JSON object may contain two shapes of values:
//   - Old format: plain strings  → {"DEEPSEEK_API_KEY": "sk-xxx"}
//   - New format: JSON objects   → {"gateway/default": {"type":"bearer","secret":"..."}}
//
// Both are returned as string values in the map; object values are stored as their
// raw JSON text so injectSecrets can parse them further.
func parseSecretsJSON(secretsJSON string) (map[string]string, error) {
	if secretsJSON == "" {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(secretsJSON), &raw); err != nil {
		return nil, fmt.Errorf("invalid secretsJSON: %w", err)
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			result[k] = s
		} else {
			// Value is a JSON object — store raw so injectSecrets can parse it.
			result[k] = string(v)
		}
	}
	return result, nil
}

// injectSecrets overrides resolved API keys from the host-supplied secrets map.
// A secret may be keyed by a model's api_key_env name, by its friendly name, by
// a {namespace}/{name} credential target, or by a flat connection id (R3.3
// three-form bridging). Empty values are ignored.
//
// Web search provider keys (Tavily, Brave) are also injected here: a secret whose
// key matches the configured tavily_api_key_env or brave_api_key_env is set on the
// WebSearchConfig, following the same pattern as model keys.
//
// The injected secrets become a StaticResolver (for the dynamic credential
// path) and, for legacy single-value keys, populate the matching model's
// Credential ref so the two-level resolver can serve it. This no longer writes
// ModelConfig.APIKey (R1.1: the APIKey field is deprecated and on its way out).
func injectSecrets(cfg *app.Config, secrets map[string]string) credential.Resolver {
	if len(secrets) == 0 {
		return nil
	}
	resolver := make(credential.StaticResolver)

	for key, val := range secrets {
		if val == "" {
			continue
		}
		// Detect format: keys containing '/' or '%2F' are credential targets
		// ({namespace}/{name}); plain keys are either flat connection ids
		// (R3.3) or legacy env-var/friendly names.
		if strings.Contains(key, "/") || strings.Contains(key, "%2F") {
			target, err := parseTargetKey(key)
			if err != nil {
				continue
			}
			cred, err := parseCredentialValue(val)
			if err != nil {
				continue
			}
			resolver[target] = cred
			// Also align the model's Credential ref for backward compat so the
			// static resolver is reached via the ref, not the (deprecated)
			// APIKey field.
			for name, mc := range cfg.Models {
				if mc.Credential.Namespace == target.Namespace && mc.Credential.Name == target.Name {
					cfg.Models[name] = mc
				}
			}
			continue
		}
		// Flat connection id (R3.3): map to the canonical target and inject.
		if target := credential.TargetFromConnectionID(key); target.Namespace != "" {
			cred, err := parseCredentialValue(val)
			if err == nil {
				resolver[target] = cred
				continue
			}
		}
		// Legacy env-var name → plain string value. The plain string IS the
		// secret (old format had no JSON envelope), so inject it directly as a
		// bearer credential on llm/<name> and align the model's Credential ref.
		for name, mc := range cfg.Models {
			if key == mc.APIKeyEnv || key == name {
				if mc.Credential.IsZero() {
					mc.Credential = app.CredentialRef{Namespace: "llm", Name: name}
					cfg.Models[name] = mc
				}
				resolver[credential.Target{Namespace: "llm", Name: name}] = credential.Credential{
					Type:   credential.Bearer,
					Secret: val,
				}
			}
		}
		// Web search provider keys.
		if cfg.Web.Search.TavilyAPIKeyEnv != "" && key == cfg.Web.Search.TavilyAPIKeyEnv {
			cfg.Web.Search.TavilyKey = val
		}
		if cfg.Web.Search.BraveAPIKeyEnv != "" && key == cfg.Web.Search.BraveAPIKeyEnv {
			cfg.Web.Search.BraveKey = val
		}
	}
	if len(resolver) == 0 {
		return nil
	}
	return resolver
}

// parseTargetKey decodes a "{namespace}/{name}" key back into a Target.
// The components may be url.PathEscape-encoded per the injection contract.
func parseTargetKey(key string) (credential.Target, error) {
	idx := strings.LastIndex(key, "/")
	if idx < 0 {
		return credential.Target{}, fmt.Errorf("invalid target key %q: missing '/'", key)
	}
	namespace, err := url.PathUnescape(key[:idx])
	if err != nil {
		return credential.Target{}, fmt.Errorf("invalid target key %q: %w", key, err)
	}
	name, err := url.PathUnescape(key[idx+1:])
	if err != nil {
		return credential.Target{}, fmt.Errorf("invalid target key %q: %w", key, err)
	}
	return credential.Target{Namespace: namespace, Name: name}, nil
}

// parseCredentialValue parses a JSON credential object from a string value.
func parseCredentialValue(raw string) (credential.Credential, error) {
	var c struct {
		Type      string `json:"type"`
		Secret    string `json:"secret"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return credential.Credential{}, err
	}
	if c.Type == "" || c.Secret == "" {
		return credential.Credential{}, fmt.Errorf("credential value missing type or secret")
	}
	cred := credential.Credential{
		Type:   credential.CredentialType(c.Type),
		Secret: c.Secret,
	}
	if c.ExpiresAt != nil {
		t := time.Unix(*c.ExpiresAt, 0)
		cred.ExpiresAt = &t
	}
	return cred, nil
}

// LoopbackURL returns the ws scheme base URL the host should hand to its client,
// e.g. for building the conversation stream endpoint.
func (h *Handle) LoopbackURL() string {
	return h.loopbackEndpoint
}
