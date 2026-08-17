package settings

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The three layers all contribute their permissions as a union, deduped.
func TestLoadUnionsAllLayers(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	writeJSON(t, UserPath(home), `{"permissions":{"allow":["user__a","shared_dup"]}}`)
	writeJSON(t, ProjectSharedPath(root), `{"permissions":{"allow":["shared__a","shared_dup"],"deny":["deny__x"]}}`)
	writeJSON(t, ProjectLocalPath(root), `{"permissions":{"allow":["local__a"]}}`)

	s := Load(root, home, &bytes.Buffer{})

	wantAllow := map[string]bool{"user__a": true, "shared__a": true, "shared_dup": true, "local__a": true}
	if len(s.Permissions.Allow) != len(wantAllow) {
		t.Fatalf("allow = %v, want %d unique entries", s.Permissions.Allow, len(wantAllow))
	}
	for _, a := range s.Permissions.Allow {
		if !wantAllow[a] {
			t.Errorf("unexpected allow entry %q", a)
		}
	}
	if len(s.Permissions.Deny) != 1 || s.Permissions.Deny[0] != "deny__x" {
		t.Errorf("deny = %v, want [deny__x]", s.Permissions.Deny)
	}
}

// A missing file is normal — no error, no output, just skipped.
func TestLoadMissingFilesAreSilent(t *testing.T) {
	var warn bytes.Buffer
	s := Load(t.TempDir(), t.TempDir(), &warn)
	if len(s.Permissions.Allow) != 0 || len(s.Permissions.Deny) != 0 {
		t.Errorf("expected empty settings, got %+v", s)
	}
	if warn.Len() != 0 {
		t.Errorf("missing files must not warn, got %q", warn.String())
	}
}

// The first load of a missing user settings file creates the template and
// reports Bootstrapped; a second load finds the file and clears the flag.
func TestLoadBootstrapsFirstRun(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	first := Load(root, home, &bytes.Buffer{})
	if !first.Bootstrapped {
		t.Fatal("first load of a missing user file must report Bootstrapped")
	}
	if _, err := os.Stat(UserPath(home)); err != nil {
		t.Fatalf("first load must create the settings template: %v", err)
	}

	second := Load(root, home, &bytes.Buffer{})
	if second.Bootstrapped {
		t.Fatal("second load must not report Bootstrapped (file now exists)")
	}
}

// A malformed file is logged and skipped — it must not brick loading, and the
// other layers still contribute.
func TestLoadMalformedIsLoggedAndSkipped(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeJSON(t, ProjectSharedPath(root), `{ this is not json `)
	writeJSON(t, ProjectLocalPath(root), `{"permissions":{"allow":["local__a"]}}`)

	var warn bytes.Buffer
	s := Load(root, home, &warn)

	if len(s.Permissions.Allow) != 1 || s.Permissions.Allow[0] != "local__a" {
		t.Errorf("allow = %v, want [local__a] (malformed shared skipped)", s.Permissions.Allow)
	}
	if !strings.Contains(warn.String(), "ignoring malformed") {
		t.Errorf("expected a malformed-file warning, got %q", warn.String())
	}
}

// Empty root/home disable those layers' files entirely (no path built).
func TestPathsEmptyRootHomeDisabled(t *testing.T) {
	if UserPath("") != "" {
		t.Error("empty home must disable the user path")
	}
	if ProjectSharedPath("") != "" || ProjectLocalPath("") != "" {
		t.Error("empty root must disable the project paths")
	}
}

// Verify merges as an OVERRIDE: the highest layer that sets a block wins.
func TestVerifyOverrideHighestLayerWins(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	writeJSON(t, UserPath(home), `{"verify":{"command":"user cmd"}}`)
	writeJSON(t, ProjectSharedPath(root), `{"verify":{"command":"shared cmd"}}`)
	writeJSON(t, ProjectLocalPath(root), `{"verify":{"command":"local cmd"}}`)

	if got := ResolveVerify(root, home, "legacy cmd"); got != "local cmd" {
		t.Errorf("ResolveVerify = %q, want %q (project-local wins)", got, "local cmd")
	}
}

