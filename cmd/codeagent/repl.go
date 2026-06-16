package main

import (
	"bufio"
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/ui"
	"context"
	"fmt"
	"os"
	"strings"
)

// repl runs an interactive session: one Session persists across turns, so the
// model remembers earlier context. The same stdin reader is shared with the
// approver so confirmation prompts and the input loop don't fight over stdin.
func repl(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider) error {
	root := cfg.Workspace.Root

	registry, err := buildRegistry(root)
	if err != nil {
		return err
	}

	stdin := bufio.NewReader(os.Stdin)

	runner := &agent.Runner{
		Model:       provider,
		ModelName:   mc.Model,
		Temperature: mc.Temperature,
		Tools:       registry,
		MaxSteps:    cfg.Agent.MaxSteps,
		Approver:    ui.ConfirmApprover{Reader: stdin},
	}

	sess, err := session.NewBuilder(root).Build()
	if err != nil {
		return err
	}

	fmt.Printf("CodeAgent — model: %s (%s)\n", mc.Name, mc.Model)
	fmt.Println("Type a request, or /help for commands. /exit to quit.")

	for {
		fmt.Print("\n> ")
		line, err := stdin.ReadString('\n')
		if err != nil { // EOF (Ctrl-D) or read error
			fmt.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			quit, cerr := handleCommand(line, cfg, &mc, runner)
			if cerr != nil {
				fmt.Println("error:", cerr)
			}
			if quit {
				return nil
			}
			continue
		}

		res, err := runner.RunTurn(ctx, sess, line)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\n" + res.Final)
	}
}

// handleCommand processes a slash command. It may mutate the current model
// (mc) and the runner (on /use). It returns quit=true for /exit and /quit.
func handleCommand(line string, cfg app.Config, mc *app.ModelConfig, runner *agent.Runner) (bool, error) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		return true, nil

	case "/help":
		fmt.Println(`Commands:
  /model        show the current model
  /models       list configured models
  /use NAME     switch to another configured model (keeps the conversation)
  /exit, /quit  leave the REPL`)
		return false, nil

	case "/model":
		fmt.Printf("current model: %s (%s)\n", mc.Name, mc.Model)
		return false, nil

	case "/models":
		for _, name := range cfg.ModelNames() {
			marker := "  "
			if name == mc.Name {
				marker = "* "
			}
			fmt.Printf("%s%s\n", marker, name)
		}
		return false, nil

	case "/use":
		if len(fields) < 2 {
			return false, fmt.Errorf("usage: /use NAME")
		}
		newMC, err := cfg.SelectModel(fields[1])
		if err != nil {
			return false, err
		}
		newProvider, err := buildProvider(newMC)
		if err != nil {
			return false, err
		}
		*mc = newMC
		runner.Model = newProvider
		runner.ModelName = newMC.Model
		runner.Temperature = newMC.Temperature
		fmt.Printf("switched to %s (%s)\n", newMC.Name, newMC.Model)
		return false, nil

	default:
		return false, fmt.Errorf("unknown command %q (try /help)", fields[0])
	}
}
