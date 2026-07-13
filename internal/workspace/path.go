package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const ManagedWorktreesRelativeRoot = ".codeagent/worktrees"

var ErrManagedWorktreeBoundary = errors.New("path is inside the managed worktree root")

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
// boundary for direct read/write tools and asset resolution.
func ValidatePath(root, target string) error {
	rootCanonical, err := CanonicalPath(root)
	if err != nil {
		return err
	}
	targetCanonical, err := CanonicalPath(target)
	if err != nil {
		return err
	}
	if !isSubPathFold(rootCanonical, targetCanonical) {
		return errors.New("path escapes workspace")
	}
	if isManagedRelative(root, target) || isManagedRelative(rootCanonical, targetCanonical) {
		return ErrManagedWorktreeBoundary
	}
	return nil
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
