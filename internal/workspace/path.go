package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const ManagedWorktreesRelativeRoot = ".codeagent/worktrees"

var ErrManagedWorktreeBoundary = errors.New("path is inside the managed worktree root")

// PathClass classifies a target path relative to a workspace root. It replaces
// the binary inside/outside check with a three-way decision that lets the caller
// choose how to handle each case: approve external reads, block managed worktree
// paths absolutely, and proceed without ceremony for paths inside the workspace.
type PathClass int

const (
	// PathInsideWorkspace — the target is within the workspace root and is not
	// inside a managed worktree. Read/write tools proceed directly.
	PathInsideWorkspace PathClass = iota
	// PathOutsideWorkspace — the target is outside the workspace root. Read-only
	// tools may request user approval through a PathAccessApprover before
	// proceeding; write and shell tools should still reject.
	PathOutsideWorkspace
	// PathManagedWorktree — the target is inside .codeagent/worktrees/. Always
	// blocked for direct workspace tools regardless of approval.
	PathManagedWorktree
)

// ClassifyPath categorises a target path relative to the workspace root. It
// resolves every existing component through symlinks (CanonicalPath) so a
// symlink escape is caught before a tool reads or writes through it.
//
// The caller decides how to handle each class:
//   - PathInsideWorkspace: proceed
//   - PathOutsideWorkspace: request approval or reject
//   - PathManagedWorktree: reject (never approvable)
func ClassifyPath(root, target string) PathClass {
	rootCanonical, err := CanonicalPath(root)
	if err != nil {
		return PathOutsideWorkspace
	}
	targetCanonical, err := CanonicalPath(target)
	if err != nil {
		return PathOutsideWorkspace
	}
	// Managed worktree check comes first — these paths are technically inside the
	// workspace but are always blocked for direct workspace tools.
	if isManagedRelative(root, target) || isManagedRelative(rootCanonical, targetCanonical) {
		return PathManagedWorktree
	}
	if !isSubPathFold(rootCanonical, targetCanonical) {
		return PathOutsideWorkspace
	}
	return PathInsideWorkspace
}

func IsSubPath(rootAbs, targetAbs string) bool {
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}

func ShouldSkipName(name string) bool {

	switch name {
	case ".git", "node_modules", "vendor", ".idea", ".vscode", ".DS_Store", "dist", "build", ".next":
		return true
	default:
		return false
	}

}

// ShouldSkipPath centralizes workspace-wide discovery exclusions. It is based
// on both the lexical and symlink-resolved path, so a base checkout cannot scan
// a managed worktree through a differently-cased name or symlink alias. A
// managed checkout remains usable because the comparison is relative to its own
// root, not to an ancestor repository.
func ShouldSkipPath(root, path string) bool {
	return isManagedRelative(root, path) || isManagedRelativeCanonical(root, path)
}

// ValidatePath enforces both workspace containment and the managed-root
// boundary for direct read/write tools and asset resolution. It delegates to
// ClassifyPath so the three-way decision is defined in one place.
func ValidatePath(root, target string) error {
	switch ClassifyPath(root, target) {
	case PathInsideWorkspace:
		return nil
	case PathManagedWorktree:
		return ErrManagedWorktreeBoundary
	default:
		return errors.New("path escapes workspace")
	}
}

// CanonicalPath resolves every existing path component and preserves a clean
// suffix for a not-yet-created target. This catches symlink escapes before a
// create operation writes through them.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	current := abs
	var suffix []string
	for {
		real, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return filepath.Clean(real), nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func SamePath(a, b string) bool {
	ca, errA := CanonicalPath(a)
	cb, errB := CanonicalPath(b)
	return errA == nil && errB == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// ValidateManagedTarget permits exactly one checkout directory immediately
// below <project>/.codeagent/worktrees after canonicalizing existing parents.
// It is intentionally separate from ValidatePath, which blocks this tree for
// normal workspace tools.
func ValidateManagedTarget(projectRoot, target string) error {
	project, err := CanonicalPath(projectRoot)
	if err != nil {
		return err
	}
	container, err := CanonicalPath(filepath.Join(project, filepath.FromSlash(ManagedWorktreesRelativeRoot)))
	if err != nil {
		return err
	}
	resolvedTarget, err := CanonicalPath(target)
	if err != nil {
		return err
	}
	if !isSubPathFold(project, container) || !strings.EqualFold(filepath.Dir(resolvedTarget), container) || strings.EqualFold(resolvedTarget, container) {
		return errors.New("managed worktree target escapes configured root")
	}
	return nil
}

func isManagedRelativeCanonical(root, path string) bool {
	canonicalRoot, err := CanonicalPath(root)
	if err != nil {
		return false
	}
	canonicalPath, err := CanonicalPath(path)
	if err != nil {
		return false
	}
	return isManagedRelative(canonicalRoot, canonicalPath)
}

func isManagedRelative(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil || !isSubPathFold(rootAbs, pathAbs) {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) >= 2 && strings.EqualFold(parts[0], ".codeagent") && strings.EqualFold(parts[1], "worktrees")
}

func isSubPathFold(root, target string) bool {
	root = strings.ToLower(filepath.Clean(root))
	target = strings.ToLower(filepath.Clean(target))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func PathDepth(rel string) int {

	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(filepath.ToSlash(rel), "/") + 1

}

// ResolveToolPath resolves a user-supplied path (from a tool input) against the
// workspace root. If the path is already absolute it is used directly (cleaned);
// otherwise it is joined with the workspace root. The result is not yet passed
// through filepath.Abs — callers should do that to catch any remaining issues
// (symlink escapes etc.).
//
// This avoids the cross-platform ambiguity of filepath.Join(root, absPath):
// on some platforms the absolute argument resets the result; on others the
// behaviour is not guaranteed.
func ResolveToolPath(rootAbs, inputPath string) string {
	clean := filepath.Clean(inputPath)
	if filepath.IsAbs(clean) {
		return clean
	}
	return filepath.Join(rootAbs, clean)
}
