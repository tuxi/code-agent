package approve

import (
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
)

// recordingApprover is a fallback that records whether it was consulted and
// returns a fixed verdict, so tests can tell short-circuit from delegation.
type recordingApprover struct {
	called  bool
	verdict agent.Verdict
}

func (r *recordingApprover) Approve(string, json.RawMessage) agent.Verdict {
	r.called = true
	if r.verdict == agent.VerdictAsk { return agent.VerdictDeny }
	return r.verdict
}

// newTestStore builds a RuleStore with a hermetic home (so it never reads the
// developer's real ~/.codeagent/settings.json) and no project settings file.
func newTestStore(t *testing.T, allow, deny []string) *RuleStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return NewRuleStore(t.TempDir(), allow, deny)
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"mcp__github__*", "mcp__github__list_issues", true},
		{"mcp__github__*", "mcp__db__query", false},
		{"mcp__*", "mcp__github__list_issues", true},
		{"*", "anything", true},
		{"mcp__db__query", "mcp__db__query", true},
		{"mcp__db__query", "mcp__db__queryx", false},
		{"mcp__*__query", "mcp__db__query", true},
		{"mcp__*__query", "mcp__db__insert", false},
		{"run_command", "edit_file", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// A nil store must return the wrapped approver unchanged.
func TestAllowlistedNilStorePassThrough(t *testing.T) {
	base := &recordingApprover{verdict: agent.VerdictAllow}
	if got := Allowlisted(nil, base); got != agent.Approver(base) {
		t.Fatalf("nil store should return the base approver unchanged, got %T", got)
	}
}

func TestAllowlistAllowShortCircuits(t *testing.T) {
	base := &recordingApprover{verdict: agent.VerdictDeny} // would deny if consulted
	a := Allowlisted(newTestStore(t, []string{"mcp__github__*"}, nil), base).(*Allowlist)

	ok, reason := a.ApproveAudited("mcp__github__list_issues", nil)
	if ok != agent.VerdictAllow {
		t.Fatal("allow rule should auto-approve")
	}
	if base.called {
		t.Fatal("an allow-matched call must not consult the fallback approver")
	}
	if reason == "" {
		t.Fatal("an allowlist auto-grant should carry an audit reason")
	}
}

// deny wins over allow, and a denied call never consults the fallback.
func TestAllowlistDenyBeatsAllow(t *testing.T) {
	base := &recordingApprover{verdict: agent.VerdictAllow}
	a := Allowlisted(newTestStore(t, []string{"mcp__github__*"}, []string{"mcp__github__delete_*"}), base).(*Allowlist)

	if ok, _ := a.ApproveAudited("mcp__github__delete_repo", nil); ok != agent.VerdictDeny {
		t.Fatal("deny should win over allow")
	}
	if base.called {
		t.Fatal("a denied call must not consult the fallback approver")
	}
}

// A call matching neither list falls through to the wrapped approver.
func TestAllowlistFallsThrough(t *testing.T) {
	base := &recordingApprover{verdict: agent.VerdictAllow}
	a := Allowlisted(newTestStore(t, []string{"mcp__github__*"}, nil), base).(*Allowlist)

	ok, _ := a.ApproveAudited("mcp__slack__post", nil)
	if !base.called {
		t.Fatal("an unmatched call should delegate to the fallback approver")
	}
	if ok != agent.VerdictAllow {
		t.Fatal("should return the fallback's verdict")
	}
}

// When wrapping an AutoApprover, the allowlist must forward its audited verdict
// so auto mode's own auto-grant reason still reaches the loop.
func TestAllowlistForwardsAuditedDelegate(t *testing.T) {
	root := t.TempDir()
	auto := NewAutoApprover(root, &recordingApprover{verdict: agent.VerdictDeny}, true) // enabled
	a := Allowlisted(newTestStore(t, []string{"mcp__x__*"}, nil), auto).(*Allowlist)

	// edit_file inside the workspace isn't in the allowlist, so it delegates to the
	// AutoApprover, which auto-approves workspace writes with a reason.
	in := json.RawMessage(`{"path":"foo.txt"}`)
	ok, reason := a.ApproveAudited("edit_file", in)
	if ok != agent.VerdictAllow || reason == "" {
		t.Fatalf("delegated auto-mode grant should keep its audit reason, got ok=%v reason=%q", ok, reason)
	}
}

// A rule Granted at runtime is honored on the next call, without rebuilding.
func TestAllowlistHonorsRuntimeGrant(t *testing.T) {
	base := &recordingApprover{verdict: agent.VerdictDeny}
	store := newTestStore(t, nil, nil)
	a := Allowlisted(store, base).(*Allowlist)

	// Before granting: unmatched → delegates (and base denies).
	if ok, _ := a.ApproveAudited("mcp__github__list_issues", nil); ok != agent.VerdictDeny {
		t.Fatal("no rule yet — should not be auto-approved")
	}
	if _, err := store.AllowAlways("mcp__github__list_issues"); err != nil {
		t.Fatalf("AllowAlways: %v", err)
	}
	// After granting the server wildcard: a DIFFERENT tool from the same server is
	// now auto-approved without a prompt.
	base.called = false
	ok, _ := a.ApproveAudited("mcp__github__create_pr", nil)
	if ok != agent.VerdictAllow {
		t.Fatal("granting mcp__github__* should auto-approve the whole server")
	}
	if base.called {
		t.Fatal("a granted call must not consult the fallback")
	}
}

// AutoApproverFrom must find the AutoApprover through an Allowlist wrapper (so the
// /auto toggle keeps working), and report absence when there is none.
func TestAutoApproverFromUnwraps(t *testing.T) {
	auto := NewAutoApprover(t.TempDir(), &recordingApprover{}, false)
	wrapped := Allowlisted(newTestStore(t, []string{"mcp__*"}, nil), auto)

	got, ok := AutoApproverFrom(wrapped)
	if !ok || got != auto {
		t.Fatalf("expected to unwrap the AutoApprover, got %v ok=%v", got, ok)
	}
	if _, ok := AutoApproverFrom(&recordingApprover{}); ok {
		t.Fatal("a plain approver has no AutoApprover")
	}
}
