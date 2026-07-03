package approve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// settingsFile is the machine-writable JSON we persist permission rules to — a
// subset of a Claude-style settings.json. Only the permissions block is read or
// written; unknown fields in an existing file are preserved by re-reading and
// re-marshaling just this shape is NOT enough, so we merge into a generic map
// (see persist) to avoid clobbering settings we don't model.
type settingsFile struct {
	Permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
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

	// persistMu serializes the read-modify-write of a settings file so two
	// concurrent grants (in serve mode every session shares one store) cannot
	// clobber each other's rule. Separate from mu so it never blocks matching.
	persistMu sync.Mutex

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
	if root != "" {
		s.projectLocalPath = filepath.Join(root, ".codeagent", "settings.local.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		s.userPath = filepath.Join(home, ".codeagent", "settings.json")
	}
	// User first, then project-local, so both contribute (it is a union — scope
	// precedence only matters for where a NEW grant is written, not for matching).
	s.loadFile(s.userPath)
	s.loadFile(s.projectLocalPath)
	return s
}

func (s *RuleStore) loadFile(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // missing (or unreadable) → nothing to add
	}
	var f settingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		fmt.Fprintf(os.Stderr, "[permissions] ignoring malformed %s: %v\n", path, err)
		return
	}
	s.mu.Lock()
	for _, p := range f.Permissions.Allow {
		s.allow[p] = struct{}{}
	}
	for _, p := range f.Permissions.Deny {
		s.deny[p] = struct{}{}
	}
	s.mu.Unlock()
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

// persistAllow appends pattern to the permissions.allow array of the settings
// file at path, preserving any other keys already in the file (it merges into a
// generic map, so settings we don't model are not clobbered). It creates the
// parent directory as needed.
//
// persistMu serializes the whole read-modify-write so concurrent grants don't
// lose each other's rule, and the write is atomic (temp file + rename) so a crash
// mid-write can't leave a partial/corrupt settings file.
func (s *RuleStore) persistAllow(path, pattern string) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc) // a corrupt file is overwritten with a valid one
	}

	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow := toStringSlice(perms["allow"])
	for _, p := range allow {
		if p == pattern {
			return nil // already persisted
		}
	}
	allow = append(allow, pattern)
	sort.Strings(allow)
	perms["allow"] = allow
	doc["permissions"] = perms

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Atomic replace: write a sibling temp file, then rename over the target.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
