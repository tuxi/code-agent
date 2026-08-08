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

// VerifyGate is an opt-in StopPolicy that runs a build/test command when the
// turn changed verifiable code without verifying it. A pass confirms the
// change; a failure re-prompts with the real result. The command runs at most
// once per gate instance (one turn). Set VerifyGate as Runner.StopPolicy to
// enable; leave Runner.StopPolicy nil for the trust-the-model default.
type VerifyGate struct {
	Command string  // e.g. "go build ./... && go test ./..."
	runner  *Runner // for runFinalizeVerify + emit

	reflector Reflector // defaults to DefaultReflector
	verified  bool
}

// NewVerifyGate creates a VerifyGate wired to the given runner's
// runFinalizeVerify and emit hooks. The Command field must be set before
// the gate is consulted (typically the same value that was previously
// configured as Runner.VerifyCommand).
func NewVerifyGate(runner *Runner, command string) *VerifyGate {
	return &VerifyGate{runner: runner, Command: command}
}

// Decide implements StopPolicy.
func (g *VerifyGate) Decide(ctx context.Context, sc StopContext) (StopVerdict, error) {
	if g.verified {
		return StopVerdict{}, nil
	}
	if g.reflector == nil {
		g.reflector = DefaultReflector{}
	}
	rc := g.reflector.Reflect(sc.Steps)
	if !rc.UnverifiedMutation || g.Command == "" {
		return StopVerdict{}, nil
	}

	g.verified = true
	status, summary := g.runner.runFinalizeVerify(ctx, g.Command)
	g.runner.emit(Event{Kind: EventVerified, Text: summary})

	switch status {
	case VerifyFailed:
		return StopVerdict{Continue: true, Message: "[reflection] The verification `" + g.Command +
			"` was run against your change to " + strings.Join(rc.CodeFilesMutated, ", ") +
			" and it FAILED:\n" + summary +
			"\nThis is the real result, not a guess. Fix the cause, then finish."}, nil
	case VerifyCouldNotRun:
		if summary == "" {
			summary = "command could not run (exit -1)"
		}
		return StopVerdict{Continue: true, Message: "[reflection] The verification `" + g.Command +
			"` could not run: " + summary +
			"\nThis is an environment problem (e.g. the toolchain is not on PATH), not a verdict on your change." +
			"\nRetry with an explicit toolchain path, or finish without it."}, nil
	default:
		return StopVerdict{}, nil
	}
}

// TodoGate is an opt-in StopPolicy that checks the model's own task checklist
// before accepting a finish. When the model wants to stop with uncompleted
// items, it fires a one-shot reconciliation nudge — the model must either mark
// items done, clear aspirational ones, or state why it is stopping anyway.
type TodoGate struct {
	fired bool
}

// Decide implements StopPolicy.
func (g *TodoGate) Decide(_ context.Context, sc StopContext) (StopVerdict, error) {
	if g.fired || !hasPendingTodos(sc.Todos) {
		return StopVerdict{}, nil
	}
	g.fired = true
	return StopVerdict{Continue: true, Message: todoReconcileNudge(sc.Todos)}, nil
}

// ComposeStopPolicy chains multiple policies in order. The first one that
// returns Continue=true short-circuits; later policies are not consulted.
// An empty verdict from all policies accepts the finish.
func ComposeStopPolicy(policies ...StopPolicy) StopPolicy {
	return StopPolicyFunc(func(ctx context.Context, sc StopContext) (StopVerdict, error) {
		for _, p := range policies {
			v, err := p.Decide(ctx, sc)
			if err != nil || v.Continue {
				return v, err
			}
		}
		return StopVerdict{}, nil
	})
}
