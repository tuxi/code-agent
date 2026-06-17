package main

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/git"
	"code-agent/internal/tools/search"
	"code-agent/internal/tools/shell"
	"code-agent/internal/ui"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	modelName, args := extractModelFlag(args)

	cfg, err := app.LoadConfig("config.yaml")
	if err != nil {
		return err
	}

	ctx := context.Background()

	// These only read the store — no model, no API key required.
	if len(args) > 0 {
		switch args[0] {
		case "sessions":
			return listSessions(ctx, cfg)
		case "stats":
			return runStats(ctx, cfg)
		}
	}

	mc, err := cfg.SelectModel(modelName)
	if err != nil {
		return err
	}

	provider, err := buildProvider(mc, cfg.Provider)
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return repl(ctx, cfg, mc, provider, "")
	}

	command := args[0]
	goal := strings.Join(args[1:], " ")

	switch command {
	case "ask":
		return runAsk(ctx, mc, provider, goal)
	case "run":
		return runAgent(ctx, cfg, mc, provider, goal)
	case "resume":
		if len(args) < 2 {
			return fmt.Errorf("usage: codeagent resume <session-id>  (see 'codeagent sessions')")
		}
		return repl(ctx, cfg, mc, provider, args[1])
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

// openStore opens (creating if needed) the per-project session database under
// <root>/.codeagent/sessions.db. Sessions are project-local: you resume the
// conversation for the repo you are in.
func openStore(root string) (session.Store, error) {
	path := filepath.Join(root, ".codeagent", "sessions.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create session store dir: %w", err)
	}
	return session.NewSQLiteStore(path)
}

// listSessions prints saved sessions, most recently updated first.
func listSessions(ctx context.Context, cfg app.Config) error {
	store, err := openStore(cfg.Workspace.Root)
	if err != nil {
		return err
	}
	defer store.Close()

	metas, err := store.List(ctx)
	if err != nil {
		return err
	}
	printSessionMetas(metas)
	return nil
}

// printSessionMetas renders a session listing, newest first. Sessions that have
// compacted also show how many times and how many tokens were reclaimed.
func printSessionMetas(metas []session.Meta) {
	if len(metas) == 0 {
		fmt.Println("no saved sessions")
		return
	}
	for _, m := range metas {
		line := fmt.Sprintf("%s  model=%s  msgs=%d  ctx=%d", m.ID, m.Model, m.MessageCount, m.PromptTokens)
		if m.Compactions > 0 {
			line += fmt.Sprintf("  compactions=%d  saved=%d", m.Compactions, m.TotalSaved)
		}
		line += "  updated=" + m.UpdatedAt.Local().Format("2006-01-02 15:04")
		fmt.Println(line)
	}
}

// runStats prints aggregate compaction telemetry across all saved sessions.
func runStats(ctx context.Context, cfg app.Config) error {
	store, err := openStore(cfg.Workspace.Root)
	if err != nil {
		return err
	}
	defer store.Close()

	st, err := store.Stats(ctx)
	if err != nil {
		return err
	}
	printStats(st)
	return nil
}

// printStats renders aggregate telemetry. This is the evidence base for sizing a
// token-aware recent window (compression varies a lot with workload), so the
// numbers matter more than the formatting.
func printStats(st session.Stats) {
	fmt.Printf("Sessions:           %d\n", st.Sessions)
	fmt.Printf("Compactions:        %d\n", st.Compactions)
	if st.Compactions == 0 {
		fmt.Println("(no compactions recorded yet — run some longer sessions first)")
		return
	}
	fmt.Printf("Avg before tokens:  %.0f\n", st.AvgBefore)
	fmt.Printf("Avg after tokens:   %.0f\n", st.AvgAfter)
	fmt.Printf("Avg saved tokens:   %.0f\n", st.AvgSaved)
	fmt.Printf("Avg ratio:          %.1f%%\n", st.AvgRatio*100)
	fmt.Printf("Avg summary chars:  %.0f\n", st.AvgSummaryChars)
	fmt.Printf("Max ratio:          %.1f%%\n", st.MaxRatio*100)
	fmt.Printf("Min ratio:          %.1f%%\n", st.MinRatio*100)
}

