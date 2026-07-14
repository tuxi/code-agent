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
	if !strings.Contains(body, `"untracked_files":1`) {
		t.Fatalf("normal response has no dirty summary: %s", body)
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

func TestManagedWorktreeMetadataAndMissingAttention(t *testing.T) {
	root := initManagedHTTPRepo(t)
	store := session.NewMemoryStore()
	repo := newFakeConversationRepo()
	manager := managedworktree.New(store, repo)
	caps := ConfiguredRuntimeCapabilities(2)
	caps.ManagedWorktree = true
	server := httptest.NewServer(NewMux(repo, &fakeEventStore{}, nil, MuxOptions{ManagedWorktrees: manager, RuntimeCapabilities: caps}))
	defer server.Close()
	capabilityResponse, err := http.Get(server.URL + "/v1/runtime/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var advertised runtimeCapabilitiesResponse
	decodeResponse(t, capabilityResponse, &advertised)
	if !advertised.Capabilities.ManagedWorktree {
		t.Fatalf("capabilities=%+v", advertised.Capabilities)
	}

	created := requestJSON(t, server.URL+"/v1/conversations", map[string]any{
		"client_request_id": "create_metadata", "workspace_path": root,
		"workspace_id": "source_main", "base_workspace_id": "base_project",
		"execution_policy": session.ExecutionPolicyIsolatedWorktree,
		"worktree":         map[string]any{"managed": true, "suggested_name": "metadata", "base_ref": "head"},
	})
	var ref ConversationRef
	decodeResponse(t, created, &ref)

	listResponse, err := http.Get(server.URL + "/v1/conversations")
	if err != nil {
		t.Fatal(err)
	}
	var refs []ConversationRef
	decodeResponse(t, listResponse, &refs)
	if len(refs) != 1 || refs[0].Worktree == nil || refs[0].Worktree.State != "ready" || refs[0].WorkspaceID == "" || refs[0].BaseWorkspaceID != "base_project" {
		t.Fatalf("refs=%+v", refs)
	}
	detailResponse, err := http.Get(server.URL + "/v1/conversations/" + ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	var detail ConversationDetail
	decodeResponse(t, detailResponse, &detail)
	if detail.Worktree == nil || detail.Worktree.State != "ready" || detail.ExecutionPolicy != session.ExecutionPolicyIsolatedWorktree {
		t.Fatalf("detail=%+v", detail)
	}
	if err := os.RemoveAll(ref.WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	activity := fetchActivity(t, server.URL)
	if len(activity.Sessions) != 1 || activity.Sessions[0].State != "workspace_missing" || activity.Sessions[0].Worktree == nil || !activity.Sessions[0].Worktree.NeedsRebind {
		t.Fatalf("activity=%+v", activity.Sessions)
	}
	detailResponse, err = http.Get(server.URL + "/v1/conversations/" + ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	decodeResponse(t, detailResponse, &detail)
	if !detail.NeedsRebind || detail.Worktree == nil || !detail.Worktree.NeedsRebind {
		t.Fatalf("missing detail=%+v", detail)
	}
}

func TestConversationDeleteDoesNotImplicitlyRemoveManagedWorktree(t *testing.T) {
	root := initManagedHTTPRepo(t)
	store := session.NewMemoryStore()
	repo := newFakeConversationRepo()
	manager := managedworktree.New(store, repo)
	server := httptest.NewServer(NewMux(repo, &fakeEventStore{}, nil, MuxOptions{ManagedWorktrees: manager}))
	defer server.Close()
	created := requestJSON(t, server.URL+"/v1/conversations", map[string]any{
		"client_request_id": "create_delete_keep", "workspace_path": root,
		"execution_policy": session.ExecutionPolicyIsolatedWorktree,
		"worktree":         map[string]any{"managed": true, "base_ref": "head"},
	})
	var ref ConversationRef
	decodeResponse(t, created, &ref)
	request, err := http.NewRequest(http.MethodDelete, server.URL+"/v1/conversations/"+ref.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", response.StatusCode)
	}
	if _, err := os.Stat(ref.WorkspacePath); err != nil {
		t.Fatalf("conversation delete removed worktree: %v", err)
	}
	record, err := manager.WorktreeBySessionID(context.Background(), ref.ID)
	if err != nil || record.State != "ready" {
		t.Fatalf("record=%+v err=%v", record, err)
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
