package agent

import "context"

// TrustDecision is the three-state outcome of project trust resolution. Each
// link in the chain (CLI flag, hook, persistent store, interactive UI) returns
// a decision. Undecided falls through to the next link; Allowed/Denied
// short-circuits.
type TrustDecision int

const (
	// TrustUndecided: nothing in this layer decided; the caller should fall
	// through to the next link. Same pattern as VerdictAsk in permission.go.
	TrustUndecided TrustDecision = iota
	// TrustAllowed: the project is trusted; its settings and resources may be
	// loaded.
	TrustAllowed
	// TrustDenied: the project is NOT trusted; project-level settings,
	// hooks, and permissions must be skipped.
	TrustDenied
)

// TrustPolicy resolves whether a project directory is trusted. Different
// frontends implement it differently (TUI dialog, REPL prompt, RPC error,
// unattended deny), so it is an interface rather than a platform-specific
// call.
//
// cwd is the absolute project root. hasResources is true when the project
// actually contains trust-requiring resources (.codeagent/settings.json,
// etc.), letting the UI short-circuit silently trusted when there is
// nothing to trust.
//
// Nil-safe: a nil TrustPolicy means the project is trusted unconditionally
// (preserves backward compatibility — existing deployments have no trust
// gating).
type TrustPolicy interface {
	ResolveTrust(ctx context.Context, cwd string, hasResources bool) (TrustDecision, error)
}
