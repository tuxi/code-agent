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
	"code-agent/internal/session"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// serveFactory builds conversations for the runtime server. Tools and skills are
// built once and shared across conversations (one workspace for now — per-
// conversation workspace is P1-B). Each conversation gets its own runner and a
// fresh session. The runner starts with a nil approver (the loop denies every
// side-effecting tool until a WebSocket connection attaches its RemoteApprover)
// and a nil emitter (conversation.New installs the fan-out hub). No conversation
// persistence yet — the store is nil, so Resume is unsupported.
type serveFactory struct {
	cfg      app.Config
	mc       app.ModelConfig
	provider model.Provider
	registry *tools.Registry
	skillReg *skills.Registry
	store    session.Store
	root     string
	srvCtx   context.Context // long-lived server context for the event recorder
}

func (f *serveFactory) Create(context.Context) (*conversation.Conversation, error) {
	// Record every event to the store (the same EventStore the REPL/TUI use) so
	// GET /v1/conversations/{id}/events can replay history. The recorder uses the
	// long-lived server context, not the per-create request context (which is
	// canceled the moment POST /v1/conversations returns). No console downstream.
	emitter := withEventStore(nil, f.store, f.srvCtx)
	runner := buildRunner(f.cfg, f.mc, f.provider, f.registry, f.skillReg, nil, emitter)
	sess, err := session.NewBuilder(f.root).
		WithBudget(f.mc.ContextWindow, f.cfg.CompactThreshold(f.mc)).
		WithSkillsIndex(f.skillReg.PromptIndex()).
		Build()
	if err != nil {
		return nil, err
	}
	sess.Model = f.mc.Model
	return conversation.New(runner, sess, nil), nil
}

func (f *serveFactory) Resume(context.Context, string) (*conversation.Conversation, error) {
	return nil, fmt.Errorf("resume is not supported until session persistence lands (P1-B)")
}

// runServe starts the runtime server: an HTTP surface (healthz, create/list
// conversations, and the agent-wire WebSocket stream) over a conversation
// Manager. P1-A skeleton — no persistence, resume, or per-conversation workspace.
func runServe(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, addr string) error {
	root := cfg.Workspace.Root

	store, err := openStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	attachObserver(provider, store, ctx)

	registry, skillReg, mcpMgr, err := buildRegistry(ctx, cfg, mc, provider, store, nil)
	if err != nil {
		return err
	}
	defer mcpMgr.Close()

	factory := &serveFactory{cfg: cfg, mc: mc, provider: provider, registry: registry, skillReg: skillReg, store: store, root: root, srvCtx: ctx}
	mgr := conversation.NewManager(factory)
	defer mgr.Shutdown()

	handler := server.NewMux(mgr, store, server.MuxOptions{ServerName: "codeagent/" + mc.Model})

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("codeagent serve — http://%s  (workspace: %s, model: %s)\n", addr, root, mc.Model)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  GET  /v1/conversations/{id}/stream   (WebSocket, agent-wire v1)")

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "codeagent serve: stopped")
	return nil
}
