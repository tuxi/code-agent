// Package mobile is the gomobile-bind surface for embedding the codeagent runtime
// inside an iOS or macOS app. `gomobile bind -target=ios ./mobile` produces a
// CodeAgent.xcframework that Swift (AgentKit) links directly; the app starts the
// runtime in-process and connects to it over the loopback agent-wire endpoint,
// exactly as it would against a remote Mac server.
//
// The surface is deliberately narrow because gomobile only bridges a restricted
// set of types across the language boundary: signed integers, floats, bool,
// string, []byte, error, and exported struct pointers with bindable methods.
// Notably it does NOT bridge maps — so secrets are passed as a JSON string and
// decoded here, rather than as map[string]string.
package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code-agent/internal/embed"
	"code-agent/internal/server"
)

// Server is a running embedded codeagent runtime. gomobile binds it as a class on
// the host side (e.g. a Swift `Server` object); its methods become instance
// methods, and the (value, error) returns become throwing calls.
type Server struct {
	h *embed.Handle
}

// Start brings up the agent-wire server on the loopback interface and returns once
// it is listening. On the Swift side this is a throwing call returning a Server.
//
//   - workspaceDir: the agent's working root — on iOS a writable directory the
//     agent reads and writes (e.g. the Documents directory). Required.
//   - dataDir:      a writable directory for the runtime's own data (session DBs).
//     On iOS pass Library/Application Support; pass "" to fall back to workspaceDir.
//     The desktop $HOME default does not work on iOS ($HOME is read-only).
//   - configYAML:   the raw config document; pass "" for built-in defaults.
//   - modelName:    which configured model to use; pass "" for default_model.
//   - secretsJSON:  a JSON object of API keys, keyed by a model's api_key_env name
//     or its friendly name, e.g. {"DEEPSEEK_API_KEY":"sk-..."}. Pass "" for none.
//     These come from the iOS Keychain and never touch the environment or disk.
//   - serverAccessToken: a fresh 256-bit random Bearer token kept only in host
//     memory for this Runtime process.
//   - addr:         listen address; pass "" for an OS-assigned ephemeral port on
//     127.0.0.1 (read it back via Server.Port).
//   - sandboxed:    pass true on iOS to assemble the sandboxed toolset (no shell,
//     git, gopls, MCP, or hooks). A non-sandboxed macOS host may pass false for
//     the full desktop toolset.
func Start(workspaceDir, dataDir, configYAML, modelName, secretsJSON, serverAccessToken, addr string, sandboxed bool) (*Server, error) {
	secrets, err := parseSecrets(secretsJSON)
	if err != nil {
		return nil, err
	}
	h, err := embed.StartServer(context.Background(), embed.Options{
		WorkspaceDir:      workspaceDir,
		DataDir:           dataDir,
		ConfigYAML:        configYAML,
		ModelName:         modelName,
		Secrets:           secrets,
		ServerAccessToken: serverAccessToken,
		Addr:              addr,
		Sandboxed:         sandboxed,
	})
	if err != nil {
		return nil, err
	}
	return &Server{h: h}, nil
}

// Port returns the TCP port the server is listening on — the OS-assigned ephemeral
// port when Start was called with addr "". Hand this to AgentKit to build the
// connection URL.
func (s *Server) Port() int {
	if s == nil || s.h == nil {
		return 0
	}
	return s.h.Port()
}

// Endpoint returns the loopback WebSocket base URL, e.g. "ws://127.0.0.1:54321".
// Named Endpoint rather than URL so gomobile maps it to a clean Swift `.endpoint()`
// (a leading-acronym name like URL would become the ugly `.uRL()`).
func (s *Server) Endpoint() string {
	if s == nil || s.h == nil {
		return ""
	}
	return s.h.LoopbackURL()
}

type sharedListenerConfig struct {
	Addr               string                      `json:"addr"`
	CertificatePEM     string                      `json:"certificate_pem"`
	PrivateKeyPEM      string                      `json:"private_key_pem"`
	BootstrapSHA256    string                      `json:"bootstrap_sha256"`
	BootstrapExpiresAt int64                       `json:"bootstrap_expires_at"`
	Devices            []server.SharedDeviceRecord `json:"devices"`
	EnrollmentTimeout  int64                       `json:"enrollment_timeout_seconds"`
}

// StartSharedListener enables the TLS-only LAN surface around this Embedded
// Runtime. configJSON is an in-memory control-plane document supplied by
// AgentKit; it must not be persisted by the Runtime.
func (s *Server) StartSharedListener(configJSON string) error {
	if s == nil || s.h == nil {
		return fmt.Errorf("runtime not started")
	}
	var config sharedListenerConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return fmt.Errorf("invalid shared listener config: %w", err)
	}
	var bootstrapExpiry time.Time
	if config.BootstrapExpiresAt > 0 {
		bootstrapExpiry = time.Unix(config.BootstrapExpiresAt, 0)
	}
	return s.h.StartSharedListener(embed.SharedListenerOptions{
		Addr:               config.Addr,
		CertificatePEM:     config.CertificatePEM,
		PrivateKeyPEM:      config.PrivateKeyPEM,
		BootstrapSHA256:    config.BootstrapSHA256,
		BootstrapExpiresAt: bootstrapExpiry,
		Devices:            config.Devices,
		EnrollmentTimeout:  time.Duration(config.EnrollmentTimeout) * time.Second,
	})
}

