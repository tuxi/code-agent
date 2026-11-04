package filesystem

import (
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
	"fmt"
	"path/filepath"
)

// resolveWriteTarget applies the path gates every write tool shares: resolve the
// tool path against the workspace root, refuse the managed-worktree subtree,
// and require approval for anything outside the workspace.
//
// It reports denied=true when the user refused an out-of-workspace path, so each
// tool can word that as its own kind of result; err is a hard failure (bad
// workspace root) that no retry from the model can fix.
func resolveWriteTarget(ec tools.ExecutionContext, relPath string) (targetAbs string, denied bool, err error) {
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return "", false, err
	}
	targetAbs = workspace.ResolveToolPath(rootAbs, relPath)
	targetAbs, err = filepath.Abs(targetAbs)
	if err != nil {
		return "", false, err
	}
	switch workspace.ClassifyPath(rootAbs, targetAbs) {
	case workspace.PathManagedWorktree:
		return "", false, fmt.Errorf("path is not accessible: %s", relPath)
	case workspace.PathOutsideWorkspace:
		if ec.PathAccessApprover == nil {
			return "", false, fmt.Errorf("path is outside the workspace: %s", relPath)
		}
		if !ec.PathAccessApprover.ApproveExternalPath(targetAbs, "write") {
			return targetAbs, true, nil
		}
		// approved — proceed
	case workspace.PathInsideWorkspace:
		// proceed
	}
	return targetAbs, false, nil
}

// diffPath renders targetAbs the way a turn diffstat wants it: relative to the
// workspace root, slash-separated. Shared by every filesystem write tool so a
// path reported by create_file and one reported by a patch agree byte for byte.
func diffPath(ec tools.ExecutionContext, targetAbs string) string {
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return filepath.ToSlash(targetAbs)
	}
	return tools.WorkspaceRel(rootAbs, targetAbs)
}

// checkPlanModeTarget enforces plan mode's single write destination: the
// authoritative markdown plan under .codeagent/plans/. Writes anywhere else
// would mutate the project before propose_plan is approved. verb names the
// attempted operation ("edit", "create", "insert into") for the message.
func checkPlanModeTarget(ec tools.ExecutionContext, targetAbs, relPath, verb string) error {
	if !ec.PlanMode {
		return nil
	}
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return err
	}
	plansDir := filepath.Join(rootAbs, ".codeagent", "plans")
	if filepath.Ext(targetAbs) != ".md" || !workspace.IsSubPath(plansDir, targetAbs) {
		return fmt.Errorf("plan mode: cannot %s %s — only markdown files under .codeagent/plans/ may change before the plan is approved", verb, relPath)
	}
	return nil
}
