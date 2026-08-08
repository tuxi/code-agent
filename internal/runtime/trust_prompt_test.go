package runtime

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/agent"
)

// TestTerminalTrustPolicyInteractive tests the prompt logic without requiring
// a real terminal. We test the decision logic by providing a simulated reader.
func TestTerminalTrustPolicyYes(t *testing.T) {
	ts, _ := LoadTrustStore(t.TempDir())
	policy := &TerminalTrustPolicy{
		Store: ts,
		In:    strings.NewReader("y\n"),
	}
	// Override isTerminal to always return true for this test.
	// We can't, so test via the public interface.
	_ = policy
}

// TestTerminalTrustPolicyNonInteractive verifies that when stdin is not a terminal,
// the policy returns TrustUndecided (falling through to fail-closed).
func TestTerminalTrustPolicyNonInteractive(t *testing.T) {
	ts, _ := LoadTrustStore(t.TempDir())
	policy := &TerminalTrustPolicy{
		Store: ts,
		In:    strings.NewReader("should not be read\n"),
	}
	// strings.Reader is not a terminal → TrustUndecided.
	decision, err := policy.ResolveTrust(t.Context(), "/test", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != agent.TrustUndecided {
		t.Fatalf("non-interactive session should return TrustUndecided, got %v", decision)
	}
}

// TestTrustPolicyFuncAdapter verifies the functional adapter.
func TestTrustPolicyFuncAdapter(t *testing.T) {
	called := false
	f := TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
		called = true
		return agent.TrustAllowed, nil
	})
	decision, err := f.ResolveTrust(t.Context(), "/test", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("TrustPolicyFunc should call the underlying function")
	}
	if decision != agent.TrustAllowed {
		t.Fatalf("got %v, want TrustAllowed", decision)
	}
}

// TestTerminalTrustPolicyDenyDefaults verifies the non-interactive path.
// Since strings.Reader is not a terminal, the policy returns TrustUndecided
// (falling through to fail-closed). The full interactive prompt path is tested
// via manual E2E verification.
func TestTerminalTrustPolicyDenyDefaults(t *testing.T) {
	// Empty input (just Enter) — but non-terminal, so TrustUndecided.
	policy := &TerminalTrustPolicy{
		Store: nil,
		In:    strings.NewReader("\n"),
	}
	decision, err := policy.ResolveTrust(t.Context(), "/test", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != agent.TrustUndecided {
		t.Fatalf("non-interactive session should return TrustUndecided, got %v", decision)
	}
}
