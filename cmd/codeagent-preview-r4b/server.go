package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/buildinfo"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/preview"
	agentruntime "code-agent/internal/runtime"
	"code-agent/internal/server"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
	previewtool "code-agent/internal/tools/preview"
)

const DefaultTurnPrompt = "R4B_TRIGGER_PREVIEW_v1: use ui_preview to render the Backend R4b live readiness preview."

type ServerOptions struct {
	Workspace  string
	StateDir   string
	SessionID  string
	TurnPrompt string
}

type Readiness struct {
	BaseURL     string `json:"base_url"`
	BearerToken string `json:"bearer_token"`
	SessionID   string `json:"session_id"`
	TurnPrompt  string `json:"turn_prompt"`
}

type Server struct {
	readiness Readiness
	listener  net.Listener
	http      *http.Server
	executor  *conversation.TurnExecutor
	store     session.Store
	oldState  string

	done      chan error
	closeOnce sync.Once
}

type previewRunBuilder struct {
	workspace string
	cfg       settings.Settings
	mc        settings.ModelConfig
	provider  model.Provider
	service   *preview.Service
}

func (b *previewRunBuilder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	reg := tools.NewRegistry()
	_ = reg.Register(previewtool.NewTool())
	runner := agentruntime.BuildRunner(b.cfg, b.mc, b.provider, reg, nil, alwaysAllowApprover{}, rc.Publisher, nil, b.workspace)
	runner.PreviewProducer = b.service
	runner.ReservedTurnID = rc.TurnID
	runner.RequestID = rc.RequestID
	runner.WorkspaceRoot = b.workspace
	return runner
}

type alwaysAllowApprover struct{}

func (alwaysAllowApprover) Approve(string, json.RawMessage) agent.Verdict { return agent.VerdictAllow }

