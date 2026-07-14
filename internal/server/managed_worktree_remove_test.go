package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/managedworktree"
	"code-agent/internal/session"
)

func TestManagedWorktreeRemoveHTTPIsDirtySafeAndForceIsExplicit(t *testing.T) {
	root := initManagedHTTPRepo(t)
	store := session.NewMemoryStore()
	repo := newFakeConversationRepo()
	manager := managedworktree.New(store, repo)
	handler := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{ManagedWorktrees: manager})
	server := httptest.NewServer(handler)
	defer server.Close()

	createBody := map[string]any{
		"client_request_id": "create_http_remove", "workspace_path": root,
		"execution_policy": session.ExecutionPolicyIsolatedWorktree,
		"worktree":         map[string]any{"managed": true, "base_ref": "head"},
	}
	createResponse := requestJSON(t, server.URL+"/v1/conversations", createBody)
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createResponse.StatusCode, readManagedHTTPBody(t, createResponse))
	}
	var conversation ConversationRef
	decodeResponse(t, createResponse, &conversation)
	if err := os.WriteFile(filepath.Join(conversation.WorkspacePath, "untracked.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	normal := requestJSON(t, server.URL+"/v1/conversations/"+conversation.ID+"/worktree/remove", map[string]any{"request_id": "remove_http"})
	if normal.StatusCode != http.StatusConflict {
		t.Fatalf("normal status=%d body=%s", normal.StatusCode, readManagedHTTPBody(t, normal))
	}
	body := readManagedHTTPBody(t, normal)
	if !strings.Contains(body, managedworktree.CodeDirty) {
		t.Fatalf("normal body=%s", body)
	}
	if _, err := os.Stat(conversation.WorkspacePath); err != nil {
		t.Fatalf("dirty worktree was removed: %v", err)
	}

	forced := requestJSON(t, server.URL+"/v1/conversations/"+conversation.ID+"/worktree/remove", map[string]any{"request_id": "remove_http_force", "force": true})
	if forced.StatusCode != http.StatusOK {
		t.Fatalf("force status=%d body=%s", forced.StatusCode, readManagedHTTPBody(t, forced))
	}
	forced.Body.Close()
	if _, err := os.Stat(conversation.WorkspacePath); !os.IsNotExist(err) {
		t.Fatalf("forced worktree still exists: %v", err)
	}
}

func requestJSON(t *testing.T, url string, value any) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readManagedHTTPBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func initManagedHTTPRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		runManagedHTTPGit(t, root, args...)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runManagedHTTPGit(t, root, "add", "tracked.txt")
	runManagedHTTPGit(t, root, "commit", "-m", "initial")
	return root
}

func runManagedHTTPGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
