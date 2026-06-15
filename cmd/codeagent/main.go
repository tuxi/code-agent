package main

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/search"
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
	if len(os.Args) < 3 {
		printUsage()
		return nil
	}

	command := os.Args[1]
	goal := strings.Join(os.Args[2:], " ")

	cfg, err := app.LoadConfig("config.yaml")
	if err != nil {
		return err
	}

	provider := model.NewOpenAICompatibleProvider(cfg.Model.BaseURL, cfg.Model.APIKey)

	switch command {
	case "ask":
		return runAsk(context.Background(), cfg, provider, goal)
	case "run":
		return runAgent(context.Background(), cfg, provider, goal)
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

func runAsk(ctx context.Context, cfg app.Config, provider model.Provider, question string) error {
	resp, err := provider.Complete(ctx, model.Request{
		Model:       cfg.Model.Model,
		Temperature: cfg.Model.Temperature,
		Messages: []model.Message{
			model.Message{
				Role:    model.RoleSystem,
				Content: "You are a helpful coding assistant.",
			},
			model.Message{
				Role:    model.RoleUser,
				Content: question,
			},
		},
	})

	if err != nil {
		return err
	}

	fmt.Println(resp.Content)
	return nil
}

func runAgent(ctx context.Context, cfg app.Config, provider model.Provider, goal string) error {
	registry := tools.NewRegistry()

	if err := registry.Register(filesystem.NewListFilesTool(cfg.Workspace.Root)); err != nil {
		return err
	}

	if err := registry.Register(filesystem.NewReadFileTool(cfg.Workspace.Root)); err != nil {
		return err
	}

	if err := registry.Register(search.NewGrepTool(cfg.Workspace.Root)); err != nil {
		return err
	}

	runner := &agent.Runner{
		Model:       provider,
		ModelName:   cfg.Model.Model,
		Temperature: cfg.Model.Temperature,
		Tools:       registry,
		MaxSteps:    cfg.Agent.MaxSteps,
	}

	result, err := runner.Run(ctx, goal)
	if err != nil {
		return err
	}

	fmt.Println("\nFinal:")
	fmt.Println(result.Final)
	return nil
}

func printUsage() {
	fmt.Println(`Usage:
  codeagent ask "hello"
  codeagent run "解释这个项目结构"

Environment:
  DEEPSEEK_API_KEY=your_api_key`)

}
