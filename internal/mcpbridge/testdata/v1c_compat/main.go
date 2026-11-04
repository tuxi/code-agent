// This fixture compiles unchanged against the pre-V1-c baseline and candidate.
// It assembles production transport/storage with a deterministic, tool-free
// Runner. It never constructs a Provider or reads user configuration/credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/server"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
)

type builder struct{ runs atomic.Int32 }

func (b *builder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "offline", Model: "offline"}, nil
}
func (b *builder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	return &runner{b, rc}
}

type runner struct {
	b  *builder
	rc conversation.RuntimeContext
}

func (r *runner) RunTurn(ctx context.Context, s *session.Session, _ string) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *runner) ResumeTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *runner) RunPreparedTurn(_ context.Context, s *session.Session) (agent.TurnResult, error) {
	r.b.runs.Add(1)
	r.rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: s.ID, TurnID: r.rc.TurnID, Text: "offline compatibility fixture"})
	return agent.TurnResult{TurnID: r.rc.TurnID}, nil
}

func main() {
	db := flag.String("db", "", "temporary SQLite")
	ready := flag.String("ready", "", "temporary endpoint record")
	tokenPath := flag.String("token-file", "", "temporary fixture credential")
	flag.Parse()
	if err := serve(*db, *ready, *tokenPath); err != nil {
		os.Exit(2)
	}
}

func serve(dbPath, readyPath, tokenPath string) error {
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}
	store, err := sqlite.New(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	repo := conversation.NewSQLiteRepository(store, 32000, 24000, "offline", "", nil)
	events := &conversation.StoreEventAdapter{Store: store}
	b := &builder{}
	ex := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), b)
	defer ex.Shutdown()
	h := server.NewMux(repo, events, ex, server.MuxOptions{RuntimeInfo: server.RuntimeInfo{ServerID: "v1c-offline-fixture"}})
	// This count is test evidence only, protected by the same fixture Bearer.
	wrapped := server.WithServerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/compat/count" && r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(map[string]int32{"runs": b.runs.Load()})
			return
		}
		h.ServeHTTP(w, r)
	}), server.ServerAuth{Enabled: true, Token: string(token)})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: wrapped, ReadHeaderTimeout: 5 * time.Second}
	defer srv.Close()
	data, _ := json.Marshal(map[string]string{"endpoint": "http://" + listener.Addr().String()})
	if err := os.WriteFile(readyPath, data, 0600); err != nil {
		listener.Close()
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		return srv.Shutdown(shutdownCtx)
	case err := <-done:
		return err
	}
}
