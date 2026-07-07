// Package sandbox — protected path registry.
//
// Protected paths are file patterns that the safety layer treats as sensitive.
// Any tool accessing them (read, write, edit, grep, or shell command) triggers
// heightened scrutiny: read-only tools emit an audit event, and mutating tools
// require explicit user confirmation regardless of allow rules or auto mode.
//
// The registry merges built-in defaults (DefaultProtectedPaths) with
// user-configured additions from settings.json's permissions.protected_paths.
// This file owns the defaults; settings files can only ADD paths, never remove
// the built-ins.

package sandbox

import (
	"path/filepath"
	"strings"
)

// DefaultProtectedPaths are the built-in sensitive file patterns. They always
// apply — settings files can add more but cannot remove these. The patterns are
// matched against the file's base name (case-insensitive).
var DefaultProtectedPaths = []string{
	// Environment files — may contain API keys, secrets, tokens
	".env", ".env.local", ".env.production", ".env.staging", ".env.development",

	// Git credentials
	".git-credentials", ".gitconfig",

	// Generic secret / credential files
	"credentials", "secrets", "tokens",

	// Private keys
	"private.key", "id_rsa", "id_ed25519", "id_ecdsa",
	"*.pem", "*.key",
}

// ProtectedPaths returns the effective list of protected patterns: built-in
// defaults unioned with any user-configured additions from settings. Duplicates
// are removed.
func ProtectedPaths(extra []string) []string {
	seen := make(map[string]struct{}, len(DefaultProtectedPaths)+len(extra))
	var out []string
	for _, p := range DefaultProtectedPaths {
		seen[strings.ToLower(p)] = struct{}{}
		out = append(out, p)
	}
	for _, p := range extra {
		if p == "" {
			continue
		}
		key := strings.ToLower(p)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

// IsPathProtected reports whether the given relative path names a file whose
// content should be treated as sensitive. The check is purely lexical — no
// symlink resolution — matching the same discipline as workspace.IsSubPath.
//
// It checks the file's base name against every protected pattern. A pattern
// without a glob character matches exactly; a pattern with '*' matches as a
// simple wildcard (e.g. "*.key" matches "server.key" and "ca.key").
func IsPathProtected(rel string, protected []string) bool {
	base := filepath.Base(rel)
	for _, pattern := range protected {
		if matchPathPattern(base, pattern) {
			return true
		}
	}
	return false
}

// matchPathPattern reports whether name matches pattern. If pattern contains
// '*', it does glob-style matching; otherwise it does case-insensitive exact
// matching. This is a simple, allocation-free matcher (no path.Match or
// filepath.Match overhead for exact matches, and no error path to handle).
func matchPathPattern(name, pattern string) bool {
	// No wildcard: simple case-insensitive comparison.
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(name, pattern)
	}
	// Glob-style: split on *, match prefix/suffix.
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		// * alone or no * at all (handled above).
		return strings.EqualFold(name, pattern)
	}
	// Prefix must match.
	if parts[0] != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(parts[0])) {
		return false
	}
	rest := name
	if parts[0] != "" {
		rest = rest[len(parts[0]):]
	}
	// Middle parts must appear in order.
	for _, mid := range parts[1 : len(parts)-1] {
		idx := strings.Index(strings.ToLower(rest), strings.ToLower(mid))
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(mid):]
	}
	// Suffix must match.
	last := parts[len(parts)-1]
	if last != "" && !strings.HasSuffix(strings.ToLower(rest), strings.ToLower(last)) {
		return false
	}
	return true
}
