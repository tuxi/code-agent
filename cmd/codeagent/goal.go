package main

import (
	"code-agent/cmd/codeagent/tui"
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/approve"
	"code-agent/internal/goal"
	"code-agent/internal/session"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

// handleGoal dispatches the /goal lifecycle:
//
//	/goal <objective>   set and start pursuing
//	/goal               show the current goal's status
//	/goal resume        continue a paused / budget-limited / blocked goal
//	/goal clear         drop the current goal
//
// Pursue runs in the FOREGROUND: the worker's approver (ui.ConfirmApprover, wrapped
// by AutoApprover) reads y/N through the REPL's single readline, so a backgrounded
// pursuit would race the input loop for stdin. Blocking the input loop while Pursue
// runs gives the worker sole ownership of stdin — so with auto OFF it stops to
// confirm each side-effecting tool (documented expected behavior), and with auto ON
// it never prompts and runs hands-off. Ctrl-C pauses at the turn boundary.
//
// The goal package is auto-mode-agnostic: the worker simply inherits runner.Approver.
func handleGoal(ctx context.Context, line string, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store, ask lineReader) error {
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/goal"))
	switch arg {
	case "":
		return goalStatus(sess)
	case "clear":
		goal.Clear(sess)
		if err := store.Save(ctx, sess); err != nil {
			return err
		}
		fmt.Println("goal cleared.")
		return nil
	case "resume":
		return goalResume(ctx, cfg, mc, runner, sess, store, ask)
	default:
		return goalStart(ctx, cfg, mc, runner, sess, store, ask, arg)
	}
}

func goalStart(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store, ask lineReader, objective string) error {
	if existing, _ := goal.FromSession(sess); existing != nil &&
		(existing.Status == goal.StatusActive || existing.Status == goal.StatusPaused) {
		return fmt.Errorf("a goal is already in progress (%s); /goal resume to continue or /goal clear to drop it", existing.Status)
	}
	// Admission (§4.7): reject goals with no verifiable endpoint / high-risk actions
	// before spending anything. Resume skips this — the goal was already admitted.
	if err := admitGoal(ctx, cfg, mc, runner, objective); err != nil {
		return err
	}
	engine, err := buildGoalEngine(cfg, mc, runner, sess, store)
	if err != nil {
		return err
	}
	restore := offerAuto(runner, ask) // prompts + enables now; restores on return
	defer restore()
	g := &goal.Goal{SessionID: sess.ID, Objective: objective, CreatedAt: time.Now()}
	fmt.Printf("goal set — pursuing (auto mode %s). Ctrl-C to pause.\n", autoState(runner))
	return pursue(ctx, engine, g)
}

func goalResume(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store, ask lineReader) error {
	g, err := goal.FromSession(sess)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("no goal to resume; /goal <objective> to start one")
	}
	if g.Status == goal.StatusAchieved || g.Status == goal.StatusCleared {
		return fmt.Errorf("goal already %s; nothing to resume", g.Status)
	}
	engine, err := buildGoalEngine(cfg, mc, runner, sess, store)
	if err != nil {
		return err
	}
	restore := offerAuto(runner, ask)
	defer restore()
	fmt.Printf("resuming goal (turns so far: %d, auto mode %s). Ctrl-C to pause.\n", g.Spent.Turns, autoState(runner))
	return pursue(ctx, engine, g)
}

// admitObjective runs the §4.7 set-time gate on the cheap independent model. It
// does NOT print — each driver surfaces the result its own way (REPL prints; TUI
// folds it into the outcome, since printing would corrupt the render). err != nil
// means rejected. caveat != "" means admitted-with-a-warning (fuzzy, or the
// admitter was unavailable and we failed open). Admission is advisory UX, not the
// safety boundary — the approver guards high-risk actions regardless.
func admitObjective(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, objective string) (caveat string, err error) {
	provider, amc := resolveSubAgentModel(cfg, mc, runner.Model)
	res, aerr := (&goal.LLMAdmitter{Provider: provider, Model: amc.Model}).Admit(ctx, objective)
	if aerr != nil {
		return fmt.Sprintf("准入判定不可用(%v),已从宽放行。", aerr), nil
	}
	if !res.OK {
		return "", fmt.Errorf("这个目标不适合 /goal:%s", res.Reason)
	}
	if res.Fuzzy {
		return "注意:该目标没有干净的 verify 命令,裁判依据证据判定,可靠性低于命令式终点 — " + res.Reason, nil
	}
	return "", nil
}

