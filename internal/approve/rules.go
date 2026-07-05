package approve

import (
	"os"
	"strings"
	"sync"

	"code-agent/internal/settings"
)

// Scope is where an "always allow" rule is persisted, mirroring Claude Code's
// permission scopes. Project-local is the default for an interactive grant: it is
// machine-local and not shared, so it never lands in version control.
type Scope int

const (
	// ScopeProjectLocal persists to <root>/.codeagent/settings.local.json —
	// private to this machine + project (gitignore it), like Claude's
	// .claude/settings.local.json.
	ScopeProjectLocal Scope = iota
	// ScopeUser persists to ~/.codeagent/settings.json — all your projects, like
	// Claude's ~/.claude/settings.json.
	ScopeUser
)

// ParseScope maps a wire/string scope to a Scope. Anything other than "user"
// (including "local", "project-local", "", or an unknown value) defaults to
// ScopeProjectLocal — the conservative, machine-local choice.
func ParseScope(s string) Scope {
	if s == "user" {
		return ScopeUser
	}
	return ScopeProjectLocal
}

// RuleStore is the single source of truth for permission rules: the union of the
// YAML config's permissions and the loaded settings files, plus any rules granted
// interactively at runtime ("Always allow"). It is safe for concurrent use — the
// loop matches against it while a frontend approver may Grant a new rule — and it
// persists grants to the scoped settings file so they survive restart.
//
// The outer Allowlist reads its verdict from here on every call, so a rule Granted
// mid-session takes effect on the very next matching call without rebuilding
// anything.
type RuleStore struct {
	mu    sync.RWMutex
	allow map[string]struct{}
	deny  map[string]struct{}

	projectLocalPath string // "" when no workspace root is known
	userPath         string // "" when the home dir cannot be resolved
}

// NewRuleStore seeds a store from the YAML config's allow/deny patterns, then
// unions in the user- and project-local settings files. Loading is best-effort:
// a missing file is skipped and a malformed one is logged and ignored rather than
// failing startup (these are machine-written files; one being corrupt must not
// brick the agent). root is the workspace root; "" disables the project-local
// file (grants then default to the user scope's path when available).
func NewRuleStore(root string, yamlAllow, yamlDeny []string) *RuleStore {
	s := &RuleStore{
		allow: toSet(yamlAllow),
		deny:  toSet(yamlDeny),
	}
	home, _ := os.UserHomeDir()
	// Write paths for a NEW grant: project-local by default, user for a shared
	// grant. Reading is a union across all layers (below); scope only decides where
	// a grant is persisted. Grants never target the shared settings.json (that file
	// is committable/team-owned — see docs/p11-project-settings.md).
	if root != "" {
		s.projectLocalPath = settings.ProjectLocalPath(root)
	}
	if home != "" {
		s.userPath = settings.UserPath(home)
	}
	// Union in the file-sourced permissions across every layer — user,
	// project-shared, and project-local (P11.a: the shared settings.json now
	// contributes too, matching Claude Code's layering).
	merged := settings.Load(root, home, os.Stderr)
	for _, p := range merged.Permissions.Allow {
		s.allow[p] = struct{}{}
	}
	for _, p := range merged.Permissions.Deny {
		s.deny[p] = struct{}{}
	}
	return s
}

// MatchDeny reports the first deny pattern matching name, if any.
func (s *RuleStore) MatchDeny(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return matchSet(s.deny, name)
}

// MatchAllow reports the first allow pattern matching name, if any.
func (s *RuleStore) MatchAllow(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return matchSet(s.allow, name)
}

// AllowAlways persists an "always allow" rule derived from a tool name at the
// project-local scope (the default an interactive terminal grant uses) and
// returns the rule that was added, for display. It satisfies the narrow granter
// interface the terminal approver depends on.
func (s *RuleStore) AllowAlways(toolName string) (string, error) {
	return s.GrantTool(toolName, ScopeProjectLocal)
}

// GrantTool derives an allow rule from a tool name (an MCP tool grants its whole
// server, "mcp__<server>__*", so one confirmation covers the rest of that
// server's tools; a built-in grants its exact name), adds it to the live rule set
// and persists it at the given scope. Returns the rule that was added.
func (s *RuleStore) GrantTool(toolName string, scope Scope) (string, error) {
	pattern := patternFor(toolName)
	if err := s.Grant(pattern, scope); err != nil {
		return "", err
	}
	return pattern, nil
}

// Grant adds an allow pattern to the live set and appends it to the scoped
// settings file (no-op if already present). The in-memory add happens even if the
// file write fails, so the rule is at least honored for the current session.
func (s *RuleStore) Grant(pattern string, scope Scope) error {
	s.mu.Lock()
	_, existed := s.allow[pattern]
	s.allow[pattern] = struct{}{}
	path := s.pathFor(scope)
	s.mu.Unlock()

	if existed || path == "" {
		return nil
	}
	return s.persistAllow(path, pattern)
}

func (s *RuleStore) pathFor(scope Scope) string {
	if scope == ScopeUser {
		return s.userPath
	}
	// Project-local, but fall back to the user path when no workspace root was
	// known, so a grant is still persisted somewhere rather than silently lost.
	if s.projectLocalPath != "" {
		return s.projectLocalPath
	}
	return s.userPath
}

// persistAllow appends pattern to the settings file's permissions.allow via the
// single canonical atomic, unknown-key-preserving writer in the settings package
// (P11.d — the same writer the agent's verify self-write uses).
func (s *RuleStore) persistAllow(path, pattern string) error {
	return settings.AddAllowRule(path, pattern)
}

// patternFor maps a tool name to the rule an "always allow" grant should add:
// a whole-server wildcard for MCP tools, the exact name for everything else.
func patternFor(toolName string) string {
	if rest, ok := strings.CutPrefix(toolName, "mcp__"); ok {
		if i := strings.Index(rest, "__"); i >= 0 {
			return "mcp__" + rest[:i] + "__*"
		}
	}
	return toolName
}

func toSet(items []string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it != "" {
			m[it] = struct{}{}
		}
	}
	return m
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// matchSet returns the first pattern in the set that matches name. Iteration
// order is nondeterministic, but patterns are disjoint verdicts (all "allow" or
// all "deny"), so any match is equivalent — only the reported pattern string
// (for the audit reason) can differ.
func matchSet(set map[string]struct{}, name string) (string, bool) {
	for p := range set {
		if matchGlob(p, name) {
			return p, true
		}
	}
	return "", false
}
