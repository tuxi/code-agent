// Package fixtureserver runs the frozen preview v1 fixtures behind the real
// preview service and Runtime HTTP mux so AgentKit can validate behavior over
// loopback HTTP instead of a second in-process interpretation of the contract.
package fixtureserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"code-agent/internal/conversation"
	"code-agent/internal/preview"
	"code-agent/internal/server"
	"code-agent/internal/session"
)

type Ready struct {
	BaseURL        string `json:"base_url"`
	BearerToken    string `json:"bearer_token"`
	CatalogURL     string `json:"catalog_url"`
	FixtureVersion string `json:"fixture_version"`
	Contract       string `json:"contract"`
	EvaluationTime string `json:"evaluation_time"`
	Authorization  string `json:"authorization"`
}

type Server struct {
	baseURL string
	ready   Ready
	catalog Catalog

	artifactBytes map[string][]byte
	eventBytes    map[string][]byte

	listener   net.Listener
	httpServer *http.Server
	repo       conversation.ConversationRepository
	tempDir    string

	done      chan struct{}
	errMu     sync.Mutex
	serveErr  error
	closeOnce sync.Once
	closeErr  error
}

// Start loads every frozen artifact into the production preview store, wires the
// production Runtime mux, and binds a temporary loopback port.
func Start(fixtureRoot string) (*Server, error) {
	data, err := loadFixtureData(fixtureRoot)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "codeagent-preview-fixtures-*")
	if err != nil {
		return nil, err
	}
	cleanup := func(repo conversation.ConversationRepository, listener net.Listener) {
		if listener != nil {
			_ = listener.Close()
		}
		if repo != nil {
			_ = repo.Close()
		}
		_ = os.RemoveAll(tempDir)
	}
	store, err := preview.NewFSStore(filepath.Join(tempDir, "store"))
	if err != nil {
		cleanup(nil, nil)
		return nil, err
	}
	if err := importFixtureArtifacts(context.Background(), store, data); err != nil {
		cleanup(nil, nil)
		return nil, err
	}
	service := preview.NewService(store)
	service.Now = func() time.Time { return data.evaluationTime }

	workspaceDir := filepath.Join(tempDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
		cleanup(nil, nil)
		return nil, err
	}
	memory := session.NewMemoryStore()
	repo := conversation.NewSQLiteRepository(memory, 128000, 90000, "preview-fixture", "", nil)
	reserved, ok := repo.(conversation.ReservedConversationRepository)
	if !ok {
		cleanup(repo, nil)
		return nil, fmt.Errorf("conversation repository does not reserve fixture session IDs")
	}
	for _, id := range fixtureSessions(data) {
		if _, err := reserved.CreateWithID(context.Background(), id, workspaceDir, ""); err != nil {
			cleanup(repo, nil)
			return nil, fmt.Errorf("create fixture session %s: %w", id, err)
		}
	}
	events := newFixtureEventStore()
	if err := seedRuntimeEvents(data, events); err != nil {
		cleanup(repo, nil)
		return nil, err
	}

	token, err := randomBearerToken()
	if err != nil {
		cleanup(repo, nil)
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cleanup(repo, nil)
		return nil, err
	}
	baseURL := "http://" + listener.Addr().String()
	scenarioSessions := eventScenarioSessions(data.baseSession)
	scenarioSessions["ready-interactive"] = data.baseSession
	catalog := buildCatalog(data, scenarioSessions)

	artifactBytes := make(map[string][]byte, len(data.artifactFiles))
	for scenario, file := range data.artifactFiles {
		raw, err := os.ReadFile(filepath.Join(data.root, filepath.FromSlash(file)))
		if err != nil {
			cleanup(repo, listener)
			return nil, err
		}
		artifactBytes[scenario] = raw
	}
	s := &Server{
		baseURL: baseURL, catalog: catalog,
		artifactBytes: artifactBytes, eventBytes: data.eventBytes,
		listener: listener, repo: repo, tempDir: tempDir, done: make(chan struct{}),
	}
	s.ready = Ready{
		BaseURL: baseURL, BearerToken: token,
		CatalogURL:     baseURL + "/v1/preview-fixtures",
		FixtureVersion: data.manifest.FixtureVersion,
		Contract:       data.manifest.Contract,
		EvaluationTime: data.evaluationTime.Format(time.RFC3339),
		Authorization:  "Bearer " + token,
	}

	core := server.NewMux(repo, events, nil, server.MuxOptions{
		Preview:    service,
		ServerName: "codeagent-preview-fixture",
		RuntimeInfo: server.RuntimeInfo{
			ServerID: "preview-fixture", DisplayName: "CodeAgent Preview Fixture",
			Product: "code-agent", RuntimeProfile: "preview-fixture",
		},
	})
	rootMux := http.NewServeMux()
	rootMux.HandleFunc("GET /v1/preview-fixtures", s.handleCatalog)
	rootMux.HandleFunc("GET /v1/preview-fixtures/artifacts/{scenario}", s.handleArtifact)
	rootMux.HandleFunc("GET /v1/preview-fixtures/events/{scenario}", s.handleEvents)
	rootMux.Handle("/", core)
	handler := server.WithServerAuth(rootMux, server.ServerAuth{Enabled: true, Token: token})
	s.httpServer = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go s.serve()
	return s, nil
}

func (s *Server) Ready() Ready { return s.ready }

func (s *Server) BaseURL() string { return s.baseURL }

func (s *Server) Done() <-chan struct{} { return s.done }

func (s *Server) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.serveErr
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.httpServer != nil {
			s.closeErr = s.httpServer.Shutdown(ctx)
		}
		if s.repo != nil {
			if err := s.repo.Close(); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
		}
		if s.tempDir != "" {
			if err := os.RemoveAll(s.tempDir); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
		}
		select {
		case <-s.done:
		case <-ctx.Done():
			if s.closeErr == nil {
				s.closeErr = ctx.Err()
			}
		}
	})
	return s.closeErr
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

func (s *Server) serve() {
	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.errMu.Lock()
	s.serveErr = err
	s.errMu.Unlock()
	close(s.done)
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	server.Success(w, r, s.catalog)
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.artifactBytes[r.PathValue("scenario")]
	if !ok {
		server.ErrorSimple(w, r, http.StatusNotFound, "preview fixture artifact was not found")
		return
	}
	writeFixtureJSON(w, raw)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.eventBytes[r.PathValue("scenario")]
	if !ok {
		server.ErrorSimple(w, r, http.StatusNotFound, "preview fixture event sequence was not found")
		return
	}
	writeFixtureJSON(w, raw)
}

func writeFixtureJSON(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Preview-Fixture-SHA256", preview.DigestBytes(raw))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func randomBearerToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := server.ValidateServerAccessToken(token); err != nil {
		return "", err
	}
	return token, nil
}
