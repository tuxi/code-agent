package main

import (
	"code-agent/cmd/codeagent/tui"
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/approve"
	"code-agent/internal/model"
	"code-agent/internal/runtime"
	"code-agent/internal/session"
	"code-agent/internal/ui"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if err := run(); err != nil {
		// An ExitCoder (headless `goal`) carries a specific exit code so CI can tell
		// achieved from blocked/errored/budget. Its message may be empty when the
		// outcome was already printed; only print a non-empty one.
		var coder interface{ ExitCode() int }
		if errors.As(err, &coder) {
			if msg := err.Error(); msg != "" {
				_, _ = fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(coder.ExitCode())
		}
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	modelName, args := runtime.ExtractModelFlag(args)
	autoMode, args := runtime.ExtractAutoFlag(args)

	cfg, err := app.LoadConfig("config.yaml")
	if err != nil {
		return err
	}
	// Wire the config-level factory (set by external consumers like DreamAI) into
	// the package-level injection point so all entry points and internal helpers
	// (WorkspaceRegistry, etc.) use it.
	if cfg.StoreFactory != nil {
		runtime.StoreFactory = cfg.StoreFactory
	}

	ctx := context.Background()

	// These only read the store — no model, no API key required.
	if len(args) > 0 {
		switch args[0] {
		case "sessions":
			return listSessions(ctx, cfg)
		case "stats":
			return runStats(ctx, cfg)
		case "trace":
			limit := 20
			if len(args) >= 2 {
				if n, err := strconv.Atoi(args[1]); err == nil {
					limit = n
				}
			}
			return runTrace(ctx, cfg, limit)
		case "tasks":
			return runTasks(ctx, cfg)
		case "task-trace":
			if len(args) < 2 {
				return fmt.Errorf("usage: codeagent task-trace <sub-session-id>  (see 'codeagent tasks')")
			}
			return runTaskTrace(ctx, cfg, args[1])
		}
	}

	mc, err := cfg.SelectModel(modelName)
	if err != nil {
		return err
	}

	provider, err := runtime.BuildProvider(mc, cfg.Provider)
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return runTUI(ctx, cfg, mc, provider, autoMode)
	}

	command := args[0]
	goal := strings.Join(args[1:], " ")

	switch command {
	case "ask":
		return runAsk(ctx, mc, provider, goal)
	case "run":
		return runAgent(ctx, cfg, mc, provider, goal, autoMode)
	case "goal":
		return runGoal(ctx, cfg, mc, provider, goal, autoMode)
	case "tui":
		return runTUI(ctx, cfg, mc, provider, autoMode)
	case "repl":
		return repl(ctx, cfg, mc, provider, "", autoMode)
	case "resume":
		if len(args) < 2 {
			return fmt.Errorf("usage: codeagent resume <session-id>  (see 'codeagent sessions')")
		}
		return repl(ctx, cfg, mc, provider, args[1], autoMode)
	case "serve":
		addr := "127.0.0.1:8787"
		if len(args) >= 2 {
			addr = args[1]
		}
		return runServe(ctx, cfg, mc, provider, addr)
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

// listSessions prints saved sessions, most recently updated first.
func listSessions(ctx context.Context, cfg app.Config) error {
	store, err := runtime.OpenStore(cfg.Workspace.Root)
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

// runStats prints aggregate telemetry across all saved sessions.
func runStats(ctx context.Context, cfg app.Config) error {
	store, err := runtime.OpenStore(cfg.Workspace.Root)
	if err != nil {
		return err
	}
	defer store.Close()
	return printStatsReport(ctx, store, cfg)
}

// printStatsReport renders all three telemetry sections: Context (compaction),
// Provider (transport), and Cost (token spend). Shared by `codeagent stats` and
// the REPL's /stats.
func printStatsReport(ctx context.Context, store session.Store, cfg app.Config) error {
	cstat, err := store.Stats(ctx)
	if err != nil {
		return err
	}
	pstat, err := store.ProviderStats(ctx)
	if err != nil {
		return err
	}
	usage, err := store.TokenUsageByModel(ctx)
	if err != nil {
		return err
	}
	fmt.Println("=== Context ===")
	printContextStats(cstat)
	fmt.Println("\n=== Provider ===")
	printProviderStats(pstat, cfg)
	fmt.Println("\n=== Cost ===")
	printCostReport(usage, cfg)
	return nil
}

// computeCost is the per-model spend in the configured currency. Prompt tokens
// are split: the cached portion is billed at cacheInPerM, the rest at inPerM. When
// cacheInPerM is 0 (cache price unconfigured), cached tokens fall back to the full
// input price — so an unconfigured cache price reproduces the old estimate rather
// than under-counting. cachedTokens is a portion of promptTokens, clamped so a
// quirky provider value cannot push the uncached count negative.
func computeCost(promptTokens, cachedTokens, completionTokens int64, inPerM, cacheInPerM, outPerM float64) float64 {
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cacheInPerM == 0 {
		cacheInPerM = inPerM
	}
	uncached := promptTokens - cachedTokens
	return (float64(uncached)*inPerM + float64(cachedTokens)*cacheInPerM + float64(completionTokens)*outPerM) / 1_000_000
}

// printCostReport joins per-model token usage with the configured prices. A
// model with no price set is shown "unpriced" (tokens but no money).
func printCostReport(usage []session.ModelUsage, cfg app.Config) {
	if len(usage) == 0 {
		fmt.Println("(no requests recorded yet)")
		return
	}
	// Requests store the wire model string; map it to that model's prices.
	priced := map[string]app.ModelConfig{}
	for _, mc := range cfg.Models {
		priced[mc.Model] = mc
	}
	cur := cfg.Currency
	if cur == "" {
		cur = "$"
	}

	var total float64
	var anyPriced bool
	for _, u := range usage {
		mc, ok := priced[u.Model]
		costStr := "unpriced"
		if ok && (mc.InputPricePerM > 0 || mc.OutputPricePerM > 0) {
			cost := computeCost(u.PromptTokens, u.CachedPromptTokens, u.CompletionTokens,
				mc.InputPricePerM, mc.CacheInputPricePerM, mc.OutputPricePerM)
			total += cost
			anyPriced = true
			costStr = fmt.Sprintf("%s%.4f", cur, cost)
		}
		fmt.Printf("  %-18s reqs=%-4d in=%-9d cached=%-9d out=%-9d %s\n",
			u.Model, u.Requests, u.PromptTokens, u.CachedPromptTokens, u.CompletionTokens, costStr)
	}
	if anyPriced {
		fmt.Printf("  TOTAL: %s%.4f\n", cur, total)
	} else {
		fmt.Println("  (set input_price_per_million / output_price_per_million per model to see cost)")
	}
}

// printContextStats renders compaction telemetry — the evidence base for sizing
// a token-aware recent window (compression varies a lot with workload).
func printContextStats(st session.Stats) {
	fmt.Printf("Sessions:           %d\n", st.Sessions)
	fmt.Printf("Compactions:        %d\n", st.Compactions)
	if st.Compactions == 0 {
		if st.MaxCompactThreshold > 0 {
			pct := float64(st.MaxPromptTokens) / float64(st.MaxCompactThreshold) * 100
			fmt.Printf("Peak context:       %d / %d (%.0f%%)\n", st.MaxPromptTokens, st.MaxCompactThreshold, pct)
		}
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

// printProviderStats renders transport telemetry — the answer to "why are
// requests slow / failing" that a bare "context deadline exceeded" cannot give.
func printProviderStats(st session.ProviderStats, cfg app.Config) {
	fmt.Printf("Requests:           %d\n", st.Requests)
	if st.Requests == 0 {
		fmt.Println("(no requests recorded yet)")
		return
	}
	fmt.Printf("Successes:          %d\n", st.Successes)
	fmt.Printf("Failures:           %d\n", st.Failures)
	fmt.Printf("Timeouts:           %d\n", st.Timeouts)
	fmt.Printf("Timeout:            %ds\n", cfg.Provider.RequestTimeoutSeconds)
	fmt.Printf("Retries:            %d\n", st.Retries)
	fmt.Printf("Avg latency:        %.1fs\n", st.AvgLatencyMs/1000)
	fmt.Printf("P50 latency:        %.1fs\n", float64(st.P50LatencyMs)/1000)
	fmt.Printf("P95 latency:        %.1fs\n", float64(st.P95LatencyMs)/1000)
	fmt.Printf("P99 latency:        %.1fs\n", float64(st.P99LatencyMs)/1000)
	fmt.Printf("Max latency:        %.1fs\n", float64(st.MaxLatencyMs)/1000)
	printLatencyHistogram(st.Histogram)
}

// printLatencyHistogram renders the latency distribution as proportional bars —
// the average hides the slow tail; the shape shows it.
func printLatencyHistogram(buckets []session.LatencyBucket) {
	max := 0
	for _, b := range buckets {
		if b.Count > max {
			max = b.Count
		}
	}
	if max == 0 {
		return
	}
	fmt.Println("Latency distribution:")
	const width = 24
	for _, b := range buckets {
		bar := ""
		if b.Count > 0 {
			n := b.Count * width / max
			if n == 0 {
				n = 1
			}
			bar = strings.Repeat("█", n)
		}
		fmt.Printf("  %-7s %-24s %d\n", b.Label, bar, b.Count)
	}
}

// runTrace prints the most recent requests with their per-attempt breakdown.
func runTrace(ctx context.Context, cfg app.Config, limit int) error {
	store, err := runtime.OpenStore(cfg.Workspace.Root)
	if err != nil {
		return err
	}
	defer store.Close()

	recs, err := store.RecentRequests(ctx, limit)
	if err != nil {
		return err
	}
	printTrace(recs)
	return nil
}

// printTrace renders each request as a header line plus one line per attempt —
// the detail that turns "context deadline exceeded" into "attempt 1 timed out at
// 30s, attempt 2 succeeded in 5s".
func printTrace(recs []session.RequestRecord) {
	if len(recs) == 0 {
		fmt.Println("no requests recorded yet")
		return
	}
	for _, r := range recs {
		outcome := "ok"
		if !r.Success {
			outcome = "FAILED (" + r.ErrorClass + ")"
		}
		fmt.Printf("%s  %s  prompt=%d  %d attempt(s)  %.1fs  %s\n",
			r.At.Local().Format("2006-01-02 15:04:05"), r.Model, r.PromptTokens, r.Attempts,
			float64(r.LatencyMs)/1000, outcome)
		for i, a := range r.Trace {
			fmt.Printf("    attempt %d: %.1fs %s\n", i+1, float64(a.LatencyMs)/1000, a.Result)
		}
	}
}

// runTasks lists recent subagent delegations. Each `task` call writes a
// task_started event whose session_id is the isolated sub-session that holds the
// full (otherwise invisible) investigation. Inspect one with `codeagent
// task-trace <id>`.
func runTasks(ctx context.Context, cfg app.Config) error {
	store, err := runtime.OpenStore(cfg.Workspace.Root)
	if err != nil {
		return err
	}
	defer store.Close()

	recs, err := store.RecentEventsByKind(ctx, string(agent.EventTaskStarted), 20)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Println("no subagent delegations recorded yet")
		return nil
	}
	for _, r := range recs {
		var ev agent.Event
		_ = json.Unmarshal(r.Payload, &ev)
		fmt.Printf("%s  %s\n    %s\n",
			r.At.Local().Format("2006-01-02 15:04"), r.SessionID, truncateOneLine(ev.Text, 100))
	}
	return nil
}

// runTaskTrace replays a sub-session's persisted event stream through the same
// console renderer the live run uses — so you can see exactly what the subagent
// did (its reads, searches, observations), which is invisible by design while it
// runs (default-quiet).
func runTaskTrace(ctx context.Context, cfg app.Config, sessionID string) error {
	store, err := runtime.OpenStore(cfg.Workspace.Root)
	if err != nil {
		return err
	}
	defer store.Close()

	recs, err := store.SessionEvents(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Printf("no events for session %q (is it a subagent sub-session? see 'codeagent tasks')\n", sessionID)
		return nil
	}
	em := consoleEmitter{}
	for _, r := range recs {
		var ev agent.Event
		if err := json.Unmarshal(r.Payload, &ev); err != nil {
			continue
		}
		switch ev.Kind {
		case agent.EventTaskStarted:
			fmt.Printf("── subagent %s ──\nprompt: %s\n", sessionID, ev.Text)
		case agent.EventTaskFinished:
			fmt.Printf("\n── conclusion ──\n%s\n", ev.Text)
		default:
			em.Emit(ev)
		}
	}
	return nil
}

// truncateOneLine collapses s to its first line and caps its length, for compact
// listings.
func truncateOneLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
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

func runAgent(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, goal string, autoMode bool) error {
	root := cfg.Workspace.Root

	store, err := runtime.OpenStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	runtime.AttachObserver(provider, store, ctx)

	registry, skillReg, mcpMgr, planRef, err := runtime.BuildRegistry(ctx, cfg, mc, provider, store, subagentProgress())
	if err != nil {
		return err
	}
	defer mcpMgr.Close()
	if s := mcpMgr.Summary(); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}

	// The AutoApprover wraps the human approver; --auto seeds it on, otherwise it is
	// a transparent pass-through (identical to before). Auto-grants are audited by
	// the loop (correlated EventAutoApproved), so the approver itself takes no emitter.
	approver := approve.NewAutoApprover(root, ui.ConfirmApprover{}, autoMode)
	runner := runtime.BuildRunner(cfg, mc, provider, registry, skillReg, approver, runtime.WithEventStore(buildEmitter(), store, ctx))
	planRef.R = runner

	sess, err := session.NewBuilder(root).
		WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
		WithSkillsIndex(skillReg.PromptIndex()).
		Build()
	if err != nil {
		return err
	}
	sess.Model = mc.Model

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

// runGoal is the non-interactive /goal driver (`codeagent [--auto] goal "<obj>"`):
// it pursues to a terminal state, prints progress + a one-line outcome, and exits
// with a code reflecting the result (achieved=0; blocked/errored/budget/paused
// distinct) so CI can branch on it. Same setup as runAgent; the pursuit and the
// exit-code mapping live in pursueHeadless.
func runGoal(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, objective string, autoMode bool) error {
	if strings.TrimSpace(objective) == "" {
		return fmt.Errorf(`usage: codeagent [--auto] goal "<objective>"`)
	}
	root := cfg.Workspace.Root

	store, err := runtime.OpenStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	runtime.AttachObserver(provider, store, ctx)

	registry, skillReg, mcpMgr, planRef, err := runtime.BuildRegistry(ctx, cfg, mc, provider, store, subagentProgress())
	if err != nil {
		return err
	}
	defer mcpMgr.Close()
	if s := mcpMgr.Summary(); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
	if !autoMode {
		fmt.Fprintln(os.Stderr, "note: auto mode OFF — non-interactive, so confirm-tier tools (mutating commands, edits) will be denied; pass --auto for hands-off.")
	}

	approver := approve.NewAutoApprover(root, ui.ConfirmApprover{}, autoMode)
	runner := runtime.BuildRunner(cfg, mc, provider, registry, skillReg, approver, runtime.WithEventStore(buildEmitter(), store, ctx))
	planRef.R = runner

	sess, err := session.NewBuilder(root).
		WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
		WithSkillsIndex(skillReg.PromptIndex()).
		Build()
	if err != nil {
		return err
	}
	sess.Model = mc.Model
	fmt.Fprintf(os.Stderr, "Model: %s (%s)\nSession: %s\n", mc.Name, mc.Model, sess.ID)

	return pursueHeadless(ctx, cfg, mc, runner, store, sess, objective)
}

// runTUI launches the Phase 7 BubbleTea workspace (M1). It builds the same runner
// as `run`/`repl` (buildRunner) but with channel-backed Emitter/Approver, so the
// loop runs on a background goroutine while the program owns the terminal. The
// agent is unchanged; only the renderer differs.
func runTUI(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, autoMode bool) error {
	root := cfg.Workspace.Root

	store, err := runtime.OpenStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	runtime.AttachObserver(provider, store, ctx)

	// Created up front so the subagent can route its condensed heartbeat through the
	// TUI's emitter — the model distinguishes sub-session events by SessionID and
	// renders them as a status line, never the transcript.
	backend := tui.NewBackend()

	registry, skillReg, mcpMgr, planRef, err := runtime.BuildRegistry(ctx, cfg, mc, provider, store, backend.Emitter)
	if err != nil {
		return err
	}
	defer mcpMgr.Close()
	// Printed before the BubbleTea program takes the screen, so the summary lands
	// on the normal terminal rather than corrupting the alt-screen.
	if s := mcpMgr.Summary(); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}

	sess, err := session.NewBuilder(root).
		WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
		WithSkillsIndex(skillReg.PromptIndex()).
		Build()
	if err != nil {
		return err
	}
	sess.Model = mc.Model

	// Wrap the card-backed approver with the AutoApprover so the TUI gets the same
	// hands-off auto mode as repl/run: --auto seeds it on; /auto flips it per session.
	approver := approve.NewAutoApprover(root, backend.Approver, autoMode)
	runner := runtime.BuildRunner(cfg, mc, provider, registry, skillReg, approver, runtime.WithEventStore(backend.Emitter, store, ctx))
	planRef.R = runner
	runner.Stream = true // 8.6: stream the model's text live (TUI only)
	runner.PlanApprover = backend.PlanApprover
	if autoMode {
		fmt.Fprintln(os.Stderr, "auto mode: ON (in-workspace edits auto-approved; commands still confirmed) — /auto off to disable")
	}
	header := tui.HeaderInfo{
		Model:            mc.Name,
		Workspace:        filepath.Base(root),
		Session:          sess.ID,
		CompactThreshold: cfg.CompactThreshold(mc),
		SubagentBudget:   runtime.SubAgentMaxSteps,
	}
	// /resume loads a stored session and re-budgets it to the current model — the
	// same helper the REPL's /resume uses.
	resume := func(id string) (*session.Session, error) {
		return loadAndRebudget(ctx, cfg, mc, store, id)
	}
	// /use switches the runner to a new model between turns — the same logic as
	// the REPL's /use, but inside the run-loop goroutine via modelSwap.
	modelSwap := func(name string) (tui.HeaderInfo, error) {
		newMC, err := cfg.SelectModel(name)
		if err != nil {
			return tui.HeaderInfo{}, err
		}
		newProvider, err := runtime.BuildProvider(newMC, cfg.Provider)
		if err != nil {
			return tui.HeaderInfo{}, err
		}
		runtime.AttachObserver(newProvider, store, ctx)
		runner.Model = newProvider
		runner.ModelName = newMC.Model
		runner.Temperature = newMC.Temperature
		runner.Compactor = runtime.BuildCompactor(newMC, newProvider)
		// Re-budget the session to the new model's window — same semantics as /use
		// in the REPL.
		sess.ContextWindow = newMC.ContextWindow
		sess.CompactThreshold = cfg.CompactThreshold(newMC)
		sess.Model = newMC.Model
		return tui.HeaderInfo{
			Model:            newMC.Name,
			Workspace:        filepath.Base(root),
			Session:          sess.ID,
			CompactThreshold: cfg.CompactThreshold(newMC),
		}, nil
	}
	goalOps := buildGoalOps(cfg, mc, runner, store)
	return tui.Run(ctx, backend, runner, sess, store, header, resume, modelSwap, cfg.ModelNames(), approver, goalOps)
}

func printUsage() {
	fmt.Println(`Usage:
  codeagent [--model NAME]                 start the TUI workspace (new session)
  codeagent [--model NAME] repl            start the interactive REPL (new session)
  codeagent [--model NAME] run "..."       run a single task
  codeagent [--model NAME] [--auto] goal "..."  pursue a goal to a verifiable end (exit code = outcome)
  codeagent [--model NAME] ask "..."       one-off question (no tools)
  codeagent sessions                       list saved sessions
  codeagent stats                          aggregate compaction + provider telemetry
  codeagent trace [N]                      show the last N requests, per attempt
  codeagent [--model NAME] resume <id>     resume a saved session
  codeagent [--model NAME] serve [addr]    run the runtime server (HTTP + agent-wire WebSocket; default 127.0.0.1:8787)

Sessions are stored per-project in .codeagent/sessions.db and persist across
runs, so a long conversation (and its summary) survives exit.

Models are defined in config.yaml under "models:"; --model selects one
(default: the configured default_model).`)
}
