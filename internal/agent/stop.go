package agent

import (
	"context"
	"strings"

	"code-agent/internal/tools"
)

// StopPolicy is the finalize decision point (8.4/8.5): it runs when the model
// returns a text-only message and wants to finish. The harness provides the
// POINT; the policy supplies the JUDGMENT. A Continue verdict rejects the
// finish; the optional Message is re-injected as a one-shot ephemeral nudge on
// the next model call (never persisted).
//
// Nil-safe: Runner.StopPolicy == nil selects the built-in default
// (finalizePolicy), so configuring nothing preserves the loop's pre-policy
// behavior exactly. An external policy REPLACES the default — the operator
// owns the stop decision. The loop's step budget is the hard backstop behind
// any policy, so a misbehaving policy can delay but never deadlock the run.
type StopPolicy interface {
	Decide(ctx context.Context, sc StopContext) (StopVerdict, error)
}

// StopContext is the state snapshot a stop policy may consult. The default
// policy reaches into the Runner for its deterministic checks; the snapshot
// exists so any policy — including an external after_turn hook — can decide
// without harness internals.
type StopContext struct {
	LastText    string       // the model's final text-only message
	Todos       []tools.Todo // the materialized task checklist (D4), in model order
	Steps       []Step       // this turn's tool steps (same shape the Reflector reads)
	PlanState   PlanStatus
	CodeMutated bool // the turn changed verifiable code (best-effort snapshot)
	ToolCalls   int  // tool calls executed this turn
	MaxSteps    int  // the loop's step budget (the hard backstop behind any policy)
}

// StopVerdict is a policy's ruling. Continue=true rejects the finish; Message
// (optional) is the one-shot ephemeral nudge injected on the next model call.
// An empty verdict accepts the finish.
type StopVerdict struct {
	Continue bool
	Message  string
}

// StopPolicyFunc adapts a function to the StopPolicy interface.
type StopPolicyFunc func(ctx context.Context, sc StopContext) (StopVerdict, error)

// Decide implements StopPolicy.
func (f StopPolicyFunc) Decide(ctx context.Context, sc StopContext) (StopVerdict, error) {
	return f(ctx, sc)
}

// finalizePolicy is the default stop policy: the finalize gate sequence that
// used to be inlined in drive(). One instance per turn (created at the top of
// drive()), so its one-shot flags reset naturally. Extracted so the stop
// decision is replaceable — the harness keeps the point, the policy keeps the
// judgment.
type finalizePolicy struct {
	r             *Runner
	reflected     bool // one self-check pass per turn
	verified      bool // the deterministic verify runs at most once
	todoGateFired bool // the todo gate re-prompts at most once
}

// Decide implements StopPolicy. It is consulted only after the plan-state
// machine gate, which is a protocol invariant and stays in the loop.
func (p *finalizePolicy) Decide(ctx context.Context, sc StopContext) (StopVerdict, error) {
	// Deterministic build verify (P4.3-R Move 2, option 2a): a turn that changed
	// verifiable code without verifying it and a real verify command is
	// configured — run it once, deterministically, and feed back the TRUTH
	// instead of guessing. A pass confirms the change; a failure re-prompts with
	// the real error so the model fixes the actual cause. With no VerifyCommand
	// this block is skipped entirely (2b silence: the runtime never asserts
	// "unverified").
	if p.r.Reflector != nil && !p.reflected {
		rc := p.r.Reflector.Reflect(sc.Steps)
		p.r.mutatedVerifiableCode = len(rc.CodeFilesMutated) > 0
		if rc.UnverifiedMutation && p.r.VerifyCommand != "" && !p.verified {
			p.verified = true
			status, summary := p.r.runFinalizeVerify(ctx)
			p.r.emit(Event{Kind: EventVerified, Text: summary})
			switch status {
			case VerifyFailed:
				p.reflected = true // don't also stack another nudge this pass
				return StopVerdict{Continue: true, Message: "[reflection] The verification `" + p.r.VerifyCommand +
					"` was run against your change to " + strings.Join(rc.CodeFilesMutated, ", ") +
					" and it FAILED:\n" + summary +
					"\nThis is the real result, not a guess. Fix the cause, then finish."}, nil
			case VerifyCouldNotRun:
				p.reflected = true
				if summary == "" {
					summary = "command could not run (exit -1)"
				}
				return StopVerdict{Continue: true, Message: "[reflection] The verification `" + p.r.VerifyCommand +
					"` could not run: " + summary +
					"\nThis is an environment problem (e.g. the toolchain is not on PATH), not a verdict on your change." +
					"\nRetry with an explicit toolchain path, or finish without it."}, nil
			}
			// Passed: the change is genuinely verified. Fall through.
		}
		// The TestEditedAfterFailure fact-question (rc.Nudge) is retired: per
		// sign-off the reflective nudge was removed from the default policy as
		// per-turn noise. The deterministic verify above is the surviving rail.
	}
	// Change review: plan-mode changes get one independent review. The count
	// increments only when a review completes, so the model ignoring the harness
	// re-triggers on the next done.
	if p.r.PlanState == PlanStatusExecuting && p.r.independentTaskAvailable() &&
		p.r.plannedMutation && !p.r.independentReviewPassed && p.r.changeReviewCount < 1 {
		return StopVerdict{Continue: true, Message: changeReviewPrompt}, nil
	}
	// Todo gate: the model declared checklist work it has not completed.
	// One-shot per turn — force a reconcile (mark done, clear aspirational
	// items, or state explicitly why it is stopping with these pending) before
	// accepting the finish. It only guards a voluntary finish, not the step-limit
	// backstop.
	if !p.todoGateFired && hasPendingTodos(sc.Todos) {
		p.todoGateFired = true
		return StopVerdict{Continue: true, Message: todoReconcileNudge(sc.Todos)}, nil
	}
	return StopVerdict{}, nil
}
