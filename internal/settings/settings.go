// Package settings loads the project settings layer — Claude Code's settings.json
// model — that owns project-scoped BEHAVIOR (permissions now; verify and hooks in
// later phases), as opposed to config.yaml's INFRASTRUCTURE (models, API keys,
// endpoints). See docs/p11-project-settings.md.
//
// On disk the files are a subset of Claude Code's settings.json, layered in
// precedence order (low → high):
//
//	~/.codeagent/settings.json          — user, all your projects
//	<root>/.codeagent/settings.json     — project, shared (committable)
//	<root>/.codeagent/settings.local.json — project, this machine (git-ignored)
//
// Secrets never live here (the files are committable); API keys stay in
// config.yaml / env / the host keychain.
package settings

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"code-agent/internal/hooks"
)

// Permissions is the tool-name allow/deny block, matched as globs downstream.
type Permissions struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Verify is the finalize-verify block (P4.3-R Move 2, relocated here by P11.b).
// Command is a literal build/test command, "auto" to detect from the workspace
// (§8 of the design), or "" to disable. Enabled (nil = on when a command is set)
// can force it off while keeping the command documented.
type Verify struct {
	Command string `json:"command"`
	Enabled *bool  `json:"enabled"`
}

// File is the on-disk shape of one settings file — a subset of Claude Code's
// settings.json. Unknown keys are ignored on read (and preserved on write by the
// atomic writer that owns persistence), so a file authored for a newer version
// never breaks an older binary. P11.a models only permissions; verify and hooks
// blocks are added in P11.b/c.
type File struct {
	Permissions Permissions `json:"permissions"`
	// Verify is a pointer so an ABSENT block (nil) is distinguishable from one set
	// to empty — the override merge needs that to know which layer "wins".
	Verify *Verify      `json:"verify"`
	Hooks  []hooks.Hook `json:"hooks"`
}

// Settings is the merged view across the layers. Permissions merge as a UNION
// (deny wins downstream); Verify merges as an OVERRIDE (the highest layer that
// sets a block wins); Hooks CONCATENATE (every layer's hooks all run, in layer
// order).
type Settings struct {
	Permissions Permissions
	Verify      *Verify      // highest-priority layer's block, nil if no layer set one
	Hooks       []hooks.Hook // all layers' hooks, user → shared → local order
}

// UserPath is the user-scope file, shared across all your projects. Empty home
// (unresolvable) disables it.
func UserPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codeagent", "settings.json")
}

// ProjectSharedPath is the committable, team-shared project file. Empty root
// disables it.
func ProjectSharedPath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".codeagent", "settings.json")
}

// ProjectLocalPath is the git-ignored, machine-local project file — the default
// target for an interactive grant or an agent-written setting. Empty root
// disables it.
func ProjectLocalPath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".codeagent", "settings.local.json")
}

// LoadFile reads and parses one settings file. A missing file is not an error
// (absence is normal) — it returns the zero File. A present-but-malformed file
// returns an error so the caller can log-and-skip rather than fail startup.
func LoadFile(path string) (File, error) {
	var f File
	if path == "" {
		return f, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

// Load reads the layer files for root/home in precedence order and returns the
// merged Settings. Best-effort: a missing file is skipped, and a malformed one is
// logged to warn (nil = os.Stderr) and skipped, so a corrupt machine-written file
// never bricks startup. Empty root disables the project files; empty home
// disables the user file.
func Load(root, home string, warn io.Writer) Settings {
	if warn == nil {
		warn = os.Stderr
	}
	var s Settings
	for _, path := range []string{UserPath(home), ProjectSharedPath(root), ProjectLocalPath(root)} {
		if path == "" {
			continue
		}
		f, err := LoadFile(path)
		if err != nil {
			fmt.Fprintf(warn, "[settings] ignoring malformed %s: %v\n", path, err)
			continue
		}
		s.Permissions.Allow = appendUnique(s.Permissions.Allow, f.Permissions.Allow...)
		s.Permissions.Deny = appendUnique(s.Permissions.Deny, f.Permissions.Deny...)
		// Verify overrides: iterating low → high, the last layer to set a block wins.
		if f.Verify != nil {
			s.Verify = f.Verify
		}
		// Hooks concatenate: every layer's hooks run, in layer order.
		s.Hooks = append(s.Hooks, f.Hooks...)
	}
	return s
}

// ResolveVerify returns the effective finalize-verify command for the workspace:
// the settings layer's verify block if any layer set one, else the config.yaml
// legacy value (P4.3-R Move 2's agent.verify_command, layer 0). "" means disabled.
// A command of "auto" is detected from the workspace (§8); anything absent leaves
// verify OFF (P11 decision 3 — auto-detect is opt-in, not the default).
func ResolveVerify(root, home, legacy string) string {
	return ResolveVerifyFrom(Load(root, home, os.Stderr), root, legacy)
}

// ResolveVerifyFrom is ResolveVerify against an already-loaded Settings, so a
// caller that also needs other blocks (e.g. hooks) loads the layer only once.
func ResolveVerifyFrom(s Settings, root, legacy string) string {
	if s.Verify != nil {
		if s.Verify.Enabled != nil && !*s.Verify.Enabled {
			return ""
		}
		return resolveVerifyCommand(s.Verify.Command, root)
	}
	return resolveVerifyCommand(legacy, root)
}

// resolveVerifyCommand maps a raw command string to the command to run: the
// disable words and "" → off; "auto" → detected; anything else verbatim.
func resolveVerifyCommand(cmd, root string) string {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "", "off", "false", "none", "disabled":
		return ""
	case "auto":
		return DetectVerify(root)
	default:
		return cmd
	}
}

// DetectVerify infers a cheap, side-effect-free build command from the files at
// the workspace root (§8). First match wins; build-class over full test suites so
// auto-running at every finish stays cheap. "" when nothing recognizable is found.
func DetectVerify(root string) string {
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return "go build ./..."
	case exists("Cargo.toml"):
		return "cargo build"
	case exists("package.json"):
		if cmd := detectNodeVerify(filepath.Join(root, "package.json")); cmd != "" {
			return cmd
		}
	}
	if exists("Package.swift") {
		return "swift build"
	}
	if xs, _ := filepath.Glob(filepath.Join(root, "*.xcodeproj")); len(xs) > 0 {
		return "swift build"
	}
	return ""
}

// detectNodeVerify picks a build/test npm script from a package.json, preferring
// the cheaper "build". "" when neither script exists (so detection falls through).
func detectNodeVerify(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	if _, ok := pkg.Scripts["build"]; ok {
		return "npm run build"
	}
	if _, ok := pkg.Scripts["test"]; ok {
		return "npm test"
	}
	return ""
}

// appendUnique appends the non-empty, not-yet-present items of xs to dst.
func appendUnique(dst []string, xs ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(xs))
	for _, d := range dst {
		seen[d] = struct{}{}
	}
	for _, x := range xs {
		if x == "" {
			continue
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		dst = append(dst, x)
	}
	return dst
}
