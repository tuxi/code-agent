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
	skillReg, err := skills.Load(t.TempDir()) // empty dir => empty skills registry
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
