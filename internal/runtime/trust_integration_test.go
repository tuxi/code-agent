package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/settings"
)

// TestLoadSettingsWithTrustNoResources verifies that even a directory without
// .codeagent/ resources requires trust — trust is now universal.
func TestLoadSettingsWithTrustNoResources(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// No policy → fail-closed (headless mode).
	set, trusted := LoadSettingsWithTrust(t.Context(), root, home, nil, nil, os.Stderr)
	if trusted {
		t.Fatal("directory without policy should fail-closed")
	}
	_ = set

	// With an allow policy → trusted.
	allowPolicy := TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
		return agent.TrustAllowed, nil
	})
	set2, trusted2 := LoadSettingsWithTrust(t.Context(), root, home, nil, allowPolicy, os.Stderr)
	if !trusted2 {
		t.Fatal("directory with allow policy should be trusted")
	}
	_ = set2
}

// TestLoadSettingsWithTrustHasResourcesNoOverride verifies that a directory
// with trust-requiring resources prompts (via policy) and that the policy
// controls whether project settings are loaded.
func TestLoadSettingsWithTrustHasResourcesDenied(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// Create a project settings file.
	settingsDir := filepath.Join(root, ".codeagent")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"verify":{"command":"go test ./..."}}`), 0o644)

	// Policy that always denies.
	denyPolicy := TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
		return agent.TrustDenied, nil
	})

	set, trusted := LoadSettingsWithTrust(t.Context(), root, home, nil, denyPolicy, os.Stderr)
	if trusted {
		t.Fatal("deny policy should result in untrusted")
	}
	// Project settings should NOT be merged — verify command should be nil/empty.
	if set.Verify != nil && set.Verify.Command != "" {
		t.Fatalf("project verify command should not be loaded when untrusted, got %q", set.Verify.Command)
	}
}

// TestLoadSettingsWithTrustHasResourcesAllowed verifies that project settings
// ARE merged when the policy allows.
func TestLoadSettingsWithTrustHasResourcesAllowed(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// Create a project settings file with a verify command.
	settingsDir := filepath.Join(root, ".codeagent")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"verify":{"command":"go test ./..."}}`), 0o644)

	// Policy that always allows.
	allowPolicy := TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
		return agent.TrustAllowed, nil
	})

	set, trusted := LoadSettingsWithTrust(t.Context(), root, home, nil, allowPolicy, os.Stderr)
	if !trusted {
		t.Fatal("allow policy should result in trusted")
	}
	if set.Verify.Command != "go test ./..." {
		t.Fatalf("project verify command should be loaded when trusted, got %q", set.Verify.Command)
	}
}

// TestLoadSettingsWithTrustCLIOverride verifies that --trust bypasses the policy.
func TestLoadSettingsWithTrustCLIOverride(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// Create a project settings file.
	settingsDir := filepath.Join(root, ".codeagent")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"verify":{"command":"go test ./..."}}`), 0o644)

	// Policy that would deny, but CLI override wins.
	denyPolicy := TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
		return agent.TrustDenied, nil
	})

	trustFlag := true
	set, trusted := LoadSettingsWithTrust(t.Context(), root, home, &trustFlag, denyPolicy, os.Stderr)
	if !trusted {
		t.Fatal("--trust CLI flag should override deny policy")
	}
	if set.Verify.Command != "go test ./..." {
		t.Fatalf("project settings should be loaded with --trust, got %q", set.Verify.Command)
	}
}

// TestLoadSettingsWithTrustCLINoTrust verifies that --no-trust denies even
// with an allow policy.
func TestLoadSettingsWithTrustCLINoTrust(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	settingsDir := filepath.Join(root, ".codeagent")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"verify":{"command":"go test ./..."}}`), 0o644)

	allowPolicy := TrustPolicyFunc(func(ctx context.Context, cwd string, hasResources bool) (agent.TrustDecision, error) {
		return agent.TrustAllowed, nil
	})

	noTrustFlag := false
	_, trusted := LoadSettingsWithTrust(t.Context(), root, home, &noTrustFlag, allowPolicy, os.Stderr)
	if trusted {
		t.Fatal("--no-trust CLI flag should override allow policy")
	}
}

