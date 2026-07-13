package git

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/tools"
)

func TestApplyPatchRejectsManagedWorktreeTargetForBothBackends(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".codeagent", "worktrees", "other", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	patch := `--- /dev/null
+++ b/.codeagent/worktrees/other/secret.txt
@@ -0,0 +1 @@
+secret
`
	input, err := json.Marshal(applyPatchInput{Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	for name, tool := range map[string]*ApplyPatchTool{
		"exec":   NewApplyPatchTool(),
		"go-git": NewApplyPatchToolGoGit(),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, input)
			if err != nil || !strings.Contains(result.Content, "workspace boundary") {
				t.Fatalf("result=%q err=%v, want managed workspace boundary refusal", result.Content, err)
			}
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("target was created: %v", statErr)
			}
		})
	}
}
