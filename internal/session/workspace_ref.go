package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceRef is the *portable* identity of a session's workspace — the value
// that survives being persisted. Unlike an absolute path it stays valid across an
// iOS app reinstall / migration, where the sandbox container path
// (…/Containers/Data/Application/<UUID>/…) changes: the <UUID> is frozen into any
// absolute path written to disk, so on next launch it points at a dead container.
//
// The rule (see docs/ios_workspace_path_spec.md): never persist an absolute path
// as identity. Persist a ref, and re-derive the absolute path on load from the
// workspace directory the host supplies *this* launch (re-anchor).
//
//   - Root == RootWorkspace: the project lives under the launch-time workspaceDir.
//     Rel (relative to it) is the authoritative identity; Resolve re-joins it.
//   - Root == RootExternal: the project is outside workspaceDir. ExtID is a stable
//     identifier the host owns (on macOS it is simply the absolute path, which is
//     stable; on iOS it is a security-scoped bookmark id and the host must re-supply
//     the absolute path on attach).
//   - Root == "" (zero value): unanchored — an empty workspace (server default) or
//     a legacy row not yet migrated. AbsHint is used as-is (old behavior).
//
// AbsHint is the last-known absolute path. It is for display/diagnostics ONLY —
// no code path may read or write files through it.
type WorkspaceRef struct {
	Root    string // RootWorkspace | RootExternal | ""
	Rel     string // Root==RootWorkspace: path relative to workspaceDir ("." == the root itself)
	ExtID   string // Root==RootExternal: host-stable id (macOS: the absolute path)
	AbsHint string // display/diagnostics only — never used to resolve files
}

const (
	RootWorkspace = "workspace"
	RootExternal  = "external"
)

// ErrExternalNeedsHostRebind is returned by Resolve for an external workspace that
// has no usable absolute path this launch: the host must re-supply it (iOS) before
// the session can run.
var ErrExternalNeedsHostRebind = errors.New("external workspace needs host to re-supply its absolute path")

// ToWorkspaceRef computes the portable ref for an absolute workspace path, given
// the launch-time workspaceDir and an optional host-supplied external id. An empty
// absPath yields the zero ref (server-default workspace). This is the write path
// (relativize on create); see spec §5.1.
func ToWorkspaceRef(absPath, workspaceDir, hostExtID string) WorkspaceRef {
	if absPath == "" {
		return WorkspaceRef{}
	}
	abs := normalizePath(absPath)
	base := normalizePath(workspaceDir)

	if base != "" && (abs == base || isUnder(abs, base)) {
		rel := "."
		if abs != base {
			if r, err := filepath.Rel(base, abs); err == nil {
				rel = r
			}
		}
		return WorkspaceRef{Root: RootWorkspace, Rel: rel, AbsHint: abs}
	}

	// External: the project is outside workspaceDir. Prefer the host-supplied stable
	// id; absent that, fall back to the absolute path itself (stable on macOS; on iOS
	// this is best-effort and the host should re-supply via attach — see batch 2).
	extID := hostExtID
	if extID == "" {
		extID = abs
	}
	return WorkspaceRef{Root: RootExternal, ExtID: extID, AbsHint: abs}
}

// Resolve re-derives the absolute workspace path for this launch. currentWorkspaceDir
// is the workspaceDir passed to *this* MobileStart; hostSuppliedAbs, when non-empty,
// is a fresh absolute path the host re-supplied on attach (required for external on
// iOS). This is the read path (re-anchor on load); see spec §5.2.
func (r WorkspaceRef) Resolve(currentWorkspaceDir, hostSuppliedAbs string) (string, error) {
	switch r.Root {
	case RootWorkspace:
		// Re-join against this launch's workspaceDir → automatically correct across
		// reinstall/migration. The host need not be involved at all.
		return safeJoin(currentWorkspaceDir, r.Rel)
	case RootExternal:
		if hostSuppliedAbs != "" {
			return normalizePath(hostSuppliedAbs), nil
		}
		if filepath.IsAbs(r.ExtID) {
			// macOS: the absolute path is itself a stable identity.
			return normalizePath(r.ExtID), nil
		}
		return "", ErrExternalNeedsHostRebind
	default:
		// Unanchored: empty workspace (server default) or not-yet-migrated legacy row.
		return r.AbsHint, nil
	}
}

// MigrateLegacyWorkspacePath converts a legacy absolute workspace_path into a ref.
// It first tries to salvage the tail after a "/Documents/" marker and re-anchor it
// under currentWorkspaceDir (the iOS reinstall case); failing that it checks whether
// the path still lives under currentWorkspaceDir as-is; failing that it records an
// external ref keyed by the absolute path (stable on macOS, best-effort on iOS).
// Idempotent: callers persist the result so the next load sees a non-empty Root.
// See spec §9.
func MigrateLegacyWorkspacePath(absPath, currentWorkspaceDir string) WorkspaceRef {
	if absPath == "" {
		return WorkspaceRef{}
	}
	// 1) Salvage by tail: the segment after /Documents/ usually still exists under
	//    the *current* container's Documents, even though the old <UUID> prefix is dead.
	if rel, ok := relAfterMarker(absPath, "/Documents/"); ok && currentWorkspaceDir != "" {
		if joined, err := safeJoin(currentWorkspaceDir, rel); err == nil && pathExists(joined) {
			return WorkspaceRef{Root: RootWorkspace, Rel: rel, AbsHint: normalizePath(absPath)}
		}
	}
	// 2) Maybe it is already under the current workspaceDir (same install, valid path).
	if ref := ToWorkspaceRef(absPath, currentWorkspaceDir, ""); ref.Root == RootWorkspace {
		return ref
	}
	// 3) Give up on relativizing — keep it external, anchored on the absolute path.
	abs := normalizePath(absPath)
	return WorkspaceRef{Root: RootExternal, ExtID: abs, AbsHint: abs}
}

// --- path helpers (shared by relativize and resolve so isUnder never flaps) -----

// normalizePath resolves symlinks and cleans a path. iOS Documents resolves with a
// /private prefix (/var → /private/var); relativize and resolve must use the SAME
// normalization or isUnder would disagree between them. EvalSymlinks fails for a
// path that does not exist yet, in which case Clean(Abs(p)) is the best we can do.
func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	abs := p
	if a, err := filepath.Abs(p); err == nil {
		abs = a
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// isUnder reports whether path is at or below base. Both should be normalized.
func isUnder(path, base string) bool {
	if base == "" {
		return false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// safeJoin joins rel onto base and refuses any rel that escapes base via "..".
// The join happens in base's normalized space so the escape check and the /private
// prefix stay consistent. rel == "" or "." yields base itself.
func safeJoin(base, rel string) (string, error) {
	nb := normalizePath(base)
	if rel == "" {
		rel = "."
	}
	joined := filepath.Clean(filepath.Join(nb, rel))
	if joined != nb && !isUnder(joined, nb) {
		return "", errors.New("workspace rel escapes its base: " + rel)
	}
	return joined, nil
}

// pathExists reports whether a filesystem entry exists at p.
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// relAfterMarker returns the path segment after the last occurrence of marker
// (e.g. "/Documents/"), trimmed of surrounding slashes. "" after the marker → ".".
func relAfterMarker(abs, marker string) (string, bool) {
	i := strings.LastIndex(abs, marker)
	if i < 0 {
		return "", false
	}
	rel := strings.Trim(abs[i+len(marker):], "/")
	if rel == "" {
		return ".", true
	}
	return rel, true
}