// A verify block anywhere overrides the config.yaml legacy value; absent
// everywhere falls back to legacy; both absent → OFF (decision 3).
func TestVerifyLegacyFallbackAndOff(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	// No settings files → legacy config value is used.
	if got := ResolveVerify(root, home, "go test ./..."); got != "go test ./..." {
		t.Errorf("legacy fallback = %q, want %q", got, "go test ./...")
	}
	// Nothing configured anywhere → disabled (auto-detect is NOT the default).
	if got := ResolveVerify(root, home, ""); got != "" {
		t.Errorf("nothing configured = %q, want empty (OFF)", got)
	}
	// A shared verify block overrides legacy.
	writeJSON(t, ProjectSharedPath(root), `{"verify":{"command":"cargo test"}}`)
	if got := ResolveVerify(root, home, "go test ./..."); got != "cargo test" {
		t.Errorf("settings override = %q, want %q", got, "cargo test")
	}
}

// enabled:false and disable words turn verify off even with a command present.
func TestVerifyDisable(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeJSON(t, ProjectLocalPath(root), `{"verify":{"command":"go test ./...","enabled":false}}`)
	if got := ResolveVerify(root, home, "legacy"); got != "" {
		t.Errorf("enabled:false = %q, want empty", got)
	}

	root2 := t.TempDir()
	writeJSON(t, ProjectLocalPath(root2), `{"verify":{"command":"off"}}`)
	if got := ResolveVerify(root2, home, "legacy"); got != "" {
		t.Errorf("command 'off' = %q, want empty", got)
	}
}

// "auto" detects a cheap build command from the workspace markers (§8).
func TestVerifyAutoDetect(t *testing.T) {
	cases := []struct {
		marker, body, want string
	}{
		{"go.mod", "module x", "go build ./..."},
		{"Cargo.toml", "[package]", "cargo build"},
		{"Package.swift", "// swift", "swift build"},
		{"package.json", `{"scripts":{"build":"tsc"}}`, "npm run build"},
		{"package.json", `{"scripts":{"test":"jest"}}`, "npm test"},
	}
	for _, c := range cases {
		root := t.TempDir()
		writeJSON(t, filepath.Join(root, c.marker), c.body)
		if got := DetectVerify(root); got != c.want {
			t.Errorf("DetectVerify with %s = %q, want %q", c.marker, got, c.want)
		}
		// And via ResolveVerify with command "auto".
		writeJSON(t, ProjectLocalPath(root), `{"verify":{"command":"auto"}}`)
		if got := ResolveVerify(root, t.TempDir(), ""); got != c.want {
			t.Errorf("ResolveVerify(auto) with %s = %q, want %q", c.marker, got, c.want)
		}
	}
	// Nothing recognizable → empty.
	if got := DetectVerify(t.TempDir()); got != "" {
		t.Errorf("DetectVerify on empty dir = %q, want empty", got)
	}
}

// Hooks CONCATENATE across layers, in layer order (user → shared → local), and
// parse the {event,match,command} JSON shape.
func TestHooksConcatenateAcrossLayers(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	writeJSON(t, UserPath(home), `{"hooks":[{"event":"pre_tool_use","match":"*","command":"u"}]}`)
	writeJSON(t, ProjectSharedPath(root), `{"hooks":[{"event":"post_tool_use","match":"edit_file","command":"s"}]}`)
	writeJSON(t, ProjectLocalPath(root), `{"hooks":[{"event":"post_tool_use","match":"*","command":"l"}]}`)

	s := Load(root, home, &bytes.Buffer{})
	if len(s.Hooks) != 3 {
		t.Fatalf("got %d hooks, want 3 (all layers concatenate): %+v", len(s.Hooks), s.Hooks)
	}
	// Layer order: user, shared, local.
	if s.Hooks[0].Command != "u" || s.Hooks[1].Command != "s" || s.Hooks[2].Command != "l" {
		t.Errorf("hook order = %q/%q/%q, want u/s/l", s.Hooks[0].Command, s.Hooks[1].Command, s.Hooks[2].Command)
	}
	// The JSON shape binds to the hooks.Hook fields.
	if s.Hooks[1].Event != "post_tool_use" || s.Hooks[1].Match != "edit_file" {
		t.Errorf("shared hook parsed as %+v, want event=post_tool_use match=edit_file", s.Hooks[1])
	}
}

