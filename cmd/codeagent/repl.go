package main

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/ui"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chzyer/readline"
)

// lineReader reads one line for the given prompt. In the REPL it is backed by the
// readline instance, so the input loop and every sub-prompt (tool approval,
// /resume selection) share a single owner of stdin with full UTF-8 / CJK line
// editing.
type lineReader func(prompt string) (string, error)

// repl runs an interactive session: one Session persists across turns (so the
// model remembers earlier context) and is saved to the store after every turn
// (so it survives exit). A non-empty resumeID loads an existing session instead
// of starting fresh.
//
// Input goes through readline (raw mode), not the terminal's cooked mode: cooked
// mode mishandles wide CJK characters (a backspace erases one display column, so
// half a character lingers) and can drive some terminals into buggy wide-char
// wrap rendering.
func repl(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, resumeID string) error {
	root := cfg.Workspace.Root

	registry, err := buildRegistry(root)
	if err != nil {
		return err
	}

	store, err := openStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	attachObserver(provider, store, ctx)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:                 "> ",
		HistoryFile:            filepath.Join(root, ".codeagent", "history"),
		HistoryLimit:           1000,
		DisableAutoSaveHistory: true, // save only real input lines, not y/N or selections
		InterruptPrompt:        "^C",
		EOFPrompt:              "exit",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	// ask reads one line under a custom prompt through the same readline instance,
	// then restores the main prompt. Used for tool approval and /resume selection
	// so there is a single owner of stdin.
	ask := func(prompt string) (string, error) {
		rl.SetPrompt(prompt)
		defer rl.SetPrompt("> ")
		return rl.Readline()
	}

	runner := &agent.Runner{
		Model:       provider,
		ModelName:   mc.Model,
		Temperature: mc.Temperature,
		Tools:       registry,
		MaxSteps:    cfg.Agent.MaxSteps,
		Approver:    ui.ConfirmApprover{Prompt: ask},
		Compactor:   buildCompactor(mc, provider),
		Emitter:     buildEmitter(),
	}

	var sess *session.Session
	if resumeID != "" {
		sess, err = loadAndRebudget(ctx, cfg, mc, store, resumeID)
		if err != nil {
			return err
		}
		fmt.Printf("Resumed session %s (%d messages)\n", sess.ID, len(sess.Messages))
	} else {
		sess, err = session.NewBuilder(root).
			WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
			Build()
		if err != nil {
			return err
		}
		sess.Model = mc.Model
		fmt.Printf("New session %s\n", sess.ID)
	}

	fmt.Printf("CodeAgent — model: %s (%s)\n", mc.Name, mc.Model)
	fmt.Println("Type a request, or /help for commands. /exit to quit.")

	for {
		line, err := rl.Readline()
		switch {
		case err == readline.ErrInterrupt: // Ctrl-C
			if line == "" {
				return nil
			}
			continue // cancel the current input
		case err == io.EOF: // Ctrl-D
			return nil
		case err != nil:
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_ = rl.SaveHistory(line) // real input lines only; sub-prompts bypass this

		if strings.HasPrefix(line, "/") {
			newSess, quit, cerr := handleCommand(line, cfg, &mc, runner, sess, store, ask)
			if cerr != nil {
				fmt.Println("error:", cerr)
			}
			if quit {
				return nil
			}
			sess = newSess // /resume may have switched the active session
			continue
		}

		res, err := runner.RunTurn(ctx, sess, line)
		// Persist after every turn, even on error: the partial history is
		// consistent (no orphaned tool_calls) and therefore resumable.
		if serr := store.Save(ctx, sess); serr != nil {
			fmt.Println("warning: failed to save session:", serr)
		}
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\n" + res.Final)
		if res.PromptTokens > 0 {
			// 展示Token 预算，这样能看出离压缩还有多远
			fmt.Printf("[context: %d / %d]\n", sess.PromptTokens, sess.CompactThreshold)
		}
	}
}

// handleCommand processes a slash command. It may mutate the current model (mc),
// the runner, and the session budget (on /use). It returns the active session —
// usually the one passed in, but /resume swaps it for a different stored session
// — plus quit=true for /exit and /quit. ask reads sub-prompts (e.g. /resume's
// selection) through the shared readline instance.
func handleCommand(line string, cfg app.Config, mc *app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store, ask lineReader) (*session.Session, bool, error) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		return sess, true, nil

	case "/help":
		fmt.Println(`Commands:
  /model        show the current model
  /models       list configured models
  /use NAME     switch to another configured model (keeps the conversation)
  /session      show the current session id
  /sessions     list saved sessions
  /stats        aggregate compaction + provider telemetry
  /trace [N]    show the last N requests, per attempt
  /resume [id]  switch to a saved session (no id => pick from a list)
  /exit, /quit  leave the REPL`)
		return sess, false, nil

	case "/session":
		fmt.Printf("current session: %s (%d messages)\n", sess.ID, len(sess.Messages))
		return sess, false, nil

	case "/sessions":
		metas, err := store.List(context.Background())
		if err != nil {
			return sess, false, err
		}
		printSessionMetas(metas)
		return sess, false, nil

	case "/stats":
		if err := printStatsReport(context.Background(), store); err != nil {
			return sess, false, err
		}
		return sess, false, nil

	case "/trace":
		limit := 20
		if len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				limit = n
			}
		}
		recs, err := store.RecentRequests(context.Background(), limit)
		if err != nil {
			return sess, false, err
		}
		printTrace(recs)
		return sess, false, nil

	case "/resume":
		next, err := resumeInteractive(cfg, mc, sess, store, ask, fields)
		if err != nil {
			return sess, false, err
		}
		return next, false, nil

	case "/model":
		fmt.Printf("current model: %s (%s)\n", mc.Name, mc.Model)
		return sess, false, nil

	case "/models":
		for _, name := range cfg.ModelNames() {
			marker := "  "
			if name == mc.Name {
				marker = "* "
			}
			fmt.Printf("%s%s\n", marker, name)
		}
		return sess, false, nil

	case "/use":
		if len(fields) < 2 {
			return sess, false, fmt.Errorf("usage: /use NAME")
		}
		newMC, err := cfg.SelectModel(fields[1])
		if err != nil {
			return sess, false, err
		}
		newProvider, err := buildProvider(newMC, cfg.Provider)
		if err != nil {
			return sess, false, err
		}
		attachObserver(newProvider, store, context.Background())
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
		return sess, false, nil

	default:
		return sess, false, fmt.Errorf("unknown command %q (try /help)", fields[0])
	}
}

