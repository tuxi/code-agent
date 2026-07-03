// Command codeagentd is the daemon-mode entry point for the CodeAgent runtime
// server. It starts an HTTP + WebSocket server with no terminal/TUI dependencies,
// designed to be launched from an IDE (GoLand), systemd, launchd, or Docker.
//
// Usage:
//
//	codeagentd [--model NAME] [addr]   default addr: 127.0.0.1:8787
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"

	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/mcp"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
)

// defaultCapabilities is the capability set advertised in the WebSocket hello
// handshake. Keep in sync with cmd/codeagent/serve.go.
var defaultCapabilities = []string{
	"streaming",
	"thinking",
	"tool_streaming",
	"plan_mode",
	"subagents",
	"session_resume",
	"client_tool_execution",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	modelName, args := runtime.ExtractModelFlag(args)

	addr := "127.0.0.1:8787"
	if len(args) > 0 {
		addr = args[0]
	}

	cfg, err := app.LoadConfig("config.yaml")
	if err != nil {
		return err
	}
	// MCP servers come from Claude-compatible `.mcp.json` files, layered by scope:
	// project (<root>/.mcp.json) over user (~/.codeagent/mcp.json). Set
	// CODEAGENT_MCP_INHERIT_CLAUDE=1 to also inherit user-scope servers from an
	// existing ~/.claude.json. Missing files => no MCP.
	inheritClaude := os.Getenv("CODEAGENT_MCP_INHERIT_CLAUDE") == "1"
	if cfg.MCP, err = mcp.ResolveDesktop(cfg.Workspace.Root, inheritClaude); err != nil {
		return err
	}
	if cfg.GlobalSkillsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.GlobalSkillsDir = filepath.Join(home, ".codeagent", "skills")
		}
	}
	if cfg.StoreFactory != nil {
		runtime.StoreFactory = cfg.StoreFactory
	}

	mc, err := cfg.SelectModel(modelName)
	if err != nil {
		return err
	}

	provider, err := runtime.BuildProvider(mc, cfg.Provider)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	root := cfg.Workspace.Root

	// Open the default workspace's store for global telemetry.
	telemetryStore, err := runtime.OpenStore(root)
	if err != nil {
		return err
	}
	defer telemetryStore.Close()
	runtime.AttachObserver(provider, telemetryStore, ctx)

	// Build the global tool registry once.
	toolReg, _, mcpMgr, planRef, jobSink, err := runtime.BuildRegistry(ctx, cfg, mc, provider, telemetryStore, nil)
	if err != nil {
		return err
	}
	defer mcpMgr.Close()

	wsReg := runtime.NewWorkspaceRegistry(root, cfg.GlobalSkillsDir)
	defer wsReg.Close()

	// Execution Model components.
	repo := conversation.NewSQLiteRepository(
		telemetryStore,
		mc.ContextWindow,
		cfg.CompactThreshold(mc),
		mc.Model,
		"", // desktop: absolute workspace paths are stable; no re-anchoring
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
		ServerName:    "codeagentd/" + mc.Model,
		Capabilities:  defaultCapabilities,
		WorkspaceRoot: root,
		Granter:       rb.Rules(),
		Prompts:       mcpMgr,
	})

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("codeagentd serve — http://%s  (default workspace: %s, model: %s)\n", addr, root, mc.Model)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST  /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  PATCH /v1/conversations/{id}        {\"name\":\"...\"}")
	fmt.Println("  GET   /v1/conversations/{id}/stream   (WebSocket)")

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	fmt.Fprintln(os.Stderr, "codeagentd: stopped")
	return nil
}
