package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/assetref"
)

func TestAssetResolutionRejectsManagedWorktreeFromBaseSession(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".codeagent", "worktrees", "other", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveAssetPath(assets.Ref{WorkspaceRelativePath: ".codeagent/worktrees/other/secret.txt"}, root)
	var httpErr assetHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusForbidden {
		t.Fatalf("err=%v", err)
	}
}
