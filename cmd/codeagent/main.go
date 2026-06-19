package main

import (
	"code-agent/cmd/codeagent/tui"
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/observation"
	"code-agent/internal/session"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/git"
	projectgraph "code-agent/internal/tools/project_graph"
	"code-agent/internal/tools/search"
	"code-agent/internal/tools/shell"
	"code-agent/internal/tools/skill"
	"code-agent/internal/ui"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		case "trace":
			limit := 20
			if len(args) >= 2 {
				if n, err := strconv.Atoi(args[1]); err == nil {
					limit = n
				}
			}
			return runTrace(ctx, cfg, limit)
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
		return runTUI(ctx, cfg, mc, provider)
	}

	command := args[0]
	goal := strings.Join(args[1:], " ")

	switch command {
	case "ask":
		return runAsk(ctx, mc, provider, goal)
	case "run":
		return runAgent(ctx, cfg, mc, provider, goal)
	case "tui":
		return runTUI(ctx, cfg, mc, provider)
	case "repl":
		return repl(ctx, cfg, mc, provider, "")
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

// requestObserver records each model request to the store for transport
// telemetry. Best-effort: a telemetry write never fails the run.
type requestObserver struct {
	ctx   context.Context
	store session.Store
}

func (o requestObserver) Observe(s model.RequestStat) {
	trace := make([]session.AttemptRecord, len(s.Trace))
	for i, a := range s.Trace {
		result := a.ErrorClass
		if result == "" {
			result = "success"
		}
		trace[i] = session.AttemptRecord{LatencyMs: a.Latency.Milliseconds(), Result: result}
	}
	_ = o.store.RecordRequest(o.ctx, session.RequestRecord{
		At:               s.At,
		Model:            s.Model,
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.CompletionTokens,
		Attempts:         s.Attempts,
		Retries:          s.Retries,
		TimedOut:         s.TimedOut,
		Success:          s.Success,
		ErrorClass:       s.ErrorClass,
		LatencyMs:        s.Latency.Milliseconds(),
		Trace:            trace,
	})
}

// attachObserver wires request telemetry into a provider once the store is open
// (buildProvider always returns a *ResilientProvider, so the assertion holds).
func attachObserver(provider model.Provider, store session.Store, ctx context.Context) {
	if rp, ok := provider.(*model.ResilientProvider); ok {
		rp.Observer = requestObserver{ctx: ctx, store: store}
	}
}

// eventStoreEmitter persists each agent event to the session store (the P7
// EventStore — the raw, replayable runtime stream) and forwards it to the next
// renderer unchanged. A pure decorator, the same shape as liveProgress: it adds
// persistence with zero changes to the loop or the renderer it wraps. Best-effort
// like requestObserver — a telemetry write never fails a run.
type eventStoreEmitter struct {
	ctx   context.Context
	store session.Store
	next  agent.Emitter
}

func (e eventStoreEmitter) Emit(ev agent.Event) {
	if payload, err := json.Marshal(ev); err == nil {
		_ = e.store.RecordEvent(e.ctx, session.EventRecord{
			SessionID: ev.SessionID,
			TurnID:    ev.TurnID,
			Kind:      string(ev.Kind),
			At:        ev.At,
			Payload:   payload,
		})
	}
	if e.next != nil {
		e.next.Emit(ev)
	}
}

// withEventStore wraps a renderer so every event is persisted before it renders.
// Shared by run/repl/tui so all three log the event stream identically.
func withEventStore(next agent.Emitter, store session.Store, ctx context.Context) agent.Emitter {
	return eventStoreEmitter{ctx: ctx, store: store, next: next}
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

// runStats prints aggregate telemetry across all saved sessions.
func runStats(ctx context.Context, cfg app.Config) error {
	store, err := openStore(cfg.Workspace.Root)
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

// computeCost is the per-model spend: tokens × price-per-million, in the
// configured currency.
func computeCost(promptTokens, completionTokens int64, inPerM, outPerM float64) float64 {
	return (float64(promptTokens)*inPerM + float64(completionTokens)*outPerM) / 1_000_000
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
			cost := computeCost(u.PromptTokens, u.CompletionTokens, mc.InputPricePerM, mc.OutputPricePerM)
			total += cost
			anyPriced = true
			costStr = fmt.Sprintf("%s%.4f", cur, cost)
		}
		fmt.Printf("  %-18s reqs=%-4d in=%-9d out=%-9d %s\n",
			u.Model, u.Requests, u.PromptTokens, u.CompletionTokens, costStr)
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
	store, err := openStore(cfg.Workspace.Root)
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

// buildRunner assembles the agent.Runner shared by `run`, `repl`, and `tui`. The
// only things that differ between entry points are the Approver (how the user
// confirms a side-effecting tool) and the Emitter (how the event stream is
// rendered) — everything else (tools, observation, reflection, the skills nudge,
// compaction, the step cap) is identical, so it lives here and the three callers
// cannot drift apart.
func buildRunner(cfg app.Config, mc app.ModelConfig, provider model.Provider, registry *tools.Registry, skillReg *skills.Registry, approver agent.Approver, emitter agent.Emitter) *agent.Runner {
	return &agent.Runner{
		Model:        provider,
		ModelName:    mc.Model,
		Temperature:  mc.Temperature,
		Tools:        registry,
		MaxSteps:     cfg.Agent.MaxSteps,
		Approver:     approver,
		Observer:     observation.DefaultObserver{},
		Reflector:    agent.DefaultReflector{},
		RemindSkills: skillReg.Len() > 0,
		Compactor:    buildCompactor(mc, provider),
		Emitter:      emitter,
	}
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

// buildRegistry registers the model-facing tool set and loads the skills
// registry. Shared by run and repl. The returned skills registry feeds both the
// load_skill tool (here) and the system-prompt index (the session builder), so
// the index the model sees and the bodies it can load stay in sync.
func buildRegistry(root string) (*tools.Registry, *skills.Registry, error) {
	registry := tools.NewRegistry()

	skillReg, err := skills.Load(filepath.Join(root, "skills"))
	if err != nil {
		return nil, nil, err
	}

	// run_command and the job_* tools share one job registry, so a job_id
	// returned by a background run_command is resolvable by job_status/logs/cancel.
	runCmd := shell.NewRunCommandTool(root)
	jobReg := runCmd.Jobs

	for _, tool := range []tools.Tool{
		filesystem.NewListFilesTool(root),
		filesystem.NewReadFileTool(root),
		filesystem.NewCreateFileTool(root),
		filesystem.NewEditFileTool(root),
		search.NewGrepTool(root),
		projectgraph.NewProjectGraphTool(root),
		git.NewDiffTool(root),
		git.NewApplyPatchTool(root),
		git.NewGitCommitTool(root),
		runCmd,
		&shell.JobStatusTool{Jobs: jobReg},
		&shell.JobLogsTool{Jobs: jobReg},
		&shell.JobCancelTool{Jobs: jobReg},
		skill.NewLoadSkillTool(skillReg),
	} {
		if err := registry.Register(tool); err != nil {
			return nil, nil, err
		}
	}
	return registry, skillReg, nil
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

	registry, skillReg, err := buildRegistry(root)
	if err != nil {
		return err
	}

	store, err := openStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	attachObserver(provider, store, ctx)

	runner := buildRunner(cfg, mc, provider, registry, skillReg, ui.ConfirmApprover{}, withEventStore(buildEmitter(), store, ctx))

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

// runTUI launches the Phase 7 BubbleTea workspace (M1). It builds the same runner
// as `run`/`repl` (buildRunner) but with channel-backed Emitter/Approver, so the
// loop runs on a background goroutine while the program owns the terminal. The
// agent is unchanged; only the renderer differs.
func runTUI(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider) error {
	root := cfg.Workspace.Root

	registry, skillReg, err := buildRegistry(root)
	if err != nil {
		return err
	}

	store, err := openStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	attachObserver(provider, store, ctx)

	sess, err := session.NewBuilder(root).
		WithBudget(mc.ContextWindow, cfg.CompactThreshold(mc)).
		WithSkillsIndex(skillReg.PromptIndex()).
		Build()
	if err != nil {
		return err
	}
	sess.Model = mc.Model

	backend := tui.NewBackend()
	runner := buildRunner(cfg, mc, provider, registry, skillReg, backend.Approver, withEventStore(backend.Emitter, store, ctx))
	header := tui.HeaderInfo{
		Model:            mc.Name,
		Workspace:        filepath.Base(root),
		Session:          sess.ID,
		CompactThreshold: cfg.CompactThreshold(mc),
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
		newProvider, err := buildProvider(newMC, cfg.Provider)
		if err != nil {
			return tui.HeaderInfo{}, err
		}
		attachObserver(newProvider, store, ctx)
		runner.Model = newProvider
		runner.ModelName = newMC.Model
		runner.Temperature = newMC.Temperature
		runner.Compactor = buildCompactor(newMC, newProvider)
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
	return tui.Run(ctx, backend, runner, sess, store, header, resume, modelSwap, cfg.ModelNames())
}

func printUsage() {
	fmt.Println(`Usage:
  codeagent [--model NAME]                 start the TUI workspace (new session)
  codeagent [--model NAME] repl            start the interactive REPL (new session)
  codeagent [--model NAME] run "..."       run a single task
  codeagent [--model NAME] ask "..."       one-off question (no tools)
  codeagent sessions                       list saved sessions
  codeagent stats                          aggregate compaction + provider telemetry
  codeagent trace [N]                      show the last N requests, per attempt
  codeagent [--model NAME] resume <id>     resume a saved session

Sessions are stored per-project in .codeagent/sessions.db and persist across
runs, so a long conversation (and its summary) survives exit.

Models are defined in config.yaml under "models:"; --model selects one
(default: the configured default_model).`)
}
