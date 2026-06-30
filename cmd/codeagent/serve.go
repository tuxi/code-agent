package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"code-agent/internal/app"
	"code-agent/internal/embed"
	"code-agent/internal/model"
)

// runServe starts the runtime server. The execution-model assembly (one global
// tool registry, workspace registry, conversation repository, turn executor) is
// shared with the embedded host via embed.Assemble, so the CLI and the in-app
// runtime expose identical behavior. Tools are stateless — each Execute call
// receives its workspace via ExecutionContext — so the same tool instances serve
// every conversation regardless of workspace.
func runServe(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, addr string) error {
	root := cfg.Workspace.Root

	handler, closers, err := embed.Assemble(ctx, cfg, mc, provider)
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
