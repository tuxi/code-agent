package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"code-agent/internal/app"
	"code-agent/internal/embed"
	"code-agent/internal/mcp"
	"code-agent/internal/model"
	"code-agent/internal/settings"
	"code-agent/internal/server"
)

// runServe starts the runtime server. The execution-model assembly (one global
// tool registry, workspace registry, conversation repository, turn executor) is
// shared with the embedded host via embed.Assemble, so the CLI and the in-app
// runtime expose identical behavior. Tools are stateless — each Execute call
// receives its workspace via ExecutionContext — so the same tool instances serve
// every conversation regardless of workspace.
func runServe(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, addr string) error {
	root, _ := os.Getwd()
	auth, err := server.ResolveExternalServerAuth(cfg.Server)
	if err != nil {
		return err
	}
	if err := server.ValidateExternalDeployment(addr, cfg.Server, auth); err != nil {
		return err
	}

	// Serve mode resolves MCP per conversation workspace (WorkspaceRegistry.
	// EnableMCP inside Assemble). main() pre-resolved cfg.MCP from the process
	// CWD for the single-workspace commands (run/repl/tui); passing that on would
	// inject the CWD's servers into EVERY workspace, so clear it — a conversation
	// whose workspace IS the CWD picks the same .mcp.json up again via the
	// workspace-scoped path.
	cfg.MCP = mcp.Config{}

	// The CLI serve path uses process lifecycle, not the in-app suspend/resume
	// verbs, so it ignores the returned Runtime bundle.
	home, _ := os.UserHomeDir()
	handler, _, closers, err := embed.Assemble(
		ctx, cfg, settings.Load(root, home, os.Stderr), mc, provider, cfg.CredentialResolver(nil), root, "",
		embed.RuntimeServerOptions{
			Profile: server.RuntimeProfileHeadless, DisplayName: cfg.Server.DisplayName, Auth: auth,
		},
	)
	if err != nil {
		return err
	}
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}()

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	scheme := "http"
	if cfg.Server.TLSCertificate != "" {
		scheme = "https"
	}
	fmt.Printf("codeagent serve — %s://%s  (default workspace: %s, model: %s)\n", scheme, addr, root, mc.Model)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST   /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  PATCH  /v1/conversations/{id}        {\"name\":\"...\"}")
	fmt.Println("  DELETE /v1/conversations/{id}")
	fmt.Println("  GET    /v1/conversations/{id}/stream   (WebSocket)")
	fmt.Println("  GET  /v1/conversations/{id}/messages")
	fmt.Println("  GET  /v1/conversations/{id}/events")
	fmt.Println("  GET  /v2/conversations/{id}/stream   (WebSocket, same as v1)")

	serve := srv.ListenAndServe
	if cfg.Server.TLSCertificate != "" {
		serve = func() error {
			return srv.ListenAndServeTLS(cfg.Server.TLSCertificate, cfg.Server.TLSPrivateKey)
		}
	}
	if err := serve(); err != nil && err != http.ErrServerClosed {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "codeagent serve: stopped")
	return nil
}