// newGoalEngine wires the engine with an INDEPENDENT cheap judge (the
// agent.subagent_model knob) so the worker never grades itself. It does NOT print;
// degraded reports that no separate model was configured (judge fell back to the
// worker's model), which each driver surfaces its own way.
func newGoalEngine(cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store) (engine *goal.Engine, degraded bool, err error) {
	checkerProvider, checkerMC := resolveSubAgentModel(cfg, mc, runner.Model)
	checker := &goal.LLMChecker{Provider: checkerProvider, Model: checkerMC.Model}
	engine, err = goal.NewEngine(sess, store, runner, checker)
	return engine, checkerMC.Model == mc.Model, err
}

// admitGoal is the REPL wrapper around admitObjective: it prints the caveat and
// returns the rejection error.
func admitGoal(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, objective string) error {
	caveat, err := admitObjective(ctx, cfg, mc, runner, objective)
	if err != nil {
		return err
	}
	if caveat != "" {
		fmt.Println(caveat)
	}
	return nil
}

// buildGoalEngine is the REPL wrapper around newGoalEngine: it prints the
// degraded-judge warning.
func buildGoalEngine(cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store) (*goal.Engine, error) {
	engine, degraded, err := newGoalEngine(cfg, mc, runner, sess, store)
	if degraded {
		fmt.Println("warning: no separate judge model (agent.subagent_model) configured — the checker runs the SAME model as the worker, so judge separation is degraded.")
	}
	return engine, err
}

// goalOps implements tui.GoalOps — the cmd-layer's goal capability injected into
// the TUI run loop (it owns admission + engine/checker construction). None of its
// methods print; the TUI renders the returned strings (a stdout write would
// corrupt the BubbleTea screen).
type goalOps struct {
	cfg    app.Config
	mc     app.ModelConfig
	runner *agent.Runner
	store  session.Store
}

func buildGoalOps(cfg app.Config, mc app.ModelConfig, runner *agent.Runner, store session.Store) tui.GoalOps {
	return goalOps{cfg: cfg, mc: mc, runner: runner, store: store}
}

// Pursue starts a new goal (objective != "") or resumes the session's existing
// goal (objective == ""), runs it to a terminal state, and returns a one-line
// outcome. The engine is the sole writer, so it persists itself.
func (o goalOps) Pursue(pctx context.Context, sess *session.Session, objective string) (string, error) {
	var g *goal.Goal
	var caveat string
	if objective == "" {
		ex, err := goal.FromSession(sess)
		if err != nil {
			return "", err
		}
		if ex == nil {
			return "", fmt.Errorf("no goal to resume; /goal <objective> to start one")
		}
		if ex.Status == goal.StatusAchieved || ex.Status == goal.StatusCleared {
			return "", fmt.Errorf("goal already %s; nothing to resume", ex.Status)
		}
		g = ex
	} else {
		if ex, _ := goal.FromSession(sess); ex != nil &&
			(ex.Status == goal.StatusActive || ex.Status == goal.StatusPaused) {
			return "", fmt.Errorf("a goal is already in progress (%s); /goal resume or /goal clear", ex.Status)
		}
		c, err := admitObjective(pctx, o.cfg, o.mc, o.runner, objective)
		if err != nil {
			return "", err // rejected — the TUI prints it
		}
		caveat = c
		g = &goal.Goal{SessionID: sess.ID, Objective: objective, CreatedAt: time.Now()}
	}
	engine, degraded, err := newGoalEngine(o.cfg, o.mc, o.runner, sess, o.store)
	if err != nil {
		return "", err
	}
	if degraded {
		caveat = appendCaveat(caveat, "未配置独立裁判模型(agent.subagent_model),裁判与 worker 同模型,独立性降低。")
	}
	perr := engine.Pursue(pctx, g)
	return goalSummaryLine(g, caveat), perr
}

// Status formats the session's current goal for /goal (no args).
func (o goalOps) Status(sess *session.Session) string {
	g, err := goal.FromSession(sess)
	if err != nil {
		return "goal: " + err.Error()
	}
	if g == nil {
		return "no active goal. /goal <objective> to start one."
	}
	return goalStatusText(g)
}

// Clear drops the session's goal and persists.
func (o goalOps) Clear(ctx context.Context, sess *session.Session) error {
	goal.Clear(sess)
	return o.store.Save(ctx, sess)
}