func (s *Server) StopSharedListener() error {
	if s == nil || s.h == nil {
		return nil
	}
	return s.h.StopSharedListener()
}

func (s *Server) SharedPort() int {
	if s == nil || s.h == nil {
		return 0
	}
	return s.h.SharedPort()
}

func (s *Server) SharedEndpoint() string {
	if s == nil || s.h == nil {
		return ""
	}
	return s.h.SharedEndpoint()
}

// SharedListenerStatus returns the current state, bound-listener diagnostics and
// most recent listener error as JSON. listen_origin is not an advertised client
// endpoint when it contains a wildcard host.
func (s *Server) SharedListenerStatus() (string, error) {
	status := embed.SharedListenerStatus{State: embed.SharedListenerStopped}
	if s != nil && s.h != nil {
		status = s.h.SharedListenerStatus()
	}
	data, err := json.Marshal(status)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PendingSharedEnrollments returns non-secret enrollment validation material as
// JSON. AgentKit persists it before acknowledging the enrollment.
func (s *Server) PendingSharedEnrollments() (string, error) {
	if s == nil || s.h == nil {
		return "[]", nil
	}
	data, err := json.Marshal(s.h.PendingSharedEnrollments())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Server) AcknowledgeSharedEnrollment(enrollmentID string) error {
	if s == nil || s.h == nil {
		return fmt.Errorf("runtime not started")
	}
	return s.h.AcknowledgeSharedEnrollment(enrollmentID)
}

func (s *Server) RejectSharedEnrollment(enrollmentID string) error {
	if s == nil || s.h == nil {
		return fmt.Errorf("runtime not started")
	}
	return s.h.RejectSharedEnrollment(enrollmentID)
}

func (s *Server) UpdateSharedDevices(devicesJSON string) error {
	if s == nil || s.h == nil {
		return fmt.Errorf("runtime not started")
	}
	var devices []server.SharedDeviceRecord
	if err := json.Unmarshal([]byte(devicesJSON), &devices); err != nil {
		return fmt.Errorf("invalid shared devices: %w", err)
	}
	return s.h.UpdateSharedDevices(devices)
}

func (s *Server) RotateSharedBootstrap(bootstrapSHA256 string, expiresAtUnix int64) error {
	if s == nil || s.h == nil {
		return fmt.Errorf("runtime not started")
	}
	return s.h.RotateSharedBootstrap(
		bootstrapSHA256,
		time.Unix(expiresAtUnix, 0),
	)
}

// Suspend cancels in-flight turns and marks them paused, then returns within a
// bounded window (v1.2 §3.1). Call it in the app's background grace window —
// NOT Stop, which now tears the runtime down. The process stays alive and
// resumable; on return to the foreground call ResumeSession. Safe when idle and
// when called repeatedly. Swift: `suspend() throws`.
func (s *Server) Suspend() error {
	if s == nil || s.h == nil {
		return nil
	}
	return s.h.Suspend()
}

// ResumeSession continues a paused turn for sessionID (v1.2 §3.2). It returns
// immediately after validating the session; the resumed turn runs asynchronously
// and its progress/outcome arrive over the WebSocket event stream (turn_resumed /
// turn_finished / turn_paused / turn_failed) and the conversation's turn_status.
// The host calls this on foreground for the active session (silent auto-resume),
// or when the user taps "continue" on a cold-start paused session. The error
// covers only failure to start (e.g. unknown session). Swift:
// `resumeSession(_ sessionID: String) throws`.
func (s *Server) ResumeSession(sessionID string) error {
	if s == nil || s.h == nil {
		return nil
	}
	return s.h.ResumeSession(sessionID)
}

// Reconfigure hot-swaps API keys and/or the model without dropping the server or
// changing the port (v1.2 §3.3) — the setting-page path that replaces the old
// restart(). secretsJSON is the same shape as Start's (pass "" to keep the current
// keys); modelName selects a configured model (pass "" to keep the current one).
// The swap lands at the next turn; in-flight turns finish on the old config.
// Swift: `reconfigure(secretsJSON:modelName:) throws`.
func (s *Server) Reconfigure(secretsJSON, modelName string) error {
	if s == nil || s.h == nil {
		return nil
	}
	return s.h.Reconfigure("", secretsJSON, modelName)
}

// ReconfigureConnections is the connection-flattening v2 form (R3.2): accepts
// connection DEFINITIONS via connectionsJSON (non-secret, "" = keep current) in
// addition to secrets and model. The 2-argument Reconfigure above is the
// backward-compatible wrapper. Swift: `reconfigure(connectionsJSON:secretsJSON:modelName:) throws`.
func (s *Server) ReconfigureConnections(connectionsJSON, secretsJSON, modelName string) error {
	if s == nil || s.h == nil {
		return nil
	}
	return s.h.Reconfigure(connectionsJSON, secretsJSON, modelName)
}

// Stop shuts the server down and releases all runtime resources. Call it ONLY on
// real teardown — user-initiated quit or a memory warning — NOT on backgrounding
// (use Suspend for that). Safe to call more than once. Swift: `stop() throws`.
func (s *Server) Stop() error {
	if s == nil || s.h == nil {
		return nil
	}
	return s.h.Stop()
}

// parseSecrets decodes the JSON secrets object. Empty input yields a nil map.
func parseSecrets(secretsJSON string) (map[string]string, error) {
	if secretsJSON == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(secretsJSON), &m); err != nil {
		return nil, fmt.Errorf("invalid secretsJSON: %w", err)
	}
	return m, nil
}
