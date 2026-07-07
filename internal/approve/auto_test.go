package approve

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"code-agent/internal/agent"
)

// fakeHuman records whether it was consulted and returns a fixed verdict, so a
// test can assert "fell back to human" vs "auto-approved without asking".
type fakeHuman struct {
	v     agent.Verdict
	calls int
}

func (h *fakeHuman) Approve(string, json.RawMessage) agent.Verdict {
	h.calls++
	return h.v
}

func editInput(t *testing.T, path string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"path": path, "old": "a", "new": "b"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Default OFF behaves byte-for-byte like the wrapped human: every call is
// delegated, the verdict propagates, and nothing is flagged for audit (§8.1).
func TestDisabled_DelegatesEverythingToHuman(t *testing.T) {
	for _, v := range []agent.Verdict{agent.VerdictAllow, agent.VerdictDeny} {
		h := &fakeHuman{v: v}
		a := NewAutoApprover(t.TempDir(), h, false)

		got, reason := a.ApproveAudited("edit_file", editInput(t, "main.go"))
		if got != v {
			t.Fatalf("disabled: got %v, want human verdict %v", got, v)
		}
		if reason != "" {
			t.Fatalf("disabled: audit reason %q, want empty (human decided)", reason)
		}
		if h.calls != 1 {
			t.Fatalf("disabled: human consulted %d times, want 1", h.calls)
		}
	}
}

// Enabled: an in-workspace edit_file / create_file auto-approves without asking
// the human and returns a non-empty audit reason for the loop to emit (§8.2).
func TestEnabled_InWorkspaceWriteAutoApprovedWithReason(t *testing.T) {
	for _, tool := range []string{"edit_file", "create_file"} {
		h := &fakeHuman{v: agent.VerdictDeny} // would DENY if consulted — proves it wasn't
		a := NewAutoApprover(t.TempDir(), h, true)

		approved, reason := a.ApproveAudited(tool, editInput(t, "pkg/sub/file.go"))
		if approved != agent.VerdictAllow {
			t.Fatalf("%s: in-workspace write should auto-approve", tool)
		}
		if reason == "" {
			t.Fatalf("%s: auto-grant must carry an audit reason", tool)
		}
		if h.calls != 0 {
			t.Fatalf("%s: human consulted %d times, want 0 (auto)", tool, h.calls)
		}
	}
}

// Enabled but the target escapes the workspace → fail-safe to human, no audit
// reason (§8.4: out-of-workspace path always confirmed).
func TestEnabled_OutOfWorkspaceWriteFallsBackToHuman(t *testing.T) {
	h := &fakeHuman{v: agent.VerdictAllow}
	a := NewAutoApprover(t.TempDir(), h, true)

	approved, reason := a.ApproveAudited("edit_file", editInput(t, "../../../etc/passwd"))
	if approved != agent.VerdictAllow {
		t.Fatal("escaping edit should defer to human, who approved here")
	}
	if reason != "" {
		t.Fatalf("escaping edit: audit reason %q, want empty (human, not auto)", reason)
	}
	if h.calls != 1 {
		t.Fatalf("escaping edit: human consulted %d times, want 1", h.calls)
	}
}

// Enabled: tools that stay human in Phase 1 (all commands, apply_patch,
// git_commit) are never auto-approved — they fall back to the human with no audit
// reason (§8.3/§8.4, ADR-9).
func TestEnabled_NeverAutoToolsFallBackToHuman(t *testing.T) {
	cases := []struct {
		tool  string
		input json.RawMessage
	}{
		{"run_command", json.RawMessage(`{"command":"go test ./..."}`)},
		{"run_command", json.RawMessage(`{"command":"rm -rf build"}`)},
		{"apply_patch", json.RawMessage(`{"patch":"--- a/x\n+++ b/x\n"}`)},
		{"git_commit", json.RawMessage(`{"message":"wip"}`)},
	}
	for _, c := range cases {
		h := &fakeHuman{v: agent.VerdictDeny}
		a := NewAutoApprover(t.TempDir(), h, true)

		approved, reason := a.ApproveAudited(c.tool, c.input)
		if approved != agent.VerdictDeny {
			t.Fatalf("%s should never auto-approve in Phase 1", c.tool)
		}
		if reason != "" {
			t.Fatalf("%s: audit reason %q, want empty", c.tool, reason)
		}
		if h.calls != 1 {
			t.Fatalf("%s: human consulted %d times, want 1", c.tool, h.calls)
		}
	}
}

// The kill switch: flipping enabled off mid-session reverts to human at the next
// call (§8.6 — effect at the next tool boundary).
func TestSetEnabled_KillSwitchRevertsToHuman(t *testing.T) {
	h := &fakeHuman{v: agent.VerdictDeny}
	a := NewAutoApprover(t.TempDir(), h, true)

	if approved, _ := a.ApproveAudited("edit_file", editInput(t, "a.go")); approved != agent.VerdictAllow {
		t.Fatal("enabled: in-workspace edit should auto-approve")
	}
	a.SetEnabled(false)
	if a.Enabled() {
		t.Fatal("Enabled() should report false after SetEnabled(false)")
	}
	if approved, _ := a.ApproveAudited("edit_file", editInput(t, "a.go")); approved != agent.VerdictDeny {
		t.Fatal("after kill switch: edit should defer to human (who denies here)")
	}
	if h.calls != 1 {
		t.Fatalf("after kill switch: human consulted %d times, want 1", h.calls)
	}
}

// Malformed / empty input cannot be classified as safe → fail-safe to human.
func TestEnabled_UnparseableInputFallsBackToHuman(t *testing.T) {
	for _, in := range []json.RawMessage{nil, json.RawMessage(`{`), json.RawMessage(`{"path":""}`)} {
		h := &fakeHuman{v: agent.VerdictAllow}
		a := NewAutoApprover(t.TempDir(), h, true)
		if _, reason := a.ApproveAudited("edit_file", in); reason != "" {
			t.Fatalf("input %q: reason %q, want empty (fail-safe)", string(in), reason)
		}
		if h.calls != 1 {
			t.Fatalf("input %q: human consulted %d times, want 1 (fail-safe)", string(in), h.calls)
		}
	}
}

// Approve (the verdict-only Approver method) agrees with ApproveAudited.
func TestApprove_MatchesApproveAudited(t *testing.T) {
	a := NewAutoApprover(t.TempDir(), &fakeHuman{v: agent.VerdictDeny}, true)
	if a.Approve("edit_file", editInput(t, "x.go")) != agent.VerdictAllow {
		t.Fatal("Approve should auto-grant an in-workspace edit")
	}
}

// The workspace root is absolutized so a relative root still classifies correctly.
func TestNewAutoApprover_RelativeRootAbsolutized(t *testing.T) {
	a := NewAutoApprover(".", &fakeHuman{}, true)
	if !filepath.IsAbs(a.root) {
		t.Fatalf("root %q should be absolute", a.root)
	}
}

// Protected paths (P4): even in-workspace writes to .env, credentials, etc.
// must NOT be auto-approved — the human must explicitly confirm.
func TestAutoApprove_ProtectedPathRequiresHuman(t *testing.T) {
	protected := []string{".env", ".env.production", "credentials", "private.key", "secrets"}
	for _, path := range protected {
		h := &fakeHuman{v: agent.VerdictDeny} // would deny if consulted
		a := NewAutoApprover(t.TempDir(), h, true)

		approved, reason := a.ApproveAudited("edit_file", editInput(t, path))
		if approved != agent.VerdictDeny {
			t.Errorf("protected path %q: should NOT auto-approve, got verdict=%v reason=%q", path, approved, reason)
		}
		if h.calls != 1 {
			t.Errorf("protected path %q: human should have been consulted, calls=%d", path, h.calls)
		}
	}
}

// Normal paths still auto-approve in workspace.
func TestAutoApprove_NormalPathStillAutoApproves(t *testing.T) {
	h := &fakeHuman{v: agent.VerdictDeny}
	a := NewAutoApprover(t.TempDir(), h, true)

	approved, _ := a.ApproveAudited("edit_file", editInput(t, "main.go"))
	if approved != agent.VerdictAllow {
		t.Fatal("normal path should still auto-approve")
	}
}
