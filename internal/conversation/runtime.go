package conversation

import (
	"context"

	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// TurnRunner is the slice of *agent.Runner that TurnExecutor drives. Defining it
// as an interface keeps the executor testable with a fake.
type TurnRunner interface {
	RunTurn(ctx context.Context, sess *session.Session, userInput string) (agent.TurnResult, error)
	// ResumeTurn continues an interrupted turn from persisted history without
	// appending a new user message (v1.2 §3.2).
	ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error)
}

// AssetTurnRunner is optional so alternate/legacy runners remain compatible.
type AssetTurnRunner interface {
	RunTurnWithAssets(ctx context.Context, sess *session.Session, userInput string, assets []model.GatewayAssetRef) (agent.TurnResult, error)
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

	// Model is the optional model profile name for this turn. When non-empty,
	// the runner looks up this model from config instead of using the server
	// default. Set from the client's agent_input.model field. Empty means
	// "use the server's default_model".
	Model string

	// Credential is the per-session credential resolver. When non-nil, the
	// turn runner uses this resolver (chained with the base resolver) for
	// model calls. In server mode this carries the client's JWT extracted
	// from the Authorization header at WS upgrade time. Nil means "use the
	// base provider as-is" (embedded + CLI modes).
	Credential credential.Resolver

	// Checkpointer persists the session mid-turn (v1.2 §2). The TurnExecutor sets
	// it from its repository so the Runner can checkpoint at each loop boundary;
	// nil in headless/test builders keeps turn-boundary-only saving.
	Checkpointer agent.Checkpointer
}

// RunBuilder is the seam the transport layer (cmd/codeagent) fills. It assembles
// a fresh turnRunner for each turn from global config (model, provider, tool
// registry, skill registry) plus the per-turn RuntimeContext. The conversation
// package deliberately does not depend on app.Config.
type RunBuilder interface {
	Build(ctx RuntimeContext) TurnRunner
}

// TitleGenerator optionally produces a concise human-readable title from the
// first exchange of a conversation. It is invoked asynchronously after the first
// turn so the user sees a descriptive name in session lists without blocking the
// turn response.
type TitleGenerator interface {
	GenerateTitle(ctx context.Context, userMessage, assistantResponse string) (string, error)
}
