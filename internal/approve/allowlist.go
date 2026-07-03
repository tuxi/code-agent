package approve

import (
	"encoding/json"
	"strings"

	"code-agent/internal/agent"
)

// Allowlist is a policy-driven agent.Approver decorator that pre-approves (or
// denies) tool calls by name pattern, so a user does not confirm every call from
// a trusted MCP server one at a time. It mirrors Claude Code's permission model:
// allow/deny are tool-name globs (e.g. "mcp__github__*"), evaluated deny-first,
// and a call matching neither falls through to the wrapped approver (the human
// prompt, or the AutoApprover).
//
// The verdict is read from a shared RuleStore on every call, so a rule the user
// adds mid-session via "Always allow" (RuleStore.Grant) takes effect immediately.
//
// Layering: the Allowlist is the OUTERMOST decision, so a matched allow rule
// short-circuits before auto mode or the human is consulted. It implements
// agent.AuditedApprover both to log its own auto-grants (the loop emits a
// correlated EventAutoApproved) and to forward the wrapped approver's audited
// verdict, so auto mode's auditing still works underneath.
//
// Scope: this gates only calls that reach the approver — every MCP tool (all are
// treated as side-effecting) plus side-effecting built-ins. Read-only built-ins
// never reach the approver, so a deny rule does not hide them.
type Allowlist struct {
	store *RuleStore
	next  agent.Approver
}

// Allowlisted wraps next with an Allowlist backed by store. A nil store returns
// next unchanged. An empty store still wraps (rules may be added at runtime), but
// then every call simply delegates — behaviorally identical to no allowlist until
// a rule exists.
func Allowlisted(store *RuleStore, next agent.Approver) agent.Approver {
	if store == nil {
		return next
	}
	return &Allowlist{store: store, next: next}
}

// Approve implements agent.Approver (verdict only).
func (a *Allowlist) Approve(toolName string, input json.RawMessage) bool {
	ok, _ := a.ApproveAudited(toolName, input)
	return ok
}

// ApproveAudited implements agent.AuditedApprover. deny wins over allow (Claude's
// precedence). A matched allow rule auto-grants with a reason the loop logs.
// Otherwise it delegates, preserving the wrapped approver's audit when it has one.
func (a *Allowlist) ApproveAudited(toolName string, input json.RawMessage) (bool, string) {
	if _, ok := a.store.MatchDeny(toolName); ok {
		return false, "" // denied — no audit reason (denials are not auto-grants)
	}
	if pattern, ok := a.store.MatchAllow(toolName); ok {
		return true, "auto-approved by permission rule " + pattern
	}
	if aa, ok := a.next.(agent.AuditedApprover); ok {
		return aa.ApproveAudited(toolName, input)
	}
	return a.next.Approve(toolName, input), ""
}

// AutoApproverFrom unwraps a possibly-decorated approver chain to find the
// AutoApprover, so the `/auto` toggle keeps working when an Allowlist (or any
// future decorator) wraps it. Returns false when no AutoApprover is present
// (e.g. the wire-server path uses a RemoteApprover with no auto mode).
func AutoApproverFrom(a agent.Approver) (*AutoApprover, bool) {
	switch v := a.(type) {
	case *AutoApprover:
		return v, true
	case *Allowlist:
		return AutoApproverFrom(v.next)
	default:
		return nil, false
	}
}

// matchGlob reports whether name matches a tool-name pattern where '*' is a
// wildcard for any run of characters. Tool names contain no '/', so a simple
// segment matcher suffices (and avoids path.Match's '/'-aware semantics and its
// error on a stray '[' in a name). "*" alone matches everything; a pattern with
// no '*' must match exactly.
func matchGlob(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(name, mid)
		if i < 0 {
			return false
		}
		name = name[i+len(mid):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}

var (
	_ agent.Approver        = (*Allowlist)(nil)
	_ agent.AuditedApprover = (*Allowlist)(nil)
)
