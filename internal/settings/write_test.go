package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SetVerifyCommand writes a verify block that Load then reads back, and preserves
// unrelated keys already in the file.
func TestSetVerifyCommandRoundTripsAndPreserves(t *testing.T) {
	root := t.TempDir()
	path := ProjectLocalPath(root)
	// Seed an existing file with an unrelated key + a permissions block.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"theme":"dark","permissions":{"allow":["x"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetVerifyCommand(path, "go test ./..."); err != nil {
		t.Fatalf("SetVerifyCommand: %v", err)
	}

	// Load resolves it.
	if got := ResolveVerify(root, t.TempDir(), ""); got != "go test ./..." {
		t.Errorf("ResolveVerify = %q, want %q", got, "go test ./...")
	}
	// Unrelated key + permissions survive.
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, `"theme": "dark"`) {
		t.Errorf("unrelated key clobbered: %s", s)
	}
	if !strings.Contains(s, `"x"`) {
		t.Errorf("permissions clobbered: %s", s)
	}
}

// AddAllowRule is idempotent and keeps other keys.
func TestAddAllowRuleIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	for i := 0; i < 3; i++ {
		if err := AddAllowRule(path, "run_command"); err != nil {
			t.Fatalf("AddAllowRule: %v", err)
		}
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Permissions.Allow) != 1 || f.Permissions.Allow[0] != "run_command" {
		t.Errorf("allow = %v, want exactly [run_command]", f.Permissions.Allow)
	}
}

// EnsureGitignored appends once and is idempotent.
func TestEnsureGitignored(t *testing.T) {
	root := t.TempDir()
	entry := ".codeagent/settings.local.json"
	for i := 0; i < 2; i++ {
		if err := EnsureGitignored(root, entry); err != nil {
			t.Fatalf("EnsureGitignored: %v", err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if n := strings.Count(string(data), entry); n != 1 {
		t.Errorf("entry appears %d times, want exactly 1: %q", n, string(data))
	}

	// Existing content without a trailing newline is not glued onto.
	root2 := t.TempDir()
	gi := filepath.Join(root2, ".gitignore")
	if err := os.WriteFile(gi, []byte("node_modules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignored(root2, entry); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(gi)
	if !strings.Contains(string(data2), "node_modules\n"+entry) {
		t.Errorf("entry glued onto existing content: %q", string(data2))
	}
}

// ParseJSON is the in-memory injection path for embedded hosts.
func TestParseJSON(t *testing.T) {
	f, err := ParseJSON([]byte(`{"permissions":{"allow":["a"]},"verify":{"command":"go build ./..."}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Permissions.Allow) != 1 || f.Permissions.Allow[0] != "a" {
		t.Errorf("allow = %v", f.Permissions.Allow)
	}
	if f.Verify == nil || f.Verify.Command != "go build ./..." {
		t.Errorf("verify = %+v", f.Verify)
	}
	if _, err := ParseJSON([]byte("not json")); err == nil {
		t.Error("malformed input should error")
	}
	if f2, err := ParseJSON(nil); err != nil || f2.Verify != nil {
		t.Errorf("empty input should be a zero File, got %+v err %v", f2, err)
	}
}