// TestTrustStorePersistenceAfterDecision verifies that after a trust decision
// is made via TerminalTrustPolicy, subsequent loads use the persisted decision.
func TestTrustStorePersistenceAfterDecision(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// Create project resources.
	os.MkdirAll(filepath.Join(root, ".codeagent"), 0o755)
	os.WriteFile(filepath.Join(root, ".codeagent", "settings.json"), []byte(`{}`), 0o644)

	// Step 1: Persist a trust decision via the store.
	ts, _ := LoadTrustStore(home)
	if err := ts.Store(root, true); err != nil {
		t.Fatalf("store trust: %v", err)
	}

	// Step 2: Load settings without a policy — should use the persisted decision.
	set, trusted := LoadSettingsWithTrust(t.Context(), root, home, nil, nil, os.Stderr)
	if !trusted {
		t.Fatal("persisted trust decision should be used when no policy is given")
	}
	_ = set
}

// TestExtractTrustFlag verifies CLI flag parsing.
func TestExtractTrustFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantVal  *bool
		wantRest []string
	}{
		{"no flag", []string{"run", "hello"}, nil, []string{"run", "hello"}},
		{"--trust", []string{"--trust", "run", "hello"}, boolPtr(true), []string{"run", "hello"}},
		{"--no-trust", []string{"--no-trust", "run"}, boolPtr(false), []string{"run"}},
		{"--trust at end", []string{"run", "--trust"}, boolPtr(true), []string{"run"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, rest := ExtractTrustFlag(tt.args)
			if tt.wantVal == nil {
				if val != nil {
					t.Fatalf("ExtractTrustFlag() = %v, want nil", *val)
				}
			} else {
				if val == nil || *val != *tt.wantVal {
					t.Fatalf("ExtractTrustFlag() = %v, want %v", val, tt.wantVal)
				}
			}
			if !stringSlicesEqual(rest, tt.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLoadSettingsWithTrustFullChain tests the complete resolution chain
// integration: no CLI flag, no hooks, no store, no policy = fail-closed.
func TestLoadSettingsWithTrustFullChainFailClosed(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	os.MkdirAll(filepath.Join(root, ".codeagent"), 0o755)
	os.WriteFile(filepath.Join(root, ".codeagent", "settings.json"), []byte(`{"verify":{"command":"go test ./..."}}`), 0o644)

	// No CLI override, no hooks, no store, no policy: fail-closed.
	set, trusted := LoadSettingsWithTrust(t.Context(), root, home, nil, nil, nil)
	if trusted {
		t.Fatal("fail-closed: should not trust without any decision source")
	}
	_ = set
}

// TestSettingsLoadBackwardCompatible verifies that settings.Load() still works
// for callers that haven't migrated yet (backward compatibility).
func TestSettingsLoadBackwardCompatible(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	_ = settings.LoadUserOnly(home, os.Stderr)
	_ = settings.LoadProjectSettings(root, os.Stderr)
	base := settings.LoadUserOnly(home, os.Stderr)
	overlay := settings.LoadProjectSettings(root, os.Stderr)
	settings.MergeSettings(&base, overlay)
	// settings.Load() should still work unchanged.
	s := settings.Load(root, home, os.Stderr)
	_ = s
}

// TestResolveTrustFullChain verifies every link in the chain.
func TestResolveTrustFullChain(t *testing.T) {
	dir := t.TempDir()
	ts := &TrustStore{path: TrustStorePath(dir), cache: map[string]*bool{}}

	t.Run("empty chain fail-closed", func(t *testing.T) {
		trusted, reason, _ := ResolveTrust(t.Context(), dir, true, nil, nil, ts, nil, nil)
		if trusted {
			t.Fatalf("(%v, %q) — empty chain should fail-closed", trusted, reason)
		}
	})

	t.Run("CLI --trust wins", func(t *testing.T) {
		flag := true
		trusted, _, _ := ResolveTrust(t.Context(), dir, true, &flag, nil, ts, nil, nil)
		if !trusted {
			t.Fatal("--trust should win")
		}
	})

	t.Run("persisted store wins when no CLI", func(t *testing.T) {
		trusted := true
		ts.cache[dir] = &trusted
		result, _, _ := ResolveTrust(t.Context(), dir, true, nil, nil, ts, nil, nil)
		if !result {
			t.Fatal("persisted trust should win")
		}
		delete(ts.cache, dir)
	})

	t.Run("default never trust", func(t *testing.T) {
		def := false
		result, reason, _ := ResolveTrust(t.Context(), dir, true, nil, nil, ts, &def, nil)
		if result || !strings.Contains(reason, "never trust") {
			t.Fatalf("(%v, %q) — want never trust", result, reason)
		}
	})
}
