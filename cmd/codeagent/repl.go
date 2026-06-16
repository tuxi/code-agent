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
		Compactor:   buildCompactor(mc, provider),
	}

	sess, err := session.NewBuilder(root).
		WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
		Build()
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
			quit, cerr := handleCommand(line, cfg, &mc, runner, sess)
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
		if res.PromptTokens > 0 {
			// 展示Token 预算，这样能看出离压缩还有多远
			fmt.Printf(
				"[context: %d / %d]\n",
				sess.PromptTokens,
				sess.CompactThreshold,
			)
		}
	}
}

// handleCommand processes a slash command. It may mutate the current model
// (mc), the runner, and the session budget (on /use). It returns quit=true for
// /exit and /quit.
func handleCommand(line string, cfg app.Config, mc *app.ModelConfig, runner *agent.Runner, sess *session.Session) (bool, error) {
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
		newProvider, err := buildProvider(newMC, cfg.Provider)
		if err != nil {
			return false, err
		}
		*mc = newMC
		runner.Model = newProvider
		runner.ModelName = newMC.Model
		runner.Temperature = newMC.Temperature
		runner.Compactor = buildCompactor(newMC, newProvider)
		// The budget belongs to the session, not the runner: switching to a model
		// with a different window must change WHEN this conversation compacts.
		sess.ContextWindow = newMC.ContextWindow
		sess.CompactThreshold = cfg.CompactThreshold(newMC)
		fmt.Printf("switched to %s (%s); context budget %d/%d\n",
			newMC.Name, newMC.Model, sess.CompactThreshold, sess.ContextWindow)
		return false, nil

	default:
		return false, fmt.Errorf("unknown command %q (try /help)", fields[0])
	}
}
