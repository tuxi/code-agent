package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
)

// defaultCapabilities is the capability set advertised in the WebSocket hello
// handshake. It is a static contract, not derived from runtime state — clients
// use it to decide which protocol features to enable.
var defaultCapabilities = []string{
	"streaming",
	"thinking",
	"tool_streaming",
	"plan_mode",
	"subagents",
	"session_resume",
	"client_tool_execution",
}

// runServe starts the runtime server. One global tool registry is built at startup
// (tools are stateless — workspace comes from ExecutionContext at call time, not
// from struct fields). The WorkspaceRegistry caches per-workspace stores and skills.
func runServe(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, addr string) error {
	root := cfg.Workspace.Root

	// Open the default workspace's store for global telemetry (API request recording).
	telemetryStore, err := runtime.OpenStore(root)
	if err != nil {
		return err
	}
	defer telemetryStore.Close()
	runtime.AttachObserver(provider, telemetryStore, ctx)

	// Build the global tool registry once. Tools are stateless — each Execute call
	// receives its workspace via ExecutionContext, so the same tool instances serve
	// every conversation regardless of workspace.
	toolReg, _, mcpMgr, planRef, err := runtime.BuildRegistry(ctx, cfg, mc, provider, telemetryStore, nil)
	if err != nil {
		return err
	}
	defer mcpMgr.Close()

	wsReg := runtime.NewWorkspaceRegistry(root)
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

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("codeagent serve — http://%s  (default workspace: %s, model: %s)\n", addr, root, mc.Model)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST   /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  PATCH  /v1/conversations/{id}        {\"name\":\"...\"}")
	fmt.Println("  DELETE /v1/conversations/{id}")
	fmt.Println("  GET    /v1/conversations/{id}/stream   (WebSocket)")
	fmt.Println("  GET  /v1/conversations/{id}/messages")
	fmt.Println("  GET  /v1/conversations/{id}/events")
	fmt.Println("  GET  /v2/conversations/{id}/stream   (WebSocket, same as v1)")

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "codeagent serve: stopped")
	return nil
}
