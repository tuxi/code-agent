package conversation

import (
	"context"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

// TurnRunner is the slice of *agent.Runner that TurnExecutor drives. Defining it
// as an interface keeps the executor testable with a fake.
type TurnRunner interface {
	RunTurn(ctx context.Context, sess *session.Session, userInput string) (agent.TurnResult, error)
}

// RuntimeContext is the parameter bundle for RunBuilder.Build. It collects
// everything a per-turn Runtime needs — session state, event publisher, and
// approvers — so the factory signature stays stable as more fields are added.
type RuntimeContext struct {
	Session      *session.Session
	Publisher    agent.Emitter // TurnExecutor assembles the composite emitter
	Approver     agent.Approver
	PlanApprover agent.PlanApprover     // nil = auto-approve plans (test/headless path)
	ClientWaiter agent.ClientToolWaiter // nil = no client tool executor
	ClientTools  []agent.ClientToolDef  // client-registered tools (nil if none)
}

// RunBuilder is the seam the transport layer (cmd/codeagent) fills. It assembles
// a fresh turnRunner for each turn from global config (model, provider, tool
// registry, skill registry) plus the per-turn RuntimeContext. The conversation
// package deliberately does not depend on app.Config.
type RunBuilder interface {
	Build(ctx RuntimeContext) TurnRunner
}
