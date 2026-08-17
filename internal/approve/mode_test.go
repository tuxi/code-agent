package approve

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/settings"
)

// recordingPlanApprover records whether it was consulted.
type recordingPlanApprover struct {
	called bool
}

func (r *recordingPlanApprover) ApprovePlan(agent.Plan) agent.PlanDecision {
	r.called = true
	return agent.PlanApproved
}

// recordingPathApprover records whether it was consulted.
type recordingPathApprover struct {
	called bool
}

func (r *recordingPathApprover) ApproveExternalPath(string, string) bool {
	r.called = true
	return true
}

func modeApprover(t *testing.T, mode Mode) (*ModeApprover, *recordingApprover) {
	t.Helper()
	human := &recordingApprover{verdict: agent.VerdictAllow}
	root := t.TempDir()
	return NewModeApprover(mode, root, human).WithPlanApprover(&recordingPlanApprover{}), human
}

func TestModeFromSettings(t *testing.T) {
	cases := []struct {
		raw  string
		want Mode
	}{
		{"", ModeAsk},
		{"ask", ModeAsk},
		{"auto", ModeAuto},
		{"full", ModeFull},
		{"bogus", ModeAsk},
	}
	for _, c := range cases {
		if got := ModeFromSettings(settings.Settings{ApprovalMode: c.raw}); got != c.want {
			t.Errorf("ModeFromSettings(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestAskDelegatesEverything(t *testing.T) {
	ma, human := modeApprover(t, ModeAsk)
	if v, reason := ma.ApproveAudited("run_command", json.RawMessage(`{"command":"rm -rf build"}`)); v != agent.VerdictAllow || reason != "" {
		t.Fatalf("ask: run_command verdict=%v reason=%q, want allow with no audit (delegated)", v, reason)
	}
	if !human.called {
		t.Fatal("ask: human was not consulted")
	}
	// Plan approval delegates too.
	human.called = false
	if ma.ApprovePlan(agent.Plan{}) != agent.PlanApproved {
		t.Fatal("ask: plan should delegate to the human plan approver")
	}
	// External path delegates.
	human.called = false
	if !ma.ApproveExternalPath("/outside/file.txt", "read") {
		t.Fatal("ask: external path should delegate to the human path approver")
	}
}

func TestAutoInWorkspaceCommandAutoApproves(t *testing.T) {
	ma, human := modeApprover(t, ModeAuto)
	root := t.TempDir()
	ma.root = root
	// In-workspace non-network command: auto.
	if v, reason := ma.ApproveAudited("run_command", json.RawMessage(`{"command":"go build ./..."}`)); v != agent.VerdictAllow || reason == "" {
		t.Fatalf("auto: in-workspace command verdict=%v reason=%q, want allow with audit", v, reason)
	}
	if human.called {
		t.Fatal("auto: human consulted for an in-workspace command")
	}
	// Network command: delegates.
	human.called = false
	if v, _ := ma.ApproveAudited("run_command", json.RawMessage(`{"command":"curl https://example.com"}`)); v != agent.VerdictAllow {
		t.Fatalf("auto: network command verdict=%v, want delegated (human allow)", v)
	}
	if !human.called {
		t.Fatal("auto: network command did not reach the human")
	}
}

func TestAutoInWorkspaceEditAutoApproves(t *testing.T) {
	ma, human := modeApprover(t, ModeAuto)
	root := t.TempDir()
	ma.root = root
	if v, reason := ma.ApproveAudited("edit_file", json.RawMessage(`{"path":"main.go"}`)); v != agent.VerdictAllow || reason == "" {
		t.Fatalf("auto: in-workspace edit verdict=%v reason=%q, want allow with audit", v, reason)
	}
	if human.called {
		t.Fatal("auto: human consulted for an in-workspace edit")
	}
	// Out-of-workspace edit: delegates.
	human.called = false
	if v, _ := ma.ApproveAudited("edit_file", json.RawMessage(`{"path":"../outside.go"}`)); v != agent.VerdictAllow {
		t.Fatalf("auto: out-of-workspace edit verdict=%v, want delegated", v)
	}
	if !human.called {
		t.Fatal("auto: out-of-workspace edit did not reach the human")
	}
	// Protected path: delegates even inside the workspace.
	human.called = false
	if v, _ := ma.ApproveAudited("edit_file", json.RawMessage(`{"path":".env"}`)); v != agent.VerdictAllow {
		t.Fatalf("auto: protected edit verdict=%v, want delegated", v)
	}
	if !human.called {
		t.Fatal("auto: protected edit did not reach the human")
	}
}

func TestAutoMCPDelegates(t *testing.T) {
	ma, human := modeApprover(t, ModeAuto)
	if v, _ := ma.ApproveAudited("mcp__github__list_issues", nil); v != agent.VerdictAllow {
		t.Fatalf("auto: MCP tool verdict=%v, want delegated", v)
	}
	if !human.called {
		t.Fatal("auto: MCP tool did not reach the human")
	}
}

func TestAutoPlanApproves(t *testing.T) {
	ma, human := modeApprover(t, ModeAuto)
	if ma.ApprovePlan(agent.Plan{}) != agent.PlanApproved {
		t.Fatal("auto: plan should be auto-approved")
	}
	if human.called {
		t.Fatal("auto: plan approval consulted the human")
	}
}

func TestFullApprovesEverythingButProtected(t *testing.T) {
	ma, human := modeApprover(t, ModeFull)
	// Any side-effecting tool auto-runs.
	if v, reason := ma.ApproveAudited("run_command", json.RawMessage(`{"command":"curl https://example.com"}`)); v != agent.VerdictAllow || reason == "" {
		t.Fatalf("full: network command verdict=%v reason=%q, want allow with audit", v, reason)
	}
	if human.called {
		t.Fatal("full: human consulted for a network command")
	}
	// MCP tools auto-run.
	human.called = false
	if v, _ := ma.ApproveAudited("mcp__github__list_issues", nil); v != agent.VerdictAllow {
		t.Fatalf("full: MCP tool verdict=%v, want allow", v)
	}
	if human.called {
		t.Fatal("full: human consulted for an MCP tool")
	}
	// External path auto-allows.
	if !ma.ApproveExternalPath("/outside/file.txt", "read") {
		t.Fatal("full: external path should auto-allow")
	}
	// Protected path write still delegates to the human.
	human.called = false
	if v, _ := ma.ApproveAudited("edit_file", json.RawMessage(`{"path":".env"}`)); v != agent.VerdictAllow {
		t.Fatalf("full: protected edit verdict=%v, want delegated", v)
	}
	if !human.called {
		t.Fatal("full: protected edit did not reach the human")
	}
	// Protected external path is denied (never auto-exposed).
	if ma.ApproveExternalPath("/home/user/.env", "read") {
		t.Fatal("full: protected external path must not auto-allow")
	}
}

func TestModeFromUnwrapsAllowlist(t *testing.T) {
	ma, _ := modeApprover(t, ModeAuto)
	wrapped := Allowlisted(newTestStore(t, nil, nil), ma)
	got, ok := ModeFrom(wrapped)
	if !ok || got != ma {
		t.Fatalf("ModeFrom(Allowlist(ModeApprover)) = %v, %v; want the ModeApprover", got, ok)
	}
}

func TestModeFromSettingsRoundTrip(t *testing.T) {
	// A settings view with a local-layer override wins over the base.
	base := settings.Settings{ApprovalMode: "ask"}
	overlay := settings.Settings{ApprovalMode: "full"}
	settings.MergeSettings(&base, overlay)
	if got := ModeFromSettings(base); got != ModeFull {
		t.Fatalf("merged approval mode = %q, want full", got)
	}
}

func TestInsideRoot(t *testing.T) {
	root := t.TempDir()
	if !insideRoot(root, "main.go") {
		t.Fatal("insideRoot: relative file inside root should be true")
	}
	if insideRoot(root, filepath.Join("..", "outside.go")) {
		t.Fatal("insideRoot: parent-relative path should be false")
	}
	if insideRoot("", "main.go") {
		t.Fatal("insideRoot: empty root should be false")
	}
}