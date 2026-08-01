// Package controlplane provides the process-local Runtime Ownership layer used
// by the cross-session control plane. Ownership is an expiring lease in a
// dedicated control.db; it is deliberately separate from the derived index.db.
package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"code-agent/internal/session"
	"code-agent/internal/sessionfork"
	"code-agent/internal/tools"

	_ "modernc.org/sqlite"
)

const (
	ProtocolVersion        = 3
	defaultLeaseTTL        = 15 * time.Second
	defaultHeartbeat       = 5 * time.Second
	ownerIdentityPath      = "/control/v1/identity"
	ownerSessionPathPrefix = "/control/v1/sessions/"
)

var (
	ErrTargetOffline    = errors.New("control plane: target owner is offline")
	ErrProtocolMismatch = errors.New("control plane: owner protocol mismatch")
)

// SessionRepository is the narrow persistence surface needed by the ownership
// service. The target Runtime remains the authority for all execution behavior.
type SessionRepository interface {
	List(context.Context) ([]session.Meta, error)
	Load(context.Context, string) (*session.Session, error)
}

type ActivitySource interface {
	HasActivity(sessionID string) bool
}

type Target interface {
	Accept(context.Context, context.Context, string, tools.SessionSendRequest) (tools.SessionDelivery, error)
	EventsSince(context.Context, string, string, int64) (*tools.SessionWaitCompletion, int64, error)
	CreateSession(context.Context, tools.SessionCreateRequest, string) (tools.SessionSpawnResult, error)
	ForkSession(context.Context, sessionfork.Request, string) (sessionfork.Result, error)
}

// Config controls lease timing. Zero values use production defaults. Now exists
// for deterministic expiry tests and must return UTC-compatible wall time.
type Config struct {
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	Now               func() time.Time
}

// Identity describes one live Runtime instance. InstanceID changes on every
// start; ServerID is the existing durable Runtime identity.
type Identity struct {
	InstanceID      string `json:"instance_id"`
	ServerID        string `json:"server_id"`
	ProtocolVersion int    `json:"protocol_version"`
	Endpoint        string `json:"endpoint"`
	PID             int    `json:"pid"`
	StartedAt       string `json:"started_at"`
}

// Lease is the resolved owner route for a session. AuthToken is intentionally
// excluded from JSON and is used only by the local Router client.
type Lease struct {
	SessionID       string `json:"session_id"`
	InstanceID      string `json:"instance_id"`
	ServerID        string `json:"server_id"`
	ProtocolVersion int    `json:"protocol_version"`
	Endpoint        string `json:"endpoint"`
	ExpiresAt       string `json:"expires_at"`
	AuthToken       string `json:"-"`
}

// SessionState is a bounded owner-authoritative description used to prove the
// route. Admission and event polling use separate authenticated B1 endpoints.
type SessionState struct {
	SessionID     string `json:"session_id"`
	InstanceID    string `json:"instance_id"`
	WorkspacePath string `json:"workspace_path"`
	TurnStatus    string `json:"turn_status"`
	Active        bool   `json:"active"`
}

type Resolution struct {
	Lease Lease        `json:"lease"`
	State SessionState `json:"state"`
	Local bool         `json:"local"`
}

type Manager struct {
	db       *sql.DB
	dbPath   string
	repo     SessionRepository
	activity ActivitySource
	target   Target
	runCtx   context.Context
	cfg      Config
	identity Identity
	token    string
	client   *http.Client

	mu        sync.Mutex
	started   bool
	closing   bool
	closed    bool
	closeDone chan struct{}
	closeErr  error
	cancel    context.CancelFunc
	listener  net.Listener
	server    *http.Server
	wg        sync.WaitGroup
}

func (m *Manager) SetTarget(target Target) {
	m.mu.Lock()
	m.target = target
	m.mu.Unlock()
}

var localOwners = struct {
	sync.RWMutex
	byInstance map[string]*Manager
}{byInstance: make(map[string]*Manager)}