// goalSummaryLine is the one-line outcome printed when a pursuit ends.
func goalSummaryLine(g *goal.Goal, caveat string) string {
	line := fmt.Sprintf("◎ /goal %s — turns %d, tokens %d, wall %s",
		g.Status, g.Spent.Turns, g.Spent.Tokens, g.Spent.Wall.Round(time.Second))
	if g.StatusNote != "" && g.Status != goal.StatusAchieved {
		line += "\n  reason: " + g.StatusNote
	}
	if g.CheckerNote != "" {
		line += "\n  judge: " + g.CheckerNote
	}
	if caveat != "" {
		line = caveat + "\n" + line
	}
	return line
}

// goalStatusText is the multi-line status panel for an existing goal, shared by
// the REPL (prints it) and the TUI (renders it).
func goalStatusText(g *goal.Goal) string {
	s := fmt.Sprintf("◎ /goal %s\n  objective: %s\n  turns: %d, tokens: %d, wall: %s",
		g.Status, truncateLine(g.Objective, 100), g.Spent.Turns, g.Spent.Tokens, g.Spent.Wall.Round(time.Second))
	if g.StatusNote != "" {
		s += "\n  note: " + g.StatusNote
	}
	if g.CheckerNote != "" {
		s += "\n  judge: " + g.CheckerNote
	}
	return s
}

func appendCaveat(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

// pursue runs the loop in the foreground under a signal context: Ctrl-C cancels
// the in-flight turn, which the Engine settles to paused. It self-reports the
// outcome and returns nil — terminal states (paused/blocked/…) are results, not
// command errors.
func pursue(ctx context.Context, e *goal.Engine, g *goal.Goal) error {
	pursueCtx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()
	err := e.Pursue(pursueCtx, g)
	reportGoal(g, err)
	return nil
}

func goalStatus(sess *session.Session) error {
	g, err := goal.FromSession(sess)
	if err != nil {
		return err
	}
	if g == nil {
		fmt.Println("no active goal. /goal <objective> to start one.")
		return nil
	}
	fmt.Println(goalStatusText(g))
	return nil
}

func reportGoal(g *goal.Goal, err error) {
	if err != nil {
		fmt.Println("\ngoal: internal error:", err)
	}
	fmt.Printf("\n◎ /goal %s — turns: %d, tokens: %d, wall: %s\n",
		g.Status, g.Spent.Turns, g.Spent.Tokens, g.Spent.Wall.Round(time.Second))
	switch g.Status {
	case goal.StatusAchieved:
		fmt.Println("  ✓ achieved — control is yours again.")
	case goal.StatusPaused:
		fmt.Println("  paused — /goal resume to continue.")
	case goal.StatusBudgetLimited, goal.StatusBlocked, goal.StatusErrored:
		if g.StatusNote != "" {
			fmt.Println("  reason:", g.StatusNote)
		}
		fmt.Println("  /goal resume to retry, or /goal clear to drop.")
	}
	// Always show the judge's reason — on achieved it confirms the separate judge
	// actually evaluated (and why it accepted), not just that the loop returned met.
	if g.CheckerNote != "" {
		fmt.Println("  judge:", g.CheckerNote)
	}
}

// offerAuto enables auto mode for the duration of one pursuit, with a single
// explicit consent, when it is currently off — so /goal is hands-off without a
// separate /auto on, but never silently. It returns a restore func (the caller
// defers it) that puts auto mode back as it was. Already-on (or no AutoApprover)
// → no prompt, no-op: a user who wants auto to persist across goals just runs
// /auto on once and is never asked again.
func offerAuto(runner *agent.Runner, ask lineReader) func() {
	a, ok := runner.Approver.(*approve.AutoApprover)
	if !ok || a.Enabled() {
		return func() {}
	}
	line, err := ask("auto mode is OFF — enable it for this pursuit? in-workspace edits auto-approved, commands still confirmed [y/N]: ")
	if err != nil || !isYes(line) {
		fmt.Println("keeping auto OFF — the worker will stop at y/N for each side-effecting tool (/auto on to keep it on across goals).")
		return func() {}
	}
	a.SetEnabled(true)
	return func() { a.SetEnabled(false) } // restore: this consent was for one pursuit only
}

func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

// autoState describes the worker's approval posture for the pursuit banner.
func autoState(runner *agent.Runner) string {
	if a, ok := runner.Approver.(*approve.AutoApprover); ok && a.Enabled() {
		return "ON — hands-off"
	}
	return "OFF — will stop at y/N per side-effecting tool; /auto on for hands-off"
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
