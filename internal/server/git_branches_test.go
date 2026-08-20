package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"code-agent/internal/gitworkspace"
)

func TestGitBranchRoutesAndCapability(t *testing.T) {
	root := newGitBranchTestRepo(t)
	h := NewMux(nil, nil, nil, MuxOptions{GitBranches: gitworkspace.New(root, nil, nil)})
	body, _ := json.Marshal(map[string]string{"workspace_path": root})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/git/branches/list", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	var list gitworkspace.Result
	decodeResponse(t, resp, &list)
	if len(list.Branches) != 1 {
		t.Fatalf("list=%+v", list)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/runtime/capabilities", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp = rec.Result()
	var caps runtimeCapabilitiesResponse
	decodeResponse(t, resp, &caps)
	if !caps.Capabilities.WorkspaceGitBranch {
		t.Fatalf("caps=%+v", caps)
	}
}

func newGitBranchTestRepo(t *testing.T) string   { return newRepositoryForServer(t) }
func newRepositoryForServer(t *testing.T) string { t.Helper(); return makeGitRepo(t) }
func makeGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGitForServer(t, root, "init", "-b", "main")
	runGitForServer(t, root, "config", "user.email", "test@example.com")
	runGitForServer(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForServer(t, root, "add", "README")
	runGitForServer(t, root, "commit", "-m", "initial")
	return root
}
func runGitForServer(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
}