const schema = `
CREATE TABLE IF NOT EXISTS runtime_instances (
	instance_id      TEXT PRIMARY KEY,
	server_id        TEXT NOT NULL,
	protocol_version INTEGER NOT NULL,
	endpoint         TEXT NOT NULL,
	auth_token       TEXT NOT NULL,
	pid              INTEGER NOT NULL,
	started_at_ms    INTEGER NOT NULL,
	heartbeat_at_ms  INTEGER NOT NULL,
	expires_at_ms    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS session_owners (
	session_id       TEXT PRIMARY KEY,
	instance_id      TEXT NOT NULL,
	claimed_at_ms    INTEGER NOT NULL,
	heartbeat_at_ms  INTEGER NOT NULL,
	expires_at_ms    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_owners_instance ON session_owners(instance_id);
CREATE INDEX IF NOT EXISTS idx_session_owners_expiry ON session_owners(expires_at_ms);
CREATE INDEX IF NOT EXISTS idx_runtime_instances_expiry ON runtime_instances(expires_at_ms);
CREATE TABLE IF NOT EXISTS session_spawn_edges (
	request_id        TEXT NOT NULL UNIQUE,
	payload_hash      TEXT NOT NULL,
	parent_session_id TEXT NOT NULL,
	child_session_id  TEXT NOT NULL PRIMARY KEY,
	source_session_id TEXT NOT NULL DEFAULT '',
	kind              TEXT NOT NULL,
	status            TEXT NOT NULL DEFAULT 'provisioning',
	created_at_ms     INTEGER NOT NULL,
	updated_at_ms     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_spawn_edges_parent ON session_spawn_edges(parent_session_id, created_at_ms);
CREATE INDEX IF NOT EXISTS idx_session_spawn_edges_source ON session_spawn_edges(source_session_id, created_at_ms);
`

func NewManager(baseDir, serverID string, repo SessionRepository, activity ActivitySource, cfg Config) (*Manager, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, errors.New("control plane: base directory is required")
	}
	if strings.TrimSpace(serverID) == "" {
		return nil, errors.New("control plane: server id is required")
	}
	if repo == nil {
		return nil, errors.New("control plane: session repository is required")
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeat
	}
	if cfg.HeartbeatInterval >= cfg.LeaseTTL {
		return nil, errors.New("control plane: heartbeat interval must be shorter than lease TTL")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	instanceID, err := randomSecret(18)
	if err != nil {
		return nil, fmt.Errorf("control plane: generate instance id: %w", err)
	}
	token, err := randomSecret(32)
	if err != nil {
		return nil, fmt.Errorf("control plane: generate auth token: %w", err)
	}
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("control plane: create state directory: %w", err)
	}
	path := filepath.Join(baseDir, "control.db")
	file, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("control plane: create database: %w", err)
	}
	_ = file.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("control plane: secure database permissions: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("control plane: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control plane: migrate database: %w", err)
	}
	return &Manager{
		db: db, dbPath: path, repo: repo, activity: activity, cfg: cfg, token: token,
		identity:  Identity{InstanceID: instanceID, ServerID: serverID, ProtocolVersion: ProtocolVersion, PID: os.Getpid()},
		client:    &http.Client{Timeout: 3 * time.Second},
		closeDone: make(chan struct{}),
	}, nil
}

func randomSecret(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (m *Manager) Identity() Identity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identity
}

// Start opens an authenticated loopback-only RPC sidecar, performs the initial
// atomic claim, and begins renewing the Runtime and session leases.
func (m *Manager) Start(parent context.Context) error {
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return errors.New("control plane: manager is closed")
	}
	if m.started {
		m.mu.Unlock()
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("control plane: listen: %w", err)
	}
	now := m.cfg.Now().UTC()
	m.identity.Endpoint = "http://" + listener.Addr().String()
	m.identity.StartedAt = now.Format(time.RFC3339Nano)
	ctx, cancel := context.WithCancel(parent)
	m.runCtx = ctx
	m.cancel = cancel
	m.listener = listener
	m.server = &http.Server{Handler: m.handler(), ReadHeaderTimeout: 5 * time.Second}
	m.started = true
	m.mu.Unlock()

	if err := m.Heartbeat(ctx); err != nil {
		_ = listener.Close()
		_ = m.releaseLeases(context.Background())
		m.mu.Lock()
		m.started = false
		m.listener = nil
		m.server = nil
		m.cancel = nil
		m.mu.Unlock()
		cancel()
		return err
	}

	localOwners.Lock()
	localOwners.byInstance[m.identity.InstanceID] = m
	localOwners.Unlock()

	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		if err := m.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[control-plane] owner RPC stopped: %v\n", err)
		}
	}()
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.Heartbeat(ctx); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "[control-plane] heartbeat: %v\n", err)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = m.Close()
	}()
	return nil
}

