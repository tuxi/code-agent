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
	"strings"
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

	mc, err := cfg.SelectModel(modelName)
	if err != nil {
		return err
	}

	provider, err := buildProvider(mc)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if len(args) == 0 {
		return repl(ctx, cfg, mc, provider)
	}

	command := args[0]
	goal := strings.Join(args[1:], " ")

	switch command {
	case "ask":
		return runAsk(ctx, mc, provider, goal)
	case "run":
		return runAgent(ctx, cfg, mc, provider, goal)
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

// buildProvider constructs a model.Provider from a resolved model config. Only
// OpenAI-compatible endpoints are wired today; this is the extension point for
// Anthropic, Gemini, Ollama, etc.
func buildProvider(mc app.ModelConfig) (model.Provider, error) {
	switch mc.Provider {
	case "openai", "":
		return model.NewOpenAICompatibleProvider(mc.BaseURL, mc.APIKey), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (only \"openai\"-compatible is wired so far)", mc.Provider)
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
	}

	sess, err := session.NewBuilder(root).Build()
	if err != nil {
		return err
	}

	fmt.Printf("Model: %s (%s)\n", mc.Name, mc.Model)

	res, err := runner.RunTurn(ctx, sess, goal)
	if err != nil {
		return err
	}

	fmt.Println("\nFinal:")
	fmt.Println(res.Final)
	return nil
}

func printUsage() {
	fmt.Println(`Usage:
  codeagent [--model NAME]                 start the interactive REPL
  codeagent [--model NAME] run "..."       run a single task
  codeagent [--model NAME] ask "..."       one-off question (no tools)
 
Models are defined in config.yaml under "models:"; --model selects one
(default: the configured default_model).`)
}
