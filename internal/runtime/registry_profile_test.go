package runtime

import (
	"code-agent/internal/app"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"testing"
)

// registerForProfile builds a registry with just the built-in tools for the given
// profile and returns the set of registered tool names.
func registerForProfile(t *testing.T, profile app.Profile) map[string]bool {
	t.Helper()
	skillReg, err := skills.Load("", t.TempDir()) // empty dir => empty skills registry
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	reg := tools.NewRegistry()
	cfg := app.Config{Profile: profile}
	if err := RegisterBuiltinTools(reg, cfg, skillReg); err != nil {
		t.Fatalf("RegisterBuiltinTools(%q): %v", profile, err)
	}
	got := map[string]bool{}
	for _, n := range reg.Names() {
		got[n] = true
	}
	return got
}

// registerWithAllowlist builds a registry under the full profile but with an
// AgentConfig.BuiltinTools allowlist, returning the set of registered tool names.
func registerWithAllowlist(t *testing.T, allow []string) map[string]bool {
	t.Helper()
	skillReg, err := skills.Load("", t.TempDir())
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	reg := tools.NewRegistry()
	cfg := app.Config{Agent: app.AgentConfig{BuiltinTools: &allow}}
	if err := RegisterBuiltinTools(reg, cfg, skillReg); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}
	got := map[string]bool{}
	for _, n := range reg.Names() {
		got[n] = true
	}
	return got
}

// TestRegisterBuiltinTools_Allowlist locks down a deployment (e.g. DreamAI sidecar):
// a non-nil BuiltinTools allowlist must register ONLY the named tools and drop the
// dangerous server-side shell/filesystem tools.
func TestRegisterBuiltinTools_Allowlist(t *testing.T) {
	dangerous := []string{"run_command", "read_file", "create_file", "edit_file", "list_files", "grep"}

	// Allow only web_search: every dangerous tool must be absent, web_search present.
	got := registerWithAllowlist(t, []string{"web_search"})
	if !got["web_search"] {
		t.Error("allowlisted web_search must be registered")
	}
	for _, name := range dangerous {
		if got[name] {
			t.Errorf("allowlist [web_search]: dangerous tool %q must NOT be registered", name)
		}
	}

	// Empty allowlist: nothing registers.
	none := registerWithAllowlist(t, []string{})
	if len(none) != 0 {
		t.Errorf("empty allowlist: expected zero tools, got %v", none)
	}

	// Nil (unset) allowlist: behaves as before — read_file present (no restriction).
	full := registerForProfile(t, app.ProfileFull) // cfg with nil BuiltinTools
	if !full["read_file"] {
		t.Error("nil allowlist (default): read_file must be registered")
	}
}

func TestRegisterBuiltinTools_SandboxedExcludesSubprocessTools(t *testing.T) {
	// Tools that shell out and have no pure-Go replacement yet — must be absent under
	// the sandboxed (iOS) profile.
	subprocessTools := []string{
		"run_command", "job_status", "job_logs", "job_cancel",
		"project_graph",
	}
	// Pure-Go tools — must be present under every profile. git_commit / git_diff /
	// apply_patch are here because the sandboxed profile registers go-git-backed
	// implementations of them.
	pureGoTools := []string{
		"list_files", "read_file", "create_file", "edit_file",
		"grep", "load_skill", "todo_write", "git_commit", "git_diff", "apply_patch",
	}

	full := registerForProfile(t, app.ProfileFull)
	sandboxed := registerForProfile(t, app.ProfileSandboxed)

	for _, name := range subprocessTools {
		if !full[name] {
			t.Errorf("full profile: expected subprocess tool %q to be registered", name)
		}
		if sandboxed[name] {
			t.Errorf("sandboxed profile: subprocess tool %q must NOT be registered", name)
		}
	}
	for _, name := range pureGoTools {
		if !sandboxed[name] {
			t.Errorf("sandboxed profile: pure-Go tool %q must be registered", name)
		}
		if !full[name] {
			t.Errorf("full profile: tool %q must be registered", name)
		}
	}

	// Read-only git tools that exist ONLY on the sandboxed profile — desktop reaches
	// the same information through the shell, so they are not added there.
	sandboxedOnlyTools := []string{"git_init", "git_clone", "git_status", "git_log"}
	for _, name := range sandboxedOnlyTools {
		if !sandboxed[name] {
			t.Errorf("sandboxed profile: expected %q to be registered", name)
		}
		if full[name] {
			t.Errorf("full profile: %q must NOT be registered (desktop uses the shell)", name)
		}
	}
}
