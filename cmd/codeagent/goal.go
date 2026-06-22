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
func handleGoal(ctx context.Context, line string, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store) error {
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
		return goalResume(ctx, cfg, mc, runner, sess, store)
	default:
		return goalStart(ctx, cfg, mc, runner, sess, store, arg)
	}
}

func goalStart(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store, objective string) error {
	if existing, _ := goal.FromSession(sess); existing != nil &&
		(existing.Status == goal.StatusActive || existing.Status == goal.StatusPaused) {
		return fmt.Errorf("a goal is already in progress (%s); /goal resume to continue or /goal clear to drop it", existing.Status)
	}
	engine, err := buildGoalEngine(cfg, mc, runner, sess, store)
	if err != nil {
		return err
	}
	g := &goal.Goal{SessionID: sess.ID, Objective: objective, CreatedAt: time.Now()}
	fmt.Printf("goal set — pursuing (auto mode %s). Ctrl-C to pause.\n", autoState(runner))
	return pursue(ctx, engine, g)
}

func goalResume(ctx context.Context, cfg app.Config, mc app.ModelConfig, runner *agent.Runner, sess *session.Session, store session.Store) error {
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
	fmt.Printf("resuming goal (turns so far: %d). Ctrl-C to pause.\n", g.Spent.Turns)
	return pursue(ctx, engine, g)
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