// Heartbeat refreshes the Runtime lease and reconciles its complete session set
// atomically. An unexpired owner cannot be stolen by another Runtime instance.
func (m *Manager) Heartbeat(ctx context.Context) error {
	metas, listErr := m.repo.List(ctx)
	now := m.cfg.Now().UTC()
	nowMS := now.UnixMilli()
	expiresMS := now.Add(m.cfg.LeaseTTL).UnixMilli()
	identity := m.Identity()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("control plane: begin heartbeat: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runtime_instances (instance_id, server_id, protocol_version, endpoint, auth_token, pid, started_at_ms, heartbeat_at_ms, expires_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET
			server_id=excluded.server_id, protocol_version=excluded.protocol_version,
			endpoint=excluded.endpoint, auth_token=excluded.auth_token, pid=excluded.pid,
			heartbeat_at_ms=excluded.heartbeat_at_ms, expires_at_ms=excluded.expires_at_ms`,
		identity.InstanceID, identity.ServerID, identity.ProtocolVersion, identity.Endpoint, m.token,
		identity.PID, nowMS, nowMS, expiresMS); err != nil {
		return fmt.Errorf("control plane: renew runtime: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_owners WHERE expires_at_ms <= ?`, nowMS); err != nil {
		return fmt.Errorf("control plane: clean session leases: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_instances WHERE expires_at_ms <= ? AND instance_id <> ?`, nowMS, identity.InstanceID); err != nil {
		return fmt.Errorf("control plane: clean runtime leases: %w", err)
	}
	if listErr == nil {
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS owner_seen_sessions (session_id TEXT PRIMARY KEY)`); err != nil {
			return fmt.Errorf("control plane: create owner seen set: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM owner_seen_sessions`); err != nil {
			return fmt.Errorf("control plane: reset owner seen set: %w", err)
		}
		for _, meta := range metas {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO owner_seen_sessions(session_id) VALUES (?)`, meta.ID); err != nil {
				return fmt.Errorf("control plane: record session %s: %w", meta.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO session_owners (session_id, instance_id, claimed_at_ms, heartbeat_at_ms, expires_at_ms)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(session_id) DO UPDATE SET
					instance_id=excluded.instance_id,
					claimed_at_ms=CASE WHEN session_owners.instance_id=excluded.instance_id THEN session_owners.claimed_at_ms ELSE excluded.claimed_at_ms END,
					heartbeat_at_ms=excluded.heartbeat_at_ms, expires_at_ms=excluded.expires_at_ms
				WHERE session_owners.instance_id=excluded.instance_id OR session_owners.expires_at_ms <= ?`,
				meta.ID, identity.InstanceID, nowMS, nowMS, expiresMS, nowMS); err != nil {
				return fmt.Errorf("control plane: claim session %s: %w", meta.ID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_owners WHERE instance_id=? AND session_id NOT IN (SELECT session_id FROM owner_seen_sessions)`, identity.InstanceID); err != nil {
			return fmt.Errorf("control plane: release removed sessions: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE session_owners SET heartbeat_at_ms=?, expires_at_ms=? WHERE instance_id=?`, nowMS, expiresMS, identity.InstanceID); err != nil {
			return fmt.Errorf("control plane: renew existing session leases: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("control plane: commit heartbeat: %w", err)
	}
	if listErr != nil {
		return fmt.Errorf("control plane: list sessions: %w", listErr)
	}
	return nil
}

func (m *Manager) Resolve(ctx context.Context, sessionID string) (*Resolution, error) {
	lease, err := m.resolveLease(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	localOwners.RLock()
	local := localOwners.byInstance[lease.InstanceID]
	localOwners.RUnlock()
	if local != nil {
		state, err := local.describeOwned(ctx, sessionID)
		if err != nil {
			return nil, offline(sessionID, err)
		}
		return &Resolution{Lease: *lease, State: *state, Local: true}, nil
	}
	state, err := m.describeRemote(ctx, *lease)
	if err != nil {
		return nil, offline(sessionID, err)
	}
	return &Resolution{Lease: *lease, State: *state, Local: false}, nil
}

func (m *Manager) resolveLease(ctx context.Context, sessionID string) (*Lease, error) {
	nowMS := m.cfg.Now().UTC().UnixMilli()
	var lease Lease
	var expiresMS int64
	err := m.db.QueryRowContext(ctx, `
		SELECT o.session_id, o.instance_id, r.server_id, r.protocol_version, r.endpoint, r.auth_token, o.expires_at_ms
		FROM session_owners o JOIN runtime_instances r ON r.instance_id=o.instance_id
		WHERE o.session_id=? AND o.expires_at_ms>? AND r.expires_at_ms>?`, sessionID, nowMS, nowMS).
		Scan(&lease.SessionID, &lease.InstanceID, &lease.ServerID, &lease.ProtocolVersion, &lease.Endpoint, &lease.AuthToken, &expiresMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, offline(sessionID, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("control plane: resolve owner: %w", err)
	}
	if lease.ProtocolVersion != ProtocolVersion {
		return nil, fmt.Errorf("%w: target=%d local=%d", ErrProtocolMismatch, lease.ProtocolVersion, ProtocolVersion)
	}
	if err := validateLoopbackEndpoint(lease.Endpoint); err != nil {
		return nil, offline(sessionID, err)
	}
	lease.ExpiresAt = time.UnixMilli(expiresMS).UTC().Format(time.RFC3339Nano)
	return &lease, nil
}

func validateLoopbackEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("invalid owner endpoint %q", endpoint)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("owner endpoint is not loopback: %q", endpoint)
	}
	return nil
}

func offline(sessionID string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: session %s", ErrTargetOffline, sessionID)
	}
	return fmt.Errorf("%w: session %s: %v", ErrTargetOffline, sessionID, cause)
}

func (m *Manager) describeRemote(ctx context.Context, lease Lease) (*SessionState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(lease.Endpoint, "/")+ownerSessionPathPrefix+url.PathEscape(lease.SessionID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+lease.AuthToken)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("owner RPC returned %s", resp.Status)
	}
	var state SessionState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}
	if state.InstanceID != lease.InstanceID || state.SessionID != lease.SessionID {
		return nil, errors.New("owner RPC identity mismatch")
	}
	return &state, nil
}