func Start(ctx context.Context, opts ServerOptions) (*Server, Readiness, error) {
	if strings.TrimSpace(opts.TurnPrompt) == "" {
		opts.TurnPrompt = DefaultTurnPrompt
	}
	workspace, err := ensureDir(opts.Workspace, "codeagent-r4b-workspace-")
	if err != nil {
		return nil, Readiness{}, err
	}
	stateDir, err := ensureDir(opts.StateDir, "codeagent-r4b-state-")
	if err != nil {
		return nil, Readiness{}, err
	}
	if err := writeFallbackPNG(filepath.Join(workspace, "assets", "fallback.png")); err != nil {
		return nil, Readiness{}, fmt.Errorf("write R4b fallback: %w", err)
	}
	oldState := agentruntime.StoreBaseDir()
	agentruntime.SetStoreBaseDir(stateDir)
	store, err := agentruntime.OpenStore(workspace)
	if err != nil {
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, err
	}
	service, err := agentruntime.OpenPreviewService(workspace)
	if err != nil {
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, err
	}
	sessionID := strings.TrimSpace(opts.SessionID)
	if sessionID == "" {
		suffix, err := randomHex(8)
		if err != nil {
			_ = store.Close()
			agentruntime.SetStoreBaseDir(oldState)
			return nil, Readiness{}, err
		}
		sessionID = "session_r4b_" + suffix
	}
	mc := settings.ModelConfig{Name: "r4b-scripted", Provider: "responses", Model: "r4b-scripted", ContextWindow: 128000}
	cfg := settings.Settings{Agent: settings.AgentConfig{MaxSteps: 4, MaxParallelTools: 1}}
	repo := conversation.NewSQLiteRepository(store, mc.ContextWindow, cfg.CompactThreshold(mc), mc.Model, "", func(string) string { return "" })
	type idCreator interface {
		CreateWithID(context.Context, string, string, string) (*session.Session, error)
	}
	var sess *session.Session
	if creator, ok := repo.(idCreator); ok {
		sess, err = creator.CreateWithID(ctx, sessionID, workspace, "")
	} else {
		sess, err = repo.Create(ctx, workspace, "")
	}
	if err != nil {
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, err
	}
	if sess.ID != sessionID {
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, fmt.Errorf("session ID creation mismatch: got %q want %q", sess.ID, sessionID)
	}
	sess.Model = mc.Model
	sess.ContextWindow = mc.ContextWindow
	sess.CompactThreshold = cfg.CompactThreshold(mc)
	eventStore := &conversation.StoreEventAdapter{Store: store}
	preTrigger, err := eventStore.Replay(ctx, sessionID)
	if err != nil {
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, err
	}
	if len(preTrigger) != 0 {
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, fmt.Errorf("pre-created session already has %d events", len(preTrigger))
	}
	active := conversation.NewActiveTurnRegistry()
	subs := conversation.NewSubscriptionManager()
	builder := &previewRunBuilder{
		workspace: workspace, cfg: cfg, mc: mc,
		provider: &previewTriggerProvider{prompt: opts.TurnPrompt}, service: service,
	}
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, builder)
	executor.SetTurnScheduler(conversation.NewTurnScheduler(1))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		executor.Shutdown()
		_ = store.Close()
		agentruntime.SetStoreBaseDir(oldState)
		return nil, Readiness{}, err
	}
	baseURL := "http://" + listener.Addr().String()
	models := server.BuildRuntimeModelCatalog(settings.Settings{}, nil)
	models.DefaultModel = "r4b-scripted"
	models.DefaultRuntimeAlias = "r4b-scripted"
	runtimeInfo := server.RuntimeInfo{
		Schema: "runtime-info/v1", ServerID: "runtime_r4b_" + strings.TrimPrefix(sessionID, "session_r4b_"),
		DisplayName: "CodeAgent R4b Live Preview Runtime", Product: buildinfo.Product,
		RuntimeVersion:    buildinfo.Version,
		AgentWireProtocol: server.AgentWireProtocolInfo{Major: buildinfo.AgentWireMajor, Revision: buildinfo.AgentWireRevision},
		RuntimeProfile:    server.RuntimeProfileHeadless,
	}
	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{
		ServerName:    buildinfo.ServerName(),
		RuntimeInfo:   runtimeInfo,
		RuntimeModels: models,
		ServerAuth:    server.ServerAuth{Enabled: true, Token: token},
		Capabilities: []string{
			"streaming", "thinking", "reasoning_streaming", "tool_streaming",
			"session_resume", "client_tool_execution",
		},
		RuntimeCapabilities: server.ConfiguredRuntimeCapabilities(1),
		Preview:             service,
	})
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	readiness := Readiness{BaseURL: baseURL, BearerToken: token, SessionID: sessionID, TurnPrompt: opts.TurnPrompt}
	srv := &Server{
		readiness: readiness, listener: listener, http: httpServer, executor: executor,
		store: store, oldState: oldState, done: make(chan error, 1),
	}
	go func() {
		err := httpServer.Serve(listener)
		if err == http.ErrServerClosed {
			err = nil
		}
		srv.done <- err
		close(srv.done)
	}()
	return srv, readiness, nil
}

func (s *Server) Ready() Readiness { return s.readiness }
func (s *Server) BaseURL() string  { return s.readiness.BaseURL }
func (s *Server) Done() <-chan error {
	return s.done
}

func (s *Server) Shutdown(ctx context.Context) error {
	var shutdownErr error
	s.closeOnce.Do(func() {
		shutdownErr = s.http.Shutdown(ctx)
		s.executor.Shutdown()
		if err := s.store.Close(); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
		agentruntime.SetStoreBaseDir(s.oldState)
	})
	return shutdownErr
}

func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func ensureDir(value, pattern string) (string, error) {
	if strings.TrimSpace(value) != "" {
		abs, err := filepath.Abs(value)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", err
		}
		return abs, nil
	}
	return os.MkdirTemp("", pattern)
}

func randomHex(bytesCount int) (string, error) {
	buf := make([]byte, bytesCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeFallbackPNG(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(32 + x*180/1200),
				G: uint8(48 + y*160/800),
				B: uint8(96 + (x+y)*80/2000),
				A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
