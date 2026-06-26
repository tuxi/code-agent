package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/server"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// serveRunBuilder is the conversation.RunBuilder for the server. It wraps
// buildRunner and uses the per-turn publisher from TurnExecutor (which fans
// out to event store + WS subscribers).
type serveRunBuilder struct {
	cfg      app.Config
	mc       app.ModelConfig
	provider model.Provider
	toolReg  *tools.Registry
	wsReg    *WorkspaceRegistry
}

func (b *serveRunBuilder) Build(ctx conversation.RuntimeContext) conversation.TurnRunner {
	// Resolve skills for the session's workspace.
	workspacePath := ctx.Session.WorkspacePath
	var skillReg *skills.Registry
	if inst, err := b.wsReg.Get(workspacePath); err == nil {
		skillReg = inst.SkillReg
	}

	runner := buildRunner(b.cfg, b.mc, b.provider, b.toolReg, skillReg, ctx.Approver, ctx.Publisher)
	if workspacePath != "" {
		runner.WorkspaceRoot = workspacePath
	}
	return runner
}

// runServe starts the runtime server. One global tool registry is built at startup
// (tools are stateless — workspace comes from ExecutionContext at call time, not
// from struct fields). The WorkspaceRegistry caches per-workspace stores and skills.
func runServe(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, addr string) error {
	root := cfg.Workspace.Root

	// Open the default workspace's store for global telemetry (API request recording).
	telemetryStore, err := openStore(root)
	if err != nil {
		return err
	}
	defer telemetryStore.Close()
	attachObserver(provider, telemetryStore, ctx)

	// Build the global tool registry once. Tools are stateless — each Execute call
	// receives its workspace via ExecutionContext, so the same tool instances serve
	// every conversation regardless of workspace.
	toolReg, _, mcpMgr, err := buildRegistry(ctx, cfg, mc, provider, telemetryStore, nil)
	if err != nil {
		return err
	}
	defer mcpMgr.Close()

	wsReg := NewWorkspaceRegistry(root)
	defer wsReg.Close()

	// ---- Execution Model components ----

	// ConversationRepository: backed by default workspace's SQLite store.
	repo := conversation.NewSQLiteRepository(
		telemetryStore,
		mc.ContextWindow,
		cfg.CompactThreshold(mc),
		mc.Model,
		func(workspacePath string) string {
			inst, err := wsReg.Get(workspacePath)
			if err != nil {
				return ""
			}
			return inst.SkillReg.PromptIndex()
		},
	)

	// ConversationEventStore: backed by default workspace's store.
	eventStore := &conversation.StoreEventAdapter{Store: telemetryStore}

	active := conversation.NewActiveTurnRegistry()
	subs := conversation.NewSubscriptionManager()
	rb := &serveRunBuilder{cfg: cfg, mc: mc, provider: provider, toolReg: toolReg, wsReg: wsReg}
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, rb)

	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{ServerName: "codeagent/" + mc.Model})

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("codeagent serve — http://%s  (default workspace: %s, model: %s)\n", addr, root, mc.Model)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  DELETE /v1/conversations/{id}")
	fmt.Println("  GET  /v1/conversations/{id}/stream   (WebSocket)")
	fmt.Println("  GET  /v1/conversations/{id}/messages")
	fmt.Println("  GET  /v1/conversations/{id}/events")
	fmt.Println("  GET  /v2/conversations/{id}/stream   (WebSocket, same as v1)")

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "codeagent serve: stopped")
	return nil
}