// buildProvider constructs a model.Provider from a resolved model config. Only
// OpenAI-compatible endpoints are wired today; this is the extension point for
// Anthropic, Gemini, Ollama, etc.
//
// Every provider is wrapped in a ResilientProvider so a transient API error
// (timeout, 429, 5xx) does not kill the run: timeout and retry policy live in
// this one transport layer, not in each provider.
func buildProvider(mc app.ModelConfig, pc app.ProviderConfig) (model.Provider, error) {
	var inner model.Provider
	switch mc.Provider {
	case "openai", "":
		inner = model.NewOpenAICompatibleProvider(mc.BaseURL, mc.APIKey)
	default:
		return nil, fmt.Errorf("unsupported provider %q (only \"openai\"-compatible is wired so far)", mc.Provider)
	}
	return &model.ResilientProvider{
		Inner:      inner,
		MaxRetries: pc.MaxRetries,
		Timeout:    time.Duration(pc.RequestTimeoutSeconds) * time.Second,
		Backoff:    time.Duration(pc.BackoffMillis) * time.Millisecond,
		MaxBackoff: time.Duration(pc.MaxBackoffSeconds) * time.Second,
	}, nil
}

// buildCompactor builds the summary compactor used to keep long sessions inside
// the context window. It summarizes with the same provider/model the agent is
// running, so switching models (`/use`) must rebuild it. Shared by run and repl.
func buildCompactor(mc app.ModelConfig, provider model.Provider) session.Compactor {
	return &session.LLMCompactor{
		Provider:           provider,
		ModelName:          mc.Model,
		Temperature:        mc.Temperature,
		KeepRecentMessages: 50,
	}
}

// buildRegistry registers the model-facing tool set. Shared by run and repl.
func buildRegistry(root string) (*tools.Registry, error) {
	registry := tools.NewRegistry()
	for _, tool := range []tools.Tool{
		filesystem.NewListFilesTool(root),
		filesystem.NewReadFileTool(root),
		filesystem.NewEditFileTool(root),
		search.NewGrepTool(root),
		git.NewDiffTool(root),
		git.NewApplyPatchTool(root),
		shell.NewRunCommandTool(root),
	} {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// extractModelFlag pulls a --model NAME (or --model=NAME) out of args from any
// position, returning the chosen name and the remaining args.
func extractModelFlag(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--model" || args[i] == "-model":
			if i+1 < len(args) {
				rest := append(append([]string{}, args[:i]...), args[i+2:]...)
				return args[i+1], rest
			}
		case strings.HasPrefix(args[i], "--model="):
			name := strings.TrimPrefix(args[i], "--model=")
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return name, rest
		}
	}
	return "", args
}

func runAsk(ctx context.Context, mc app.ModelConfig, provider model.Provider, question string) error {
	resp, err := provider.Complete(ctx, model.Request{
		Model:       mc.Model,
		Temperature: mc.Temperature,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a helpful coding assistant."},
			{Role: model.RoleUser, Content: question},
		},
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Content)
	return nil
}

func runAgent(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, goal string) error {
	root := cfg.Workspace.Root

	registry, err := buildRegistry(root)
	if err != nil {
		return err
	}

	runner := &agent.Runner{
		Model:       provider,
		ModelName:   mc.Model,
		Temperature: mc.Temperature,
		Tools:       registry,
		MaxSteps:    cfg.Agent.MaxSteps,
		Approver:    ui.ConfirmApprover{},
		Compactor:   buildCompactor(mc, provider),
	}

	sess, err := session.NewBuilder(root).
		WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
		Build()
	if err != nil {
		return err
	}
	sess.Model = mc.Model

	store, err := openStore(root)
	if err != nil {
		return err
	}
	defer store.Close()

	fmt.Printf("Model: %s (%s)\nSession: %s\n", mc.Name, mc.Model, sess.ID)

	res, runErr := runner.RunTurn(ctx, sess, goal)
	// Persist whatever the turn produced, even on error: the partial history is
	// consistent and resumable.
	if err := store.Save(ctx, sess); err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to save session:", err)
	}
	if runErr != nil {
		return runErr
	}

	fmt.Println("\nFinal:")
	fmt.Println(res.Final)
	fmt.Printf("\n(resume with: codeagent resume %s)\n", sess.ID)
	return nil
}

func printUsage() {
	fmt.Println(`Usage:
  codeagent [--model NAME]                 start the interactive REPL (new session)
  codeagent [--model NAME] run "..."       run a single task
  codeagent [--model NAME] ask "..."       one-off question (no tools)
  codeagent sessions                       list saved sessions
  codeagent stats                          aggregate compaction telemetry
  codeagent [--model NAME] resume <id>     resume a saved session in the REPL

Sessions are stored per-project in .codeagent/sessions.db and persist across
runs, so a long conversation (and its summary) survives exit.

Models are defined in config.yaml under "models:"; --model selects one
(default: the configured default_model).`)
}