// A real permissions-only settings.local.json (no verify/hooks blocks the newer
// phases added) must load cleanly and be fully backward compatible: permissions
// apply, verify stays OFF, hooks are none. This is the exact file a user reported.
func TestPermissionsOnlyFileBackwardCompatible(t *testing.T) {
	root := t.TempDir()
	writeJSON(t, ProjectLocalPath(root), `{
  "permissions": {
    "allow": [
      "create_file",
      "edit_file",
      "run_command"
    ]
  }
}`)

	s := Load(root, "", &bytes.Buffer{})

	// Permissions load.
	got := map[string]bool{}
	for _, a := range s.Permissions.Allow {
		got[a] = true
	}
	for _, want := range []string{"create_file", "edit_file", "run_command"} {
		if !got[want] {
			t.Errorf("allow missing %q; got %v", want, s.Permissions.Allow)
		}
	}
	// The absent blocks are zero values, not errors.
	if s.Verify != nil {
		t.Errorf("Verify = %+v, want nil (block absent)", s.Verify)
	}
	if len(s.Hooks) != 0 {
		t.Errorf("Hooks = %v, want none (block absent)", s.Hooks)
	}
	// With no verify anywhere (block absent + empty legacy), verify is OFF.
	if got := ResolveVerifyFrom(s, root, ""); got != "" {
		t.Errorf("ResolveVerify = %q, want empty (verify OFF — opt-in)", got)
	}
}

// LoadFile distinguishes missing (no error) from malformed (error).
func TestLoadFileMissingVsMalformed(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadFile(filepath.Join(dir, "nope.json")); err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	writeJSON(t, bad, `nope`)
	if _, err := LoadFile(bad); err == nil {
		t.Error("malformed file should error")
	}
}

// ApprovalMode merges as an OVERRIDE like verify: the highest layer that sets
// it wins; absent everywhere stays empty (the caller defaults to "ask").
func TestApprovalModeOverrideHighestLayerWins(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	writeJSON(t, UserPath(home), `{"approval_mode":"auto"}`)
	writeJSON(t, ProjectSharedPath(root), `{"approval_mode":"full"}`)
	writeJSON(t, ProjectLocalPath(root), `{"approval_mode":"ask"}`)

	s := Load(root, home, &bytes.Buffer{})
	if s.ApprovalMode != "ask" {
		t.Errorf("ApprovalMode = %q, want %q (project-local wins)", s.ApprovalMode, "ask")
	}

	// Local absent → shared wins.
	root2 := t.TempDir()
	writeJSON(t, UserPath(home), `{"approval_mode":"auto"}`)
	writeJSON(t, ProjectSharedPath(root2), `{"approval_mode":"full"}`)
	s2 := Load(root2, home, &bytes.Buffer{})
	if s2.ApprovalMode != "full" {
		t.Errorf("ApprovalMode = %q, want %q (shared wins over user)", s2.ApprovalMode, "full")
	}

	// Nothing set anywhere → empty (caller defaults to ask).
	s3 := Load(t.TempDir(), t.TempDir(), &bytes.Buffer{})
	if s3.ApprovalMode != "" {
		t.Errorf("ApprovalMode = %q, want empty when unset", s3.ApprovalMode)
	}
}

// SetApprovalMode persists the tier as a top-level key, preserving other keys.
func TestSetApprovalModePersistsAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codeagent", "settings.local.json")
	writeJSON(t, path, `{"permissions":{"allow":["run_command"]},"verify":{"command":"go build ./..."}}`)

	if err := SetApprovalMode(path, "auto"); err != nil {
		t.Fatalf("SetApprovalMode: %v", err)
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.ApprovalMode != "auto" {
		t.Errorf("ApprovalMode = %q, want auto", f.ApprovalMode)
	}
	if len(f.Permissions.Allow) != 1 || f.Permissions.Allow[0] != "run_command" {
		t.Errorf("permissions clobbered: %v", f.Permissions.Allow)
	}
	if f.Verify == nil || f.Verify.Command != "go build ./..." {
		t.Errorf("verify clobbered: %+v", f.Verify)
	}
}