func (m *Manager) describeOwned(ctx context.Context, sessionID string) (*SessionState, error) {
	owned, err := m.owns(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, errors.New("session lease is not owned by this Runtime")
	}
	sess, err := m.repo.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	status, _ := sess.Metadata["turn_status"].(string)
	active := false
	if m.activity != nil {
		active = m.activity.HasActivity(sessionID)
	}
	return &SessionState{SessionID: sessionID, InstanceID: m.identity.InstanceID, WorkspacePath: sess.WorkspacePath, TurnStatus: status, Active: active}, nil
}

func (m *Manager) owns(ctx context.Context, sessionID string) (bool, error) {
	var count int
	err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_owners WHERE session_id=? AND instance_id=? AND expires_at_ms>?`, sessionID, m.identity.InstanceID, m.cfg.Now().UTC().UnixMilli()).Scan(&count)
	return count == 1, err
}

func (m *Manager) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ownerIdentityPath, func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, m.Identity())
	})
	mux.HandleFunc("GET "+ownerSessionPathPrefix+"{id}", func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		state, err := m.describeOwned(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, "target offline", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST "+ownerSessionPathPrefix+"{id}/turns", func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request tools.SessionSendRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		request.TargetSessionID = r.PathValue("id")
		delivery, err := m.acceptOwned(r.Context(), request.TargetSessionID, request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusAccepted, delivery)
	})
	mux.HandleFunc("GET "+ownerSessionPathPrefix+"{id}/events", func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cursor, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil || cursor < 0 || r.URL.Query().Get("turn_id") == "" {
			http.Error(w, "invalid event cursor", http.StatusBadRequest)
			return
		}
		completion, latest, err := m.eventsOwned(r.Context(), r.PathValue("id"), r.URL.Query().Get("turn_id"), cursor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, eventPollResponse{Completion: completion, Cursor: latest})
	})
	mux.HandleFunc("POST "+ownerSessionPathPrefix+"{id}/forks", func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var rpc spawnRPCRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rpc); err != nil || rpc.ChildID == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		rpc.Request.SourceSessionID = r.PathValue("id")
		result, err := m.forkOwned(r.Context(), rpc.Request, rpc.ChildID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	})
	return mux
}

func (m *Manager) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	if len(got) != len(m.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(m.token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed || m.closing {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		m.mu.Lock()
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	m.closing = true
	cancel := m.cancel
	server := m.server
	listener := m.listener
	m.mu.Unlock()

	localOwners.Lock()
	delete(localOwners.byInstance, m.identity.InstanceID)
	localOwners.Unlock()
	if cancel != nil {
		cancel()
	}
	if server != nil {
		ctx, done := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(ctx)
		done()
	} else if listener != nil {
		_ = listener.Close()
	}
	m.wg.Wait()

	err := m.releaseLeases(context.Background())
	closeErr := m.db.Close()
	if err == nil {
		err = closeErr
	}
	m.mu.Lock()
	m.closeErr = err
	m.closed = true
	m.closing = false
	close(m.closeDone)
	m.mu.Unlock()
	return err
}

func (m *Manager) releaseLeases(ctx context.Context) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_owners WHERE instance_id=?`, m.identity.InstanceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_instances WHERE instance_id=?`, m.identity.InstanceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) DBPath() string { return m.dbPath }