// resumeInteractive switches to another saved session. With an id (`/resume id`)
// it switches directly; otherwise it lists sessions and reads a numbered choice.
// It returns the session to make active — the current one unchanged on
// cancel/no-op. A non-empty current session is saved before switching away.
func resumeInteractive(cfg app.Config, mc *app.ModelConfig, sess *session.Session, store session.Store, read lineReader, fields []string) (*session.Session, error) {
	ctx := context.Background()

	target := ""
	if len(fields) >= 2 {
		target = fields[1]
	} else {
		metas, err := store.List(ctx)
		if err != nil {
			return sess, err
		}
		if len(metas) == 0 {
			fmt.Println("no saved sessions to resume")
			return sess, nil
		}
		for i, m := range metas {
			marker := "  "
			if m.ID == sess.ID {
				marker = "* " // the session you're currently in
			}
			fmt.Printf("%s[%d] %s  model=%s  msgs=%d  updated=%s\n",
				marker, i+1, m.ID, m.Model, m.MessageCount, m.UpdatedAt.Local().Format("2006-01-02 15:04"))
		}
		choice, err := read("Select a number to resume (enter to cancel): ")
		if err != nil {
			return sess, nil // Ctrl-C / Ctrl-D cancels
		}
		choice = strings.TrimSpace(choice)
		if choice == "" {
			fmt.Println("cancelled")
			return sess, nil
		}
		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 1 || idx > len(metas) {
			return sess, fmt.Errorf("invalid selection %q", choice)
		}
		target = metas[idx-1].ID
	}

	if target == sess.ID {
		fmt.Println("already in this session")
		return sess, nil
	}
	// Persist the current session before leaving it (it may hold state since the
	// last turn, e.g. a /use re-budget) — unless it's an untouched throwaway (a
	// fresh launch with no turns), which must not pollute the session list.
	if !sess.IsEmpty() {
		if err := store.Save(ctx, sess); err != nil {
			fmt.Println("warning: failed to save current session:", err)
		}
	}
	loaded, err := loadAndRebudget(ctx, cfg, *mc, store, target)
	if err != nil {
		return sess, err
	}
	fmt.Printf("resumed session %s (%d messages)\n", loaded.ID, len(loaded.Messages))
	return loaded, nil
}

// loadAndRebudget loads a stored session and re-budgets it to the currently
// selected model — the model that will actually run owns WHEN to compact, the
// same semantics as /use.
func loadAndRebudget(ctx context.Context, cfg app.Config, mc app.ModelConfig, store session.Store, id string) (*session.Session, error) {
	sess, err := store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	sess.Model = mc.Model
	sess.ContextWindow = mc.ContextWindow
	sess.CompactThreshold = cfg.CompactThreshold(mc)
	return sess, nil
}
