package main

import (
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

// admitGoal runs the §4.7 set-time gate on the cheap independent model. A rejected
// goal returns an error (the REPL prints it). It FAILS OPEN: if the admitter itself
// errors, it warns and proceeds — admission is advisory UX, not the safety boundary
// (the approver guards high-risk actions regardless).
func admitGoal(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, objective string) error {
	provider, amc := resolveSubAgentModel(cfg, mc, runner.Model)
	res, err := (&goal.LLMAdmitter{Provider: provider, Model: amc.Model}).Admit(ctx, objective)
	if err != nil {
		fmt.Printf("note: 准入判定不可用(%v),继续。\n", err)
		return nil
	}
	if !res.OK {
		return fmt.Errorf("这个目标不适合 /goal:%s", res.Reason)
	}
	if res.Fuzzy {
		fmt.Printf("注意:该目标没有干净的 verify 命令,裁判依据证据判定,可靠性低于命令式终点 — %s\n", res.Reason)
	}
	return nil
}

// buildGoalEngine wires the engine. The checker runs on an INDEPENDENT cheap model
// (the agent.subagent_model knob, via resolveSubAgentModel) so the judge is not the
// worker grading itself. If no separate model is configured it falls back to the
// worker's — usable for trying /goal out, but judge separation is degraded, so warn.
func buildGoalEngine(cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store) (*goal.Engine, error) {
	checkerProvider, checkerMC := resolveSubAgentModel(cfg, mc, runner.Model)
	if checkerMC.Model == mc.Model {
		fmt.Println("warning: no separate judge model (agent.subagent_model) configured — the checker runs the SAME model as the worker, so judge separation is degraded.")
	}
	checker := &goal.LLMChecker{Provider: checkerProvider, Model: checkerMC.Model}
	return goal.NewEngine(sess, store, runner, checker)
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
	fmt.Printf("◎ /goal %s\n  objective: %s\n  turns: %d, tokens: %d, wall: %s\n",
		g.Status, truncateLine(g.Objective, 100), g.Spent.Turns, g.Spent.Tokens, g.Spent.Wall.Round(time.Second))
	if g.StatusNote != "" {
		fmt.Println("  note:", g.StatusNote)
	}
	if g.CheckerNote != "" {
		fmt.Println("  judge:", g.CheckerNote)
	}
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
